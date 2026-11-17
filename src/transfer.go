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

type importSelection struct {
	Accounts []member
	Presets  []string
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
	if p.Format != "commander-profile" || (p.Version != 1 && p.Version != 2) {
		return p, errFormat
	}
	if err := validateProfiles(p.Profile); err != nil {
		return p, err
	}
	if p.Version == 1 {
		if len(p.Profile.Profiles) != 1 || p.Profile.Active == nil || p.Memory != nil || len(p.Accounts) > 0 {
			return p, errFormat
		}
		p.Accounts = p.Profile.Profiles[*p.Profile.Active]
	}
	if p.Accounts == nil {
		p.Accounts = []member{}
	}
	if err := validateProfiles(profiles{Version: 1, Profiles: map[string][]member{"archive": p.Accounts}}); err != nil {
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
	p.Version = 2
	p.Profile.Active = nil
	return p, nil
}

func (a *app) selectedArchive(accounts []member, presets []string, memory bool) (profileArchive, error) {
	p := profileArchive{Format: "commander-profile", Version: 2, Accounts: accounts, Profile: profiles{Version: 1, Profiles: map[string][]member{}, Scopes: map[string][]string{}}, Logins: map[string]object{}}
	book, err := a.readProfiles()
	if err != nil {
		return p, err
	}
	for _, name := range presets {
		members, ok := book.Profiles[name]
		if !ok {
			return p, errors.New("saved selection changed")
		}
		p.Profile.Profiles[name] = members
		p.Profile.Scopes[name] = book.Scopes[name]
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
	seen := map[string]bool{}
	lines := []string{}
	for _, text := range []string{current, incoming} {
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			key := strings.TrimSpace(line)
			if !seen[key] {
				seen[key] = true
				lines = append(lines, line)
			}
		}
	}
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	return out, validateMemory([]byte(out))
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
	if len(selection.Presets) > 0 {
		book, err := a.readProfiles()
		if err != nil {
			return err
		}
		if book.Scopes == nil {
			book.Scopes = map[string][]string{}
		}
		for _, name := range selection.Presets {
			members, ok := p.Profile.Profiles[name]
			if !ok {
				return errors.New("selection was not in the archive")
			}
			if _, exists := book.Profiles[name]; exists && !selection.Replace["preset:"+name] {
				return errors.New("saved selection already exists")
			}
			book.Profiles[name] = members
			book.Scopes[name] = p.Profile.Scopes[name]
		}
		path := filepath.Join(a.root, "profiles.json")
		before, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		store.originals["profiles.json"] = before
		files["profiles.json"] = encode(book)
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

func (a *app) choosePresets(book profiles) ([]string, error) {
	names := []string{}
	for name := range book.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	selected := map[string]bool{}
	for {
		options := []string{}
		for _, name := range names {
			mark := "○ "
			if selected[name] {
				mark = "✓ "
			}
			options = append(options, mark+name)
		}
		options = append(options, "continue")
		i, err := a.choose("saved selections", "optional: include reusable account and repository selections.", options)
		if err != nil {
			return nil, err
		}
		if i == len(names) {
			result := []string{}
			for _, name := range names {
				if selected[name] {
					result = append(result, name)
				}
			}
			return result, nil
		}
		selected[names[i]] = !selected[names[i]]
	}
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
	path := ""
	if len(args) == 2 {
		path = args[1]
	}
	if args[0] == "export" {
		all, err := a.savedMembers()
		if err != nil {
			return err
		}
		initial := []member{}
		if a.session != nil {
			initial = a.session.Accounts
		}
		accounts, err := a.selectMembers("export accounts", all, initial, true)
		if err != nil {
			return err
		}
		book, err := a.readProfiles()
		if err != nil {
			return err
		}
		presets, err := a.choosePresets(book)
		if err != nil {
			return err
		}
		choice, err := a.choose("export memory", "MEMORY.md contains personal preferences. Include it only when wanted.", []string{"leave memory out", "include MEMORY.md"})
		if err != nil {
			return err
		}
		if len(accounts) == 0 && len(presets) == 0 && choice == 0 {
			return errors.New("nothing selected")
		}
		if path == "" {
			path, err = a.input("archive path", "commander.zip")
			if err != nil {
				return err
			}
		}
		path, err = archivePath(path)
		if err != nil {
			return err
		}
		if filepath.Ext(path) != ".zip" {
			return errors.New("use a .zip archive")
		}
		password, err := a.password("archive password", true)
		if err != nil {
			return err
		}
		unlock, err := a.lock("native")
		if err != nil {
			return err
		}
		payload, err := a.selectedArchive(accounts, presets, choice == 1)
		unlock()
		if err != nil {
			return err
		}
		if err = writeArchive(path, payload, password); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "exported %d accounts, %d selections, memory=%t: %s\n", len(accounts), len(presets), choice == 1, path)
		return nil
	}
	path, err := archivePath(path)
	if err != nil {
		return err
	}
	password, err := a.password("archive password", false)
	if err != nil {
		return err
	}
	p, err := readArchive(path, password)
	if err != nil {
		return err
	}
	p, err = normalizeArchive(p)
	if err != nil {
		return err
	}
	accounts, err := a.selectMembers("import accounts", p.Accounts, nil, true)
	if err != nil {
		return err
	}
	presets, err := a.choosePresets(p.Profile)
	if err != nil {
		return err
	}
	selection := importSelection{Accounts: accounts, Presets: presets, Memory: "keep", Replace: map[string]bool{}}
	if p.Memory != nil {
		current, _ := os.ReadFile(filepath.Join(a.root, "MEMORY.md"))
		i, err := a.choose("import memory", "Current memory:\n"+string(current)+"\nImported memory:\n"+*p.Memory, []string{"keep current memory", "merge unique statements", "replace with imported memory"})
		if err != nil {
			return err
		}
		selection.Memory = []string{"keep", "merge", "replace"}[i]
	}
	store := newStorage(a.root)
	kept := []member{}
	for _, m := range accounts {
		current, err := store.state(m.Service)
		if err != nil {
			return err
		}
		old, exists := selectLogins(m.Service, current, []member{m})
		next, err := selectLogins(m.Service, p.Logins[m.Service], []member{m})
		if err != nil {
			return err
		}
		if hasLogin(m.Service, current, m) && (exists != nil || !reflect.DeepEqual(old, next)) {
			i, err := a.choose("account conflict", m.label()+" already exists with different login data.", []string{"keep existing account", "replace selected account"})
			if err != nil {
				return err
			}
			if i == 0 {
				continue
			}
			selection.Replace[m.id()] = true
		}
		kept = append(kept, m)
	}
	selection.Accounts = kept
	book, err := a.readProfiles()
	if err != nil {
		return err
	}
	selection.Presets = nil
	for _, name := range presets {
		if _, exists := book.Profiles[name]; exists {
			i, err := a.choose("selection conflict", name+" already exists.", []string{"keep existing selection", "replace saved selection"})
			if err != nil {
				return err
			}
			if i == 0 {
				continue
			}
			selection.Replace["preset:"+name] = true
		}
		selection.Presets = append(selection.Presets, name)
	}
	if len(selection.Accounts) == 0 && len(selection.Presets) == 0 && selection.Memory == "keep" {
		return errors.New("nothing selected")
	}
	if !a.confirm(fmt.Sprintf("import %d accounts, %d selections, memory: %s? Running sessions keep their selections.", len(selection.Accounts), len(selection.Presets), selection.Memory)) {
		return errCancelled
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	profileUnlock, err := a.lock("profiles")
	if err != nil {
		return err
	}
	defer profileUnlock()
	memoryUnlock, err := a.lock("memory")
	if err != nil {
		return err
	}
	defer memoryUnlock()
	if err = a.importSelected(p, selection); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "selected archive contents imported; running session selections unchanged")
	if len(selection.Accounts) > 0 {
		admin := *a
		admin.session = nil
		if a.session != nil {
			admin.run = admin.nativeRun
		}
		if _, err = admin.run("git", []string{"--version"}, true, nil); err != nil {
			return fmt.Errorf("import saved; tool preparation needs attention: %w", err)
		}
		rows := admin.resolve(selection.Accounts)
		admin.printAccounts(rows)
		for _, row := range rows {
			if row.State != "Saved" && row.State != "Active" {
				return errors.New("import saved; reconnect accounts marked for attention in Commander Accounts")
			}
		}
	}
	return nil
}
