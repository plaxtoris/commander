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

// Replace holds the account IDs whose existing login may be replaced.
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
	if p.Memory != nil {
		if err := validateMemory([]byte(*p.Memory)); err != nil {
			return p, err
		}
	}
	if err := p.CodingConfigs.validate(); err != nil {
		return p, err
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

func (a *app) archiveSessions(sessions []session, memory *string) (profileArchive, error) {
	p := profileArchive{
		Format: "commander-profile", Version: 2, Accounts: []member{}, Memory: memory,
		Profile: selections{Version: 1, Profiles: map[string][]member{}, Scopes: map[string][]string{}},
		Logins:  map[string]object{},
	}
	grouped := map[string][]member{}
	seen := map[string]bool{}
	for _, s := range sessions {
		p.Profile.Profiles[s.Name], p.Profile.Scopes[s.Name] = s.Accounts, s.Repositories
		for _, m := range s.Accounts {
			if !seen[m.id()] {
				seen[m.id()] = true
				p.Accounts = append(p.Accounts, m)
				grouped[m.Service] = append(grouped[m.Service], m)
			}
		}
	}
	store := newStorage(a.root)
	configs, err := store.codingConfigs()
	if err != nil {
		return p, err
	}
	p.CodingConfigs = configs
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
		encoded, err := encodeLogins(service, merged)
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
	if len(p.CodingConfigs) > 0 {
		unlock, err := a.waitLock("coding-config")
		if err != nil {
			return err
		}
		defer unlock()
		configs, err := store.codingConfigs()
		if err != nil {
			return err
		}
		for agent, data := range p.CodingConfigs {
			configs[agent] = data
		}
		files[filepath.Join(".secrets", codingConfigFile)] = encode(configs)
	}
	if p.Memory != nil {
		unlock, err := a.waitLock("memory")
		if err != nil {
			return err
		}
		defer unlock()
		path := filepath.Join(a.root, "MEMORY.md")
		if err = noSymlinks(a.root, path); err != nil {
			return err
		}
		if store.originals["MEMORY.md"], err = os.ReadFile(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		files["MEMORY.md"] = []byte(*p.Memory)
	}
	existing, err := a.sessions()
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	var retired []session
	for _, name := range selection.Sessions {
		members, ok := p.Profile.Profiles[name]
		if !ok || taken[strings.ToLower(name)] {
			return errFormat
		}
		taken[strings.ToLower(name)] = true
		id, err := newSessionID()
		if err != nil {
			return err
		}
		s := session{ID: id, Name: name, Accounts: members, Repositories: p.Profile.Scopes[name]}
		if err = s.validate(); err != nil {
			return err
		}
		for _, old := range existing {
			if strings.EqualFold(old.Name, name) {
				unlock, err := a.sessionLock(old.ID, false, false)
				if err != nil {
					return fmt.Errorf("session %s is in use; close it and retry import", old.Name)
				}
				defer unlock()
				retired = append(retired, old)
			}
		}
		files[filepath.Join(".sessions", s.ID, "session.json")] = encode(s)
	}
	var remove []string
	for _, old := range retired {
		scoped := *a
		scoped.session = &old
		if err = scoped.removeSessionContainers(); err != nil {
			return err
		}
		if err = scoped.stopBridge(); err != nil {
			return err
		}
		remove = append(remove, old.ID)
	}
	return store.publish(files, remove...)
}

func (a *app) transfer(args []string) error {
	if len(args) > 2 {
		return errors.New("usage: commander export [SESSION] or commander import [FILE.zip]")
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
		path := ""
		if len(args) == 2 {
			path = args[1]
		}
		message, err = a.importArchive(path)
	} else if len(args) == 1 {
		message, err = a.chooseExport()
	} else {
		var s session
		if s, err = a.findSession(args[1]); err == nil {
			message, err = a.exportArchive(s.Name+".zip", []string{s.ID}, true)
		}
	}
	a.clear()
	if err == nil {
		fmt.Fprintln(a.out, message)
	}
	return err
}

func (a *app) chooseExport() (string, error) {
	sessions, err := a.sessions()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(sessions)+1)
	for _, s := range sessions {
		names = append(names, s.Name)
	}
	memoryPath := filepath.Join(a.root, "MEMORY.md")
	if err = noSymlinks(a.root, memoryPath); err != nil {
		return "", err
	}
	_, err = os.Stat(memoryPath)
	hasMemory := err == nil
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if hasMemory {
		names = append(names, "MEMORY.md")
	}
	configs, err := newStorage(a.root).codingConfigs()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		if len(configs) > 0 {
			return a.exportArchive("commander.zip", nil, false)
		}
		return "", errors.New("no sessions, memory or agent settings to export")
	}
	selected := make([]bool, len(names))
	for i := range selected {
		selected[i] = true
	}
	at := 0
	notice, tone := "", ""
	if len(configs) > 0 {
		notice, tone = "includes shared agent settings", "2"
	}
	for {
		rows := make([]row, len(names))
		for i, name := range names {
			box := "[ ] "
			if selected[i] {
				box = "[x] "
			}
			rows[i] = row{pick: true, line: uiLine{{box + name, ""}}}
		}
		key, i, err := a.list(view{title: "export", keys: "[space] select  [a] all / none  [enter] export  [esc] cancel", notice: notice, tone: tone, rows: rows}, at)
		if err != nil {
			return "", err
		}
		at = i
		switch key {
		case "esc", "q":
			return "", errCancelled
		case "space":
			selected[i] = !selected[i]
		case "a":
			all := true
			for _, on := range selected {
				all = all && on
			}
			for i := range selected {
				selected[i] = !all
			}
		case "enter":
			var ids []string
			for i, s := range sessions {
				if selected[i] {
					ids = append(ids, s.ID)
				}
			}
			memory := hasMemory && selected[len(sessions)]
			if len(ids) > 0 || memory || len(configs) > 0 {
				return a.exportArchive("commander.zip", ids, memory)
			}
			notice = "select at least one item"
		}
	}
}

func (a *app) exportArchive(name string, ids []string, memory bool) (string, error) {
	path, err := filepath.Abs(name)
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
	payload, err := a.exportSnapshot(ids, memory)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 && payload.Memory == nil && len(payload.CodingConfigs) == 0 {
		return "", errors.New("no sessions, memory or agent settings to export")
	}
	if err = writeArchive(path, payload, password, replace); err != nil {
		return "", err
	}
	message := fmt.Sprintf("exported %s, %s", count(len(ids), "session"), count(len(payload.Accounts), "account"))
	if payload.Memory != nil {
		message += ", memory"
	}
	if len(payload.CodingConfigs) > 0 {
		message += ", agent settings"
	}
	return message + " to " + path, nil
}

// Share the native lock with running tools and serialize the short snapshot
// with credential refreshes that write more than one file.
func (a *app) exportSnapshot(ids []string, includeMemory bool) (profileArchive, error) {
	unlock, err := a.installationLock("native", true, false)
	if err != nil {
		return profileArchive{}, fmt.Errorf("saved logins are being updated; retry export: %w", err)
	}
	defer unlock()
	release, err := a.credentialLock("write")
	if err != nil {
		return profileArchive{}, err
	}
	defer release()
	sessions := make([]session, 0, len(ids))
	releaseSessions, err := a.waitLock("sessions")
	if err != nil {
		return profileArchive{}, err
	}
	defer releaseSessions()
	for _, id := range ids {
		s, err := a.readSession(id)
		if err != nil {
			return profileArchive{}, err
		}
		sessions = append(sessions, s)
	}
	var memory *string
	if includeMemory {
		path := filepath.Join(a.root, "MEMORY.md")
		if err = noSymlinks(a.root, path); err != nil {
			return profileArchive{}, err
		}
		data, err := os.ReadFile(path)
		if err == nil {
			text := string(data)
			memory = &text
		} else if !os.IsNotExist(err) {
			return profileArchive{}, err
		}
	}
	return a.archiveSessions(sessions, memory)
}

func (a *app) importChoices(p profileArchive, confirm func(string) bool) (importSelection, error) {
	selection := importSelection{Replace: map[string]bool{}}
	names := []string{}
	for name := range p.Profile.Profiles {
		names = append(names, name)
	}
	if len(names) == 0 && p.Memory == nil && len(p.CodingConfigs) == 0 {
		return selection, errors.New("the archive holds no session, memory or agent settings")
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	archived := map[string]member{}
	for _, m := range p.Accounts {
		archived[m.id()] = m
	}
	store, chosen := newStorage(a.root), map[string]bool{}
	for _, name := range names {
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
	return selection, nil
}

func (a *app) chooseImportPath() (string, error) {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return "", err
	}
	var names, labels []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && !strings.HasPrefix(name, ".") && strings.EqualFold(filepath.Ext(name), ".zip") {
			names = append(names, name)
			labels = append(labels, clean(name))
		}
	}
	if len(names) > 0 {
		i, err := a.choose("import", append(labels, "+ enter a path"), 0)
		if err != nil {
			return "", err
		}
		if i < len(names) {
			return filepath.Join(a.root, names[i]), nil
		}
	}
	return a.prompt("archive", "", completePath)
}

func (a *app) importArchive(path string) (string, error) {
	var err error
	if path == "" {
		if path, err = a.chooseImportPath(); err != nil {
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
	if p.Memory != nil {
		message += ", memory"
	}
	if len(p.CodingConfigs) > 0 {
		message += ", agent settings"
	}
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
