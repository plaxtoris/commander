package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Sessions contain selections, never credentials. Each command reads one snapshot.
type session struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Accounts     []member `json:"accounts"`
	Repositories []string `json:"repositories"`
}

var sessionID = regexp.MustCompile(`^[a-f0-9]{24}$`)
var repositoryScope = regexp.MustCompile(`^[A-Za-z0-9_.-]+/(?:[A-Za-z0-9_.-]+|\*)$`)

func validRepositoryScope(value string) bool {
	if !repositoryScope.MatchString(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func repositoryInput(repos []string) string {
	if len(repos) == 0 {
		return "-"
	}
	return strings.Join(repos, ",")
}
func parseRepositories(value string) []string {
	result := []string{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" && part != "-" {
			result = append(result, part)
		}
	}
	return result
}
func (a *app) parseAccounts(value string) ([]member, error) {
	selected := []member{}
	if value == "-" || value == "none" {
		return selected, nil
	}
	all, err := a.savedMembers()
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(value, ",") {
		service, selector, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, errors.New("accounts use service=selector, separated by commas; - selects none")
		}
		matches := []member{}
		for _, m := range all {
			_, user := m.github()
			if m.Service == service && (m.selector() == selector || m.Account == selector || user == selector) {
				matches = append(matches, m)
			}
		}
		if len(matches) != 1 {
			return nil, errors.New("account selector is missing or ambiguous: " + part)
		}
		selected = append(selected, matches[0])
	}
	return selected, nil
}

func (a *app) sessionPath(id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", errors.New("invalid session ID")
	}
	p := filepath.Join(a.root, ".sessions", id)
	return p, noSymlinks(a.root, p)
}

func (a *app) readSession(id string) (session, error) {
	var s session
	p, err := a.sessionPath(id)
	if err != nil {
		return s, err
	}
	if err = noSymlinks(a.root, filepath.Join(p, "session.json")); err != nil {
		return s, err
	}
	b, err := os.ReadFile(filepath.Join(p, "session.json"))
	if err != nil {
		return s, errors.New("session is missing; run commander session new")
	}
	if json.Unmarshal(b, &s) != nil || s.ID != id || !validName(s.Name) {
		return s, errors.New("invalid session")
	}
	if err := validateProfiles(profiles{Version: 1, Profiles: map[string][]member{s.Name: s.Accounts}}); err != nil {
		return s, err
	}
	for _, r := range s.Repositories {
		if !validRepositoryScope(r) {
			return s, errors.New("invalid repository selection")
		}
	}
	return s, nil
}

func (a *app) saveSession(s session) error {
	if s.Accounts == nil {
		s.Accounts = []member{}
	}
	p, err := a.sessionPath(s.ID)
	if err != nil {
		return err
	}
	if !validName(s.Name) {
		return errors.New("use a visible session name")
	}
	if err := validateProfiles(profiles{Version: 1, Profiles: map[string][]member{s.Name: s.Accounts}}); err != nil {
		return err
	}
	for _, r := range s.Repositories {
		if !validRepositoryScope(r) {
			return errors.New("repositories use owner/name or owner/*")
		}
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(p, "session.json"), encode(s), 0600)
}

func (a *app) newSession(name string, accounts []member, repos []string) (session, error) {
	if accounts == nil {
		accounts = []member{}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return session{}, err
	}
	s := session{ID: hex.EncodeToString(b), Name: name, Accounts: accounts, Repositories: repos}
	return s, a.saveSession(s)
}

func (s session) allows(repository string) bool {
	repository = strings.ToLower(strings.TrimSuffix(repository, ".git"))
	if !validRepositoryScope(repository) || strings.HasSuffix(repository, "/*") {
		return false
	}
	for _, r := range s.Repositories {
		r = strings.ToLower(r)
		if r == repository || strings.HasSuffix(r, "/*") && strings.HasPrefix(repository, strings.TrimSuffix(r, "*")) {
			return true
		}
	}
	return false
}

func (a *app) sessionCommand(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		entries, err := os.ReadDir(filepath.Join(a.root, ".sessions"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !sessionID.MatchString(entry.Name()) {
				continue
			}
			s, err := a.readSession(entry.Name())
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s  %s  %d accounts  %s\n", s.ID, s.Name, len(s.Accounts), strings.Join(s.Repositories, ", "))
		}
		return nil
	}
	switch args[0] {
	case "new":
		if len(args) < 2 {
			return errors.New("usage: commander session new NAME [PRESET] [--repos owner/name,owner/*]")
		}
		name, preset := args[1], ""
		accountSelection := ""
		reposSet := false
		repos := []string{}
		for i := 2; i < len(args); i++ {
			if args[i] == "--repos" && i+1 < len(args) {
				i++
				repos = parseRepositories(args[i])
				reposSet = true
			} else if args[i] == "--accounts" && i+1 < len(args) {
				i++
				accountSelection = args[i]
			} else if strings.HasPrefix(args[i], "-") {
				return errors.New("unknown session option")
			} else if preset == "" {
				preset = args[i]
			} else {
				return errors.New("invalid session arguments")
			}
		}
		accounts := []member{}
		if preset != "" {
			_, selected, err := a.profile(preset)
			if err != nil {
				return err
			}
			accounts = selected
			if !reposSet {
				book, err := a.readProfiles()
				if err != nil {
					return err
				}
				repos = book.Scopes[preset]
			}
		}
		if accountSelection != "" {
			var err error
			accounts, err = a.parseAccounts(accountSelection)
			if err != nil {
				return err
			}
		}
		if a.terminal() {
			all, err := a.savedMembers()
			if err != nil {
				return err
			}
			accounts, err = a.selectMembers("session accounts", all, accounts, true)
			if err != nil {
				return err
			}
			value, err := a.input("repositories (owner/name or owner/*, comma separated; - for none)", repositoryInput(repos))
			if err != nil {
				return err
			}
			repos = parseRepositories(value)
		}
		s, err := a.newSession(name, accounts, repos)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "session: %s\nstart a shell: %s session shell %s\n", s.ID, quote(filepath.Join(a.root, "commander")), s.ID)
		return nil
	case "shell":
		if len(args) != 2 {
			return errors.New("usage: commander session shell ID")
		}
		s, err := a.readSession(args[1])
		if err != nil {
			return err
		}
		a.session = &s
		unlock, err := a.sessionLock(s.ID, true, true)
		if err != nil {
			return err
		}
		defer unlock()
		if err = a.sessionWrappers(s); err != nil {
			return err
		}
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
		cmd := exec.Command(shell)
		cmd.Env = a.environment("shell", nil)
		cmd.Stdin = a.in
		cmd.Stdout = a.out
		cmd.Stderr = a.errOut
		return cmd.Run()
	case "edit":
		if len(args) < 2 {
			return errors.New("usage: commander session edit ID")
		}
		s, err := a.readSession(args[1])
		if err != nil {
			return err
		}
		if len(args) > 2 {
			for i := 2; i < len(args); i += 2 {
				if i+1 >= len(args) {
					return errors.New("selection option needs a value")
				}
				switch args[i] {
				case "--accounts":
					s.Accounts, err = a.parseAccounts(args[i+1])
					if err != nil {
						return err
					}
				case "--repos":
					s.Repositories = parseRepositories(args[i+1])
				default:
					return errors.New("edit accepts --accounts and --repos")
				}
			}
			return a.saveSession(s)
		}
		all, err := a.savedMembers()
		if err != nil {
			return err
		}
		s.Accounts, err = a.selectMembers("session accounts", all, s.Accounts, true)
		if err != nil {
			return err
		}
		value, err := a.input("repositories (- for none)", repositoryInput(s.Repositories))
		if err != nil {
			return err
		}
		s.Repositories = parseRepositories(value)
		return a.saveSession(s)
	case "close":
		if len(args) != 2 {
			return errors.New("usage: commander session close ID")
		}
		p, err := a.sessionPath(args[1])
		if err != nil {
			return err
		}
		unlock, err := a.sessionLock(args[1], false, false)
		if err != nil {
			return errors.New("session still has active commands")
		}
		defer unlock()
		if entries, _ := os.ReadDir(filepath.Join(p, "tasks")); len(entries) > 0 {
			return errors.New("finish the session tasks before closing it")
		}
		s, err := a.readSession(args[1])
		if err != nil {
			return err
		}
		scoped := *a
		scoped.session = &s
		if err = scoped.removeSessionContainers(); err != nil {
			return err
		}
		if err = scoped.stopBridge(); err != nil {
			return err
		}
		return os.RemoveAll(p)
	default:
		return errors.New("session commands: new, list, shell, edit, close")
	}
}

func (a *app) sessionLock(id string, shared, wait bool) (func(), error) {
	p, err := a.sessionPath(id)
	if err != nil {
		return nil, err
	}
	unlock, err := fileLock(filepath.Join(p, ".lock"), shared, wait)
	if err != nil {
		return nil, err
	}
	if _, err = a.readSession(id); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func fileLock(path string, shared, wait bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	if !wait {
		mode |= syscall.LOCK_NB
	}
	if err = syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (a *app) savedMembers() ([]member, error) {
	admin := *a
	admin.session = nil
	if a.session != nil {
		admin.run = admin.nativeRun
	}
	p, err := a.readProfiles()
	if err != nil {
		return nil, err
	}
	all := map[string]member{}
	for _, members := range p.Profiles {
		for _, m := range members {
			all[m.id()] = m
		}
	}
	// Include connected accounts which have not been added to a preset.
	for _, service := range services {
		if !tokenService(service) && !a.toolUsed(serviceTools[service]) {
			continue
		}
		rows, err := admin.accounts(service)
		if err != nil {
			continue
		}
		for _, r := range rows {
			all[r.id()] = r.member
		}
	}
	result := []member{}
	for _, m := range all {
		result = append(result, m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].label() < result[j].label() })
	return result, nil
}

func (a *app) selectMembers(title string, all, initial []member, allowEmpty bool) ([]member, error) {
	selected := map[string]bool{}
	for _, m := range initial {
		selected[m.id()] = true
	}
	filter := ""
	for {
		visible := []member{}
		options := []string{}
		for _, m := range all {
			if !strings.Contains(strings.ToLower(m.label()), strings.ToLower(filter)) {
				continue
			}
			visible = append(visible, m)
			mark := "○ "
			if selected[m.id()] {
				mark = "✓ "
			}
			options = append(options, mark+m.label())
		}
		options = append(options, "continue", "select all shown", "clear all", "search", "cancel")
		i, err := a.choose(title, "select accounts individually. enter toggles the selected row.", options)
		if err != nil {
			return nil, err
		}
		if i < len(visible) {
			m := visible[i]
			selected[m.id()] = !selected[m.id()]
			continue
		}
		switch i - len(visible) {
		case 0:
			result := []member{}
			for _, m := range all {
				if selected[m.id()] {
					result = append(result, m)
				}
			}
			if len(result) > 0 || allowEmpty {
				return result, nil
			}
		case 1:
			for _, m := range visible {
				selected[m.id()] = true
			}
		case 2:
			selected = map[string]bool{}
		case 3:
			filter, err = a.input("search", filter)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errCancelled
		}
	}
}

func (a *app) sessionWrappers(s session) error {
	p, err := a.sessionPath(s.ID)
	if err != nil {
		return err
	}
	bin := filepath.Join(p, "bin")
	if err = os.MkdirAll(bin, 0700); err != nil {
		return err
	}
	for _, tool := range append(append([]string{}, managedTools...), "dockerhub", "confluence") {
		body := "#!/bin/sh\nexec " + quote(filepath.Join(a.root, "commander")) + " --session " + quote(s.ID) + " exec -- " + quote(tool) + " \"$@\"\n"
		if err = atomicWrite(filepath.Join(bin, tool), []byte(body), 0700); err != nil {
			return err
		}
	}
	return nil
}
