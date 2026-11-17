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
	Memory   string
	Replace  map[string]bool
}

func hasLogin(service string, state object, m member) bool {
	if tokenService(service) || service == "gcloud" {
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

func normalizeArchive(p profileArchive) (profileArchive, error) {
	if p.Format != "commander-profile" || p.Version != 2 || p.Profile.Active != nil {
		return p, errFormat
	}
	if err := validateProfiles(p.Profile); err != nil {
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
	if p.Memory != nil {
		if err := validateMemory([]byte(*p.Memory)); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (a *app) selectedArchive(accounts []member, sessions []session, memory bool) (profileArchive, error) {
	p := profileArchive{Format: "commander-profile", Version: 2, Accounts: accounts, Profile: profiles{Version: 1, Profiles: map[string][]member{}, Scopes: map[string][]string{}}, Logins: map[string]object{}}
	for _, s := range sessions {
		p.Profile.Profiles[s.Name] = s.Accounts
		p.Profile.Scopes[s.Name] = s.Repositories
	}
	grouped := map[string][]member{}
	for _, m := range accounts {
		grouped[m.Service] = append(grouped[m.Service], m)
	}
	store := newStorage(a.root)
	for service, members := range grouped {
		state, err := store.state(service)
		if err != nil {
			return p, err
		}
		p.Logins[service], err = selectLogins(service, state, members)
		if err != nil {
			return p, err
		}
	}
	if memory {
		path := filepath.Join(a.root, "MEMORY.md")
		if err := noSymlinks(a.root, path); err != nil {
			return p, err
		}
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return p, err
		}
		v := string(b)
		p.Memory = &v
	}
	return normalizeArchive(p)
}

func mergeMemory(current, incoming string) (string, error) {
	merged := normalizeMemory([]byte(current + "\n" + incoming))
	return string(merged), validateMemory(merged)
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
		current, err := store.state(service)
		if err != nil {
			return err
		}
		incoming, err := selectLogins(service, p.Logins[service], members)
		if err != nil {
			return err
		}
		for _, m := range members {
			old, oldErr := selectLogins(service, current, []member{m})
			next, err := selectLogins(service, incoming, []member{m})
			if err != nil {
				return err
			}
			if hasLogin(service, current, m) && (oldErr != nil || !reflect.DeepEqual(old, next)) && !selection.Replace[m.id()] {
				return errConflict
			}
		}
		merged := mergeCredentialState(service, current, incoming)
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
		for _, name := range selection.Sessions {
			members, ok := p.Profile.Profiles[name]
			if !ok {
				return errors.New("session was not in the archive")
			}
			s := session{Name: name, Accounts: members, Repositories: p.Profile.Scopes[name]}
			for _, old := range existing {
				if strings.EqualFold(old.Name, name) {
					if !selection.Replace["session:"+name] {
						return errors.New("session " + old.Name + " already exists")
					}
					s.ID, s.Name = old.ID, old.Name
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
	if selection.Memory != "" && selection.Memory != "keep" {
		if p.Memory == nil {
			return errors.New("archive has no memory")
		}
		path := filepath.Join(a.root, "MEMORY.md")
		if err := noSymlinks(a.root, path); err != nil {
			return err
		}
		before, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		store.originals["MEMORY.md"] = before
		next := *p.Memory
		switch selection.Memory {
		case "replace":
		case "merge":
			next, err = mergeMemory(string(before), next)
			if err != nil {
				return err
			}
		default:
			return errors.New("memory mode must be keep, merge or replace")
		}
		files["MEMORY.md"] = []byte(next)
	}
	return store.publish(files)
}

func (a *app) transfer(args []string) error {
	if !a.terminal() {
		return errors.New("open a terminal to select archive contents and enter its password")
	}
	if len(args) < 1 || len(args) > 2 || (args[0] == "import" && len(args) != 2) {
		return errors.New("usage: commander export [FILE.zip] or commander import FILE.zip")
	}
	if err := a.ensure(); err != nil {
		return err
	}
	path := strings.Join(args[1:], "")
	run := a.exportArchive
	if args[0] == "import" {
		run = a.importArchive
	}
	message, err := run(path)
	a.clear()
	if err == nil {
		fmt.Fprintln(a.out, message)
	}
	return err
}

func (a *app) exportArchive(path string) (string, error) {
	logins, _ := a.logins()
	all, err := a.sessions()
	if err != nil {
		return "", err
	}
	items := []choice{}
	for _, r := range logins {
		name, context := r.display()
		items = append(items, choice{group: serviceName(r.Service), label: name, detail: context})
	}
	for _, s := range all {
		items = append(items, choice{group: "sessions", label: s.Name, detail: s.summary()})
	}
	items = append(items, choice{group: "memory", label: "MEMORY.md", detail: "personal preferences"})
	items, err = a.checklist("export", items)
	if err != nil {
		return "", err
	}
	accounts, sessions := []member{}, []session{}
	for i, item := range items[:len(logins)] {
		if item.on {
			accounts = append(accounts, logins[i].member)
		}
	}
	for i, item := range items[len(logins) : len(logins)+len(all)] {
		if item.on {
			sessions = append(sessions, all[i])
		}
	}
	memory := items[len(items)-1].on
	if len(accounts) == 0 && len(sessions) == 0 && !memory {
		return "", errors.New("nothing selected")
	}
	if path == "" {
		if path, err = a.prompt("archive", "commander.zip"); err != nil {
			return "", err
		}
	}
	if path, err = archivePath(path); err != nil {
		return "", err
	}
	if filepath.Ext(path) != ".zip" {
		return "", errors.New("use a .zip archive")
	}
	password, err := a.password("archive password", true)
	if err != nil {
		return "", err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return "", err
	}
	payload, err := a.selectedArchive(accounts, sessions, memory)
	unlock()
	if err != nil {
		return "", err
	}
	if err = writeArchive(path, payload, password); err != nil {
		return "", err
	}
	return fmt.Sprintf("exported %s to %s", contents(len(accounts), len(sessions), memory), path), nil
}

func contents(accounts, sessions int, memory bool) string {
	parts := []string{count(accounts, "account"), count(sessions, "session")}
	if memory {
		parts = append(parts, "memory")
	}
	return strings.Join(parts, ", ")
}

func (a *app) importArchive(path string) (string, error) {
	var err error
	if path == "" {
		if path, err = a.prompt("archive", ""); err != nil {
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
	names := []string{}
	for name := range p.Profile.Profiles {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	items := []choice{}
	for _, m := range p.Accounts {
		name, context := m.display()
		items = append(items, choice{group: serviceName(m.Service), label: name, detail: context, on: true})
	}
	for _, name := range names {
		items = append(items, choice{group: "sessions", label: name, detail: session{Accounts: p.Profile.Profiles[name]}.summary(), on: true})
	}
	if p.Memory != nil {
		items = append(items, choice{group: "memory", label: "merge MEMORY.md", radio: "memory"}, choice{group: "memory", label: "replace MEMORY.md", radio: "memory"})
	}
	if items, err = a.checklist("import", items); err != nil {
		return "", err
	}
	selection := importSelection{Memory: "keep", Replace: map[string]bool{}}
	store := newStorage(a.root)
	for i, m := range p.Accounts {
		if !items[i].on {
			continue
		}
		current, err := store.state(m.Service)
		if err != nil {
			return "", err
		}
		old, exists := selectLogins(m.Service, current, []member{m})
		next, err := selectLogins(m.Service, p.Logins[m.Service], []member{m})
		if err != nil {
			return "", err
		}
		if hasLogin(m.Service, current, m) && (exists != nil || !reflect.DeepEqual(old, next)) {
			if !a.ask("replace the existing login " + m.label() + "?") {
				continue
			}
			selection.Replace[m.id()] = true
		}
		selection.Accounts = append(selection.Accounts, m)
	}
	existing, err := a.sessions()
	if err != nil {
		return "", err
	}
	for i, name := range names {
		if !items[len(p.Accounts)+i].on {
			continue
		}
		for _, s := range existing {
			if strings.EqualFold(s.Name, name) {
				if !a.ask("replace session " + s.Name + "?") {
					name = ""
					break
				}
				selection.Replace["session:"+name] = true
			}
		}
		if name != "" {
			selection.Sessions = append(selection.Sessions, name)
		}
	}
	for _, item := range items[len(p.Accounts)+len(names):] {
		if item.on {
			selection.Memory, _, _ = strings.Cut(item.label, " ")
		}
	}
	if len(selection.Accounts) == 0 && len(selection.Sessions) == 0 && selection.Memory == "keep" {
		return "", errors.New("nothing selected")
	}
	unlock, err := a.lock("native")
	if err != nil {
		return "", err
	}
	defer unlock()
	for _, name := range []string{"sessions", "memory"} {
		release, err := a.waitLock(name)
		if err != nil {
			return "", err
		}
		defer release()
	}
	if err = a.importSelected(p, selection); err != nil {
		return "", err
	}
	message := "imported " + contents(len(selection.Accounts), len(selection.Sessions), selection.Memory != "keep")
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
