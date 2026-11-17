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
)

// Sessions are named account and repository selections without credentials.
// The ID never changes, so renames keep shells, tasks and containers working.
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
	all, _ := a.logins()
	for _, part := range strings.Split(value, ",") {
		service, selector, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, errors.New("accounts use service=selector, separated by commas; - selects none")
		}
		matches := []member{}
		for _, m := range all {
			_, user := m.github()
			if m.Service == service && (m.selector() == selector || m.Account == selector || user == selector) {
				matches = append(matches, m.member)
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
		return s, errors.New("session is missing; see commander session list")
	}
	if json.Unmarshal(b, &s) != nil || s.ID != id {
		return s, errors.New("invalid session")
	}
	return s, s.validate()
}

func (s *session) validate() error {
	if s.Accounts == nil {
		s.Accounts = []member{}
	}
	if s.Repositories == nil {
		s.Repositories = []string{}
	}
	if !validName(s.Name) {
		return errors.New("use a visible session name of at most 80 characters")
	}
	if err := validateMembers(s.Accounts); err != nil {
		return err
	}
	for _, r := range s.Repositories {
		if !validRepositoryScope(r) {
			return errors.New("repositories use owner/name or owner/*")
		}
	}
	return nil
}

func (a *app) saveSession(s session) error {
	p, err := a.sessionPath(s.ID)
	if err != nil {
		return err
	}
	if err = s.validate(); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(p, "session.json"), encode(s), 0600)
}

// Sessions are listed by name. Unreadable ones, such as an interrupted
// creation, are skipped so they never block the others.
func (a *app) sessions() ([]session, error) {
	entries, err := os.ReadDir(filepath.Join(a.root, ".sessions"))
	if os.IsNotExist(err) {
		return []session{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []session{}
	for _, entry := range entries {
		if !sessionID.MatchString(entry.Name()) {
			continue
		}
		if s, err := a.readSession(entry.Name()); err == nil {
			result = append(result, s)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		return a < b || a == b && result[i].ID < result[j].ID
	})
	return result, nil
}

func (a *app) findSession(ref string) (session, error) {
	if sessionID.MatchString(ref) {
		if s, err := a.readSession(ref); err == nil {
			return s, nil
		}
	}
	all, err := a.sessions()
	if err != nil {
		return session{}, err
	}
	matches := []session{}
	for _, s := range all {
		if strings.EqualFold(s.Name, ref) {
			matches = append(matches, s)
		}
	}
	if len(matches) > 1 {
		return session{}, fmt.Errorf("several sessions are named %q; use the ID from %s", ref, filepath.Join(a.root, ".sessions"))
	}
	if len(matches) == 0 {
		return session{}, fmt.Errorf("no session %q; see commander session list", ref)
	}
	return matches[0], nil
}

// Names are unique regardless of case, so a name always selects one session.
func (a *app) checkName(name, id string) error {
	if !validName(name) {
		return errors.New("use a visible session name of at most 80 characters")
	}
	all, err := a.sessions()
	if err != nil {
		return err
	}
	for _, s := range all {
		if s.ID != id && strings.EqualFold(s.Name, name) {
			return fmt.Errorf("session %s already exists", s.Name)
		}
	}
	return nil
}

func (a *app) newSession(name string, accounts []member, repos []string) (session, error) {
	unlock, err := a.waitLock("sessions")
	if err != nil {
		return session{}, err
	}
	defer unlock()
	if err = a.checkName(name, ""); err != nil {
		return session{}, err
	}
	id, err := newSessionID()
	if err != nil {
		return session{}, err
	}
	s := session{ID: id, Name: name, Accounts: accounts, Repositories: repos}
	if err = s.validate(); err != nil {
		return session{}, err
	}
	if err = os.MkdirAll(filepath.Join(a.root, ".sessions", id), 0700); err != nil {
		return session{}, err
	}
	return s, a.saveSession(s)
}

func newSessionID() (string, error) {
	b := make([]byte, 12)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

// UpdateSession changes the current state of a session under the sessions
// lock, so concurrent renames and edits never overwrite each other.
func (a *app) updateSession(id string, change func(*session) error) error {
	unlock, err := a.waitLock("sessions")
	if err != nil {
		return err
	}
	defer unlock()
	s, err := a.readSession(id)
	if err != nil {
		return err
	}
	if err = change(&s); err != nil {
		return err
	}
	return a.saveSession(s)
}

func (a *app) renameSession(s session, name string) error {
	return a.updateSession(s.ID, func(s *session) error {
		s.Name = name
		return a.checkName(name, s.ID)
	})
}

// Deleting waits for no command, keeps unfinished tasks and removes the
// session's containers before its selection.
func (a *app) deleteSession(s session) error {
	p, err := a.sessionPath(s.ID)
	if err != nil {
		return err
	}
	unlock, err := a.sessionLock(s.ID, false, false)
	if err != nil {
		return errors.New("session " + s.Name + " is still in use; exit its shells first")
	}
	defer unlock()
	if entries, _ := os.ReadDir(filepath.Join(p, "tasks")); len(entries) > 0 {
		return errors.New("finish the tasks of session " + s.Name + " first")
	}
	scoped := *a
	scoped.session = &s
	if err = scoped.removeSessionContainers(); err != nil {
		return err
	}
	if err = scoped.stopBridge(); err != nil {
		return err
	}
	release, err := a.waitLock("sessions")
	if err != nil {
		return err
	}
	defer release()
	return os.RemoveAll(p)
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

func (s session) summary() string {
	parts := []string{}
	for _, m := range s.Accounts {
		name, _ := m.display()
		parts = append(parts, serviceName(m.Service)+" "+name)
	}
	if len(parts) == 0 {
		return "no accounts"
	}
	return strings.Join(parts, " · ")
}

// Selection flags replace a session's accounts or repositories. Without
// flags, a terminal opens the selection screen.
func (a *app) selectionFlags(s *session, args []string) error {
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return errors.New("selection option needs a value")
		}
		switch args[i] {
		case "--accounts":
			accounts, err := a.parseAccounts(args[i+1])
			if err != nil {
				return err
			}
			s.Accounts = accounts
		case "--repos":
			s.Repositories = parseRepositories(args[i+1])
		default:
			return errors.New("use --accounts service=user,... and --repos owner/name,owner/*")
		}
	}
	if len(args) > 0 || !a.terminal() {
		return nil
	}
	accounts, repos, err := a.editSelection(s.Name, s.Accounts, s.Repositories)
	s.Accounts, s.Repositories = accounts, repos
	return err
}

func (a *app) sessionCommand(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		all, err := a.sessions()
		if err != nil {
			return err
		}
		rows := [][]string{}
		for _, s := range all {
			rows = append(rows, []string{s.Name, s.summary(), strings.Join(s.Repositories, " ")})
		}
		for _, line := range table(rows) {
			fmt.Fprintln(a.out, line)
		}
		return nil
	}
	usage := map[string]string{"new": "NAME [--accounts service=user,...] [--repos owner/*,...]", "edit": "NAME [--accounts ...] [--repos ...]", "rename": "NAME NEW", "delete": "NAME", "shell": "NAME"}
	if usage[args[0]] == "" {
		return errors.New("session commands: list, new, edit, rename, delete, shell")
	}
	fixed := map[string]int{"rename": 3, "delete": 2, "shell": 2}[args[0]]
	if len(args) < 2 || fixed > 0 && len(args) != fixed {
		return errors.New("usage: commander session " + args[0] + " " + usage[args[0]])
	}
	if args[0] == "new" {
		s := session{Name: args[1]}
		if err := a.checkName(s.Name, ""); err != nil {
			return err
		}
		if err := a.selectionFlags(&s, args[2:]); err != nil {
			return err
		}
		s, err := a.newSession(s.Name, s.Accounts, s.Repositories)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "session %s created · open it with: commander session shell %s\n", s.Name, quote(s.Name))
		return nil
	}
	s, err := a.findSession(args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "edit":
		if err = a.selectionFlags(&s, args[2:]); err != nil {
			return err
		}
		return a.updateSession(s.ID, func(current *session) error {
			current.Accounts, current.Repositories = s.Accounts, s.Repositories
			return nil
		})
	case "rename":
		return a.renameSession(s, args[2])
	case "delete":
		return a.deleteSession(s)
	}
	return a.sessionShell(s)
}

func (a *app) sessionShell(s session) error {
	scoped := *a
	scoped.session = &s
	unlock, err := scoped.sessionLock(s.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err = scoped.sessionWrappers(s); err != nil {
		return err
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	fmt.Fprintf(a.out, "session %s · exit returns\n", clean(s.Name))
	cmd := exec.Command(shell)
	cmd.Env = scoped.environment("shell", nil)
	cmd.Stdin = a.in
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	return cmd.Run()
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

// Logins lists the connected accounts of used services. A failed service keeps
// a status, so screens can explain why its accounts are missing.
func (a *app) logins() ([]account, map[string]string) {
	admin := a.admin()
	rows := []account{}
	failed := map[string]string{}
	for _, service := range services {
		if !tokenService(service) && !a.toolUsed(serviceTools[service]) {
			continue
		}
		found, err := admin.accounts(service)
		if err != nil {
			failed[service] = serviceFailure(err)
			continue
		}
		rows = append(rows, found...)
	}
	return rows, failed
}

// Admin views read every connected login with native tools, outside any
// session selection.
func (a *app) admin() *app {
	admin := *a
	if a.session != nil {
		admin.session = nil
		admin.run = admin.nativeRun
	}
	return &admin
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
