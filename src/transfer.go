package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// An import selection names archive accounts and sessions. Replace holds
// account IDs and "session:NAME" keys whose existing data may be replaced.
type importSelection struct {
	Accounts []member
	Sessions []string
	Replace  map[string]bool
}

func hasLogin(service string, state object, m member) bool {
	if codingService(service) || tokenService(service) || service == "gcloud" {
		_, ok := state[m.selector()]
		return ok
	}
	if service == "github" {
		host, user := m.github()
		entry := obj(state[host])
		_, ok := obj(entry["users"])[user]
		return ok || entry["user"] == user
	}
	var rows []any
	key := "name"
	switch service {
	case "azure":
		rows = list(obj(state["profile"])["subscriptions"])
		key = "id"
	case "office":
		rows = list(state["connections"])
	case "hetzner":
		rows = list(state["contexts"])
	}
	for _, row := range rows {
		if obj(row)[key] == m.selector() {
			return true
		}
	}
	return false
}

// LoginConflict reports whether importing m would replace different saved
// login data.
func loginConflict(service string, current, incoming object, m member) (bool, error) {
	next, err := selectLogins(service, incoming, []member{m})
	if err != nil {
		return false, err
	}
	old, oldErr := selectLogins(service, current, []member{m})
	return hasLogin(service, current, m) && (oldErr != nil || !reflect.DeepEqual(old, next)), nil
}

func normalizeArchive(p profileArchive) (profileArchive, error) {
	if p.Format != "commander-profile" || p.Version != 2 || p.Profile.Active != nil {
		return p, errFormat
	}
	if err := validateSelections(p.Profile); err != nil {
		return p, err
	}
	if p.Accounts == nil {
		p.Accounts = []member{}
	}
	if err := validateMembers(p.Accounts); err != nil {
		return p, err
	}
	grouped := map[string][]member{}
	for _, m := range p.Accounts {
		grouped[m.Service] = append(grouped[m.Service], m)
	}
	if len(grouped) != len(p.Logins) {
		return p, errFormat
	}
	for service, state := range p.Logins {
		selected, err := selectLogins(service, state, grouped[service])
		if err != nil || !reflect.DeepEqual(selected, state) {
			return p, errFormat
		}
	}
	return p, nil
}

// A session archive holds one session with the logins of its accounts.
func (a *app) sessionArchive(s session) (profileArchive, error) {
	p := profileArchive{Format: "commander-profile", Version: 2, Accounts: s.Accounts, Profile: selections{Version: 1, Profiles: map[string][]member{s.Name: s.Accounts}, Scopes: map[string][]string{s.Name: s.Repositories}}, Logins: map[string]object{}}
	grouped := map[string][]member{}
	for _, m := range s.Accounts {
		grouped[m.Service] = append(grouped[m.Service], m)
	}
	store := newStorage(a.root)
	for service, members := range grouped {
		state, err := store.state(service, members...)
		if err != nil {
			return p, err
		}
		for _, m := range members {
			if !hasLogin(service, state, m) {
				return p, errors.New(m.label() + " has no saved login; sign in again or remove it from the session")
			}
		}
		p.Logins[service], err = selectLogins(service, state, members)
		if err != nil {
			return p, err
		}
	}
	return normalizeArchive(p)
}

func (a *app) importSelected(payload profileArchive, selection importSelection) error {
	p, err := normalizeArchive(payload)
	if err != nil {
		return err
	}
	allowed := map[string]member{}
	for _, m := range p.Accounts {
		allowed[m.id()] = m
	}
	for _, m := range selection.Accounts {
		if want, ok := allowed[m.id()]; !ok || want.id() != m.id() || want.Account != m.Account || want.Context != m.Context {
			return errors.New("account was not in the archive")
		}
	}
	store := newStorage(a.root)
	files := map[string][]byte{}
	grouped := map[string][]member{}
	for _, m := range selection.Accounts {
		grouped[m.Service] = append(grouped[m.Service], m)
	}
	for service, members := range grouped {
		current, err := store.state(service, members...)
		if err != nil {
			return err
		}
		incoming, err := selectLogins(service, p.Logins[service], members)
		if err != nil {
			return err
		}
		for _, m := range members {
			conflict, err := loginConflict(service, current, incoming, m)
			if err != nil {
				return err
			}
			if conflict && !selection.Replace[m.id()] {
				return errConflict
			}
			if codingService(service) {
				if !conflict && hasLogin(service, current, m) {
					delete(incoming, m.selector())
					continue
				}
				unlock, err := a.nativeLoginLock(m, false)
				if err != nil {
					return err
				}
				defer unlock()
			}
		}
		merged := incoming
		if !codingService(service) {
			merged = mergeCredentialState(service, current, incoming)
		}
		// A fresh encoder writes the complete merged service state without treating
		// an explicitly selected replacement as an accidental import conflict.
		tmp, err := os.MkdirTemp(a.root, ".import-")
		if err != nil {
			return err
		}
		encoded, err := newStorage(tmp).install(service, merged)
		os.RemoveAll(tmp)
		if err != nil {
			return err
		}
		for name, data := range encoded {
			if _, err = store.read(name); err != nil {
				return err
			}
			files[filepath.Join(".secrets", name)] = data
		}
	}
	if len(selection.Sessions) > 0 {
		existing, err := a.sessions()
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		for _, name := range selection.Sessions {
			members, ok := p.Profile.Profiles[name]
			if !ok {
				return errors.New("session was not in the archive")
			}
			if taken[strings.ToLower(name)] {
				return errors.New("the archive names two sessions " + name)
			}
			taken[strings.ToLower(name)] = true
			s := session{Name: name, Accounts: members, Repositories: p.Profile.Scopes[name]}
			for _, old := range existing {
				if strings.EqualFold(old.Name, name) {
					if !selection.Replace["session:"+name] {
						return errors.New("session " + old.Name + " already exists")
					}
					s.ID, s.Name = old.ID, old.Name
					break
				}
			}
			if s.ID == "" {
				if s.ID, err = newSessionID(); err != nil {
					return err
				}
			}
			if err = s.validate(); err != nil {
				return err
			}
			path := filepath.Join(".sessions", s.ID, "session.json")
			before, err := os.ReadFile(filepath.Join(a.root, path))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			store.originals[path] = before
			files[path] = encode(s)
		}
	}
	return store.publish(files)
}

func (a *app) transfer(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: commander export SESSION or commander import FILE.zip")
	}
	if !a.terminal() {
		return errors.New("open a terminal to enter the archive password")
	}
	if err := a.ensure(); err != nil {
		return err
	}
	var message string
	var err error
	if args[0] == "import" {
		message, err = a.importArchive(args[1])
	} else {
		var s session
		if s, err = a.findSession(args[1]); err == nil {
			message, err = a.exportArchive(s)
		}
	}
	a.clear()
	if err == nil {
		fmt.Fprintln(a.out, message)
	}
	return err
}

// Export writes one session with its logins to SESSION.zip in the current
// folder. Memory and logins outside the session stay local.
func (a *app) exportArchive(s session) (string, error) {
	path, err := filepath.Abs(strings.ReplaceAll(s.Name, string(os.PathSeparator), "_") + ".zip")
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	replace := err == nil
	if replace && !info.Mode().IsRegular() {
		return "", errors.New(path + " exists and is not a file")
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if replace && !a.ask("replace "+filepath.Base(path)+"?") {
		return "", errCancelled
	}
	password, err := a.password("archive password", true)
	if err != nil {
		return "", err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return "", err
	}
	payload, err := a.sessionArchive(s)
	unlock()
	if err != nil {
		return "", err
	}
	if err = writeArchive(path, payload, password, replace); err != nil {
		return "", err
	}
	return fmt.Sprintf("exported %s with %s to %s", s.Name, count(len(s.Accounts), "account"), path), nil
}

// importChoices selects every archive session with the archived logins it
// uses. Existing sessions and different saved logins are replaced only after
// confirm agrees. Logins outside the sessions and memory of older archives
// are ignored.
func (a *app) importChoices(p profileArchive, confirm func(string) bool) (importSelection, error) {
	selection := importSelection{Replace: map[string]bool{}}
	names := []string{}
	for name := range p.Profile.Profiles {
		names = append(names, name)
	}
	if len(names) == 0 {
		return selection, errors.New("the archive holds no session")
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	existing, err := a.sessions()
	if err != nil {
		return selection, err
	}
	archived := map[string]member{}
	for _, m := range p.Accounts {
		archived[m.id()] = m
	}
	store, chosen := newStorage(a.root), map[string]bool{}
	for _, name := range names {
		skip := false
		for _, s := range existing {
			if strings.EqualFold(s.Name, name) {
				skip = !confirm("replace session " + s.Name + "?")
				selection.Replace["session:"+name] = !skip
			}
		}
		if skip {
			continue
		}
		selection.Sessions = append(selection.Sessions, name)
		for _, used := range p.Profile.Profiles[name] {
			m, ok := archived[used.id()]
			if !ok || chosen[m.id()] {
				continue
			}
			chosen[m.id()] = true
			current, err := store.state(m.Service, m)
			if err != nil {
				return selection, err
			}
			conflict, err := loginConflict(m.Service, current, p.Logins[m.Service], m)
			if err != nil {
				return selection, err
			}
			if conflict {
				if !confirm("replace the existing login " + m.label() + "?") {
					continue
				}
				selection.Replace[m.id()] = true
			}
			selection.Accounts = append(selection.Accounts, m)
		}
	}
	if len(selection.Sessions) == 0 {
		return selection, errCancelled
	}
	return selection, nil
}

func (a *app) importArchive(path string) (string, error) {
	var err error
	if path == "" {
		if path, err = a.prompt("archive", "", completePath); err != nil {
			return "", err
		}
	}
	if path, err = archivePath(path); err != nil {
		return "", err
	}
	password, err := a.password("archive password", false)
	if err != nil {
		return "", err
	}
	p, err := readArchive(path, password)
	if err != nil {
		return "", err
	}
	if p, err = normalizeArchive(p); err != nil {
		return "", err
	}
	selection, err := a.importChoices(p, a.ask)
	if err != nil {
		return "", err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return "", err
	}
	release, err := a.waitLock("sessions")
	if err != nil {
		unlock()
		return "", err
	}
	err = a.importSelected(p, selection)
	release()
	unlock()
	// GitHub's status check acquires the native login lock itself.
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf("imported %s, %s", count(len(selection.Sessions), "session"), count(len(selection.Accounts), "account"))
	if len(selection.Accounts) > 0 {
		admin := a.admin()
		if _, err = admin.run("git", []string{"--version"}, true, nil); err != nil {
			return "", fmt.Errorf("%s; tool preparation needs attention: %w", message, err)
		}
		if err = attention(admin.resolve(selection.Accounts)); err != nil {
			return "", fmt.Errorf("%s; %w", message, err)
		}
	}
	return message, nil
}
