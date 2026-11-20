package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var services = []string{"codex", "claude", "litellm", "github", "azure", "office", "hetzner", "gcloud", "dockerhub", "confluence"}
var serviceTools = map[string]string{"github": "gh", "azure": "az", "office": "m365", "hetzner": "hcloud", "gcloud": "gcloud", "dockerhub": "dockerhub", "confluence": "confluence"}

func serviceName(service string) string {
	if name := map[string]string{"litellm": "openai-compatible", "github": "github", "azure": "azure", "office": "microsoft 365", "hetzner": "hetzner", "gcloud": "google cloud", "dockerhub": "dockerhub", "confluence": "confluence"}[service]; name != "" {
		return name
	}
	return service
}

type member struct {
	Service string          `json:"service"`
	Key     json.RawMessage `json:"key"`
	Account string          `json:"account"`
	Context string          `json:"context"`
	Repos   []string        `json:"repos,omitempty"`
}
type account struct {
	member
	Active bool
	State  string
}

// Archives keep sessions in this named selection layout, which earlier
// releases used for saved selections.
type selections struct {
	Version  int                 `json:"version"`
	Active   *string             `json:"active"` // always null; kept for readable archives
	Profiles map[string][]member `json:"profiles"`
	Scopes   map[string][]string `json:"repositories,omitempty"`
}

func jsonKey(value any) json.RawMessage { b, _ := json.Marshal(value); return b }
func (m member) selector() string       { var key string; json.Unmarshal(m.Key, &key); return key }
func (m member) github() (string, string) {
	var key []string
	json.Unmarshal(m.Key, &key)
	if len(key) != 2 {
		return "", ""
	}
	return key[0], key[1]
}

// ref is the selector people type for a login: the GitHub user or the key.
func (m member) ref() string {
	if _, user := m.github(); user != "" {
		return user
	}
	return m.selector()
}
func (m member) id() string {
	var value any
	json.Unmarshal(m.Key, &value)
	return m.Service + ":" + string(jsonKey(value))
}

// Display returns the account name and its context as screens show them.
func (m member) display() (string, string) {
	if host, user := m.github(); m.Service == "github" {
		return user, host
	}
	if m.Service == "hetzner" {
		return m.Account, ""
	}
	return m.Account, m.Context
}

// Item shows the account name, with its context when that tells logins apart.
func (m member) item() uiLine {
	name, context := m.display()
	if context = strings.TrimPrefix(context, "https://"); context == "" || context == "github.com" {
		return uiLine{{name, ""}}
	}
	return uiLine{{name, ""}, {" (" + context + ")", "2"}}
}
func (m member) label() string {
	s := serviceName(m.Service) + " · " + m.Account
	if m.Context != "" {
		s += " · " + m.Context
	}
	return s
}
func printable(s string, limit int) bool {
	if utf8.RuneCountInString(s) > limit {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return utf8.ValidString(s)
}
func validName(s string) bool { return s != "" && strings.TrimSpace(s) == s && printable(s, 80) }
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s)
}

func validateMembers(members []member) error {
	invalid := errors.New("invalid account selection")
	if members == nil {
		return invalid
	}
	seen := map[string]bool{}
	for _, m := range members {
		if !contains(services, m.Service) {
			return invalid
		}
		if !printable(m.Account, 1024) || !printable(m.Context, 1024) {
			return invalid
		}
		if codingService(m.Service) && (!codingLabel.MatchString(m.selector()) || m.selector() != m.Account || m.Context != "") {
			return invalid
		}
		for _, scope := range m.Repos {
			if m.Service != "github" || !validRepositoryScope(scope) {
				return invalid
			}
		}
		if m.Service == "github" {
			host, user := m.github()
			if host == "" || user == "" || !printable(host, 1024) || !printable(user, 1024) {
				return invalid
			}
		} else {
			key := m.selector()
			if key == "" || !printable(key, 1024) {
				return invalid
			}
		}
		if seen[m.id()] {
			return invalid
		}
		seen[m.id()] = true
	}
	return nil
}

func validateSelections(p selections) error {
	invalid := errors.New("invalid archive selection")
	if p.Version != 1 || p.Profiles == nil {
		return invalid
	}
	for name, repos := range p.Scopes {
		if _, ok := p.Profiles[name]; !ok {
			return invalid
		}
		for _, r := range repos {
			if !validRepositoryScope(r) {
				return invalid
			}
		}
	}
	for name, members := range p.Profiles {
		if !validName(name) || validateMembers(members) != nil {
			return invalid
		}
	}
	return nil
}

func (a *app) accounts(service string) ([]account, error) {
	if service == "github" {
		nativeUnlock, err := a.installationLock("native", true, true)
		if err != nil {
			return nil, err
		}
		defer nativeUnlock()
		// Status can rotate tokens too, so it shares the runtime refresh lock.
		unlock, err := a.credentialLock("github")
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	return a.queryAccounts(service)
}

func (a *app) queryAccounts(service string) ([]account, error) {
	if codingService(service) {
		return a.codingAccounts(service)
	}
	if tokenService(service) {
		return a.tokenAccounts(service)
	}
	commands := map[string][]string{
		"github": {"auth", "status", "--json", "hosts"}, "azure": {"account", "list", "--all", "--output", "json"},
		"office": {"connection", "list", "--output", "json"}, "hetzner": {"context", "list", "--output", "json", "--allow-sensitive=false"},
		"gcloud": {"auth", "list", "--format=json"},
	}
	args, ok := commands[service]
	if !ok {
		return nil, errors.New("unknown service")
	}
	data, err := a.run(serviceTools[service], args, true, nil)
	if err != nil {
		return nil, err
	}
	var records []map[string]any
	if service == "github" {
		var response struct {
			Hosts map[string][]map[string]any `json:"hosts"`
		}
		if json.Unmarshal(data, &response) != nil {
			return nil, errors.New("could not read GitHub account information")
		}
		hosts := []string{}
		for h := range response.Hosts {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			for _, r := range response.Hosts[h] {
				r["host"] = h
				records = append(records, r)
			}
		}
	} else if json.Unmarshal(data, &records) != nil {
		return nil, fmt.Errorf("could not read %s account information", service)
	}
	rows := []account{}
	indices := map[string]int{}
	for _, r := range records {
		get := func(key string) string { s, _ := r[key].(string); return clean(s) }
		flag := func(key string) bool { b, _ := r[key].(bool); return b }
		row := account{member: member{Service: service}, State: "Ready"}
		switch service {
		case "github":
			if get("login") == "" {
				continue
			}
			row.Key = jsonKey([]string{get("host"), get("login")})
			row.Account = get("login") + " @ " + get("host")
			row.Active = flag("active")
			switch {
			case get("state") == "success":
			case get("state") == "refresh_expired" || strings.HasPrefix(get("error"), "HTTP 401:"):
				row.State = "Sign in again"
			case get("state") == "timeout":
				row.State = "Check connection"
			default:
				row.State = "Could not check"
			}
		case "azure":
			row.Key = jsonKey(get("id"))
			user, _ := r["user"].(map[string]any)
			name, _ := user["name"].(string)
			row.Account = clean(name)
			row.Context = get("name")
			row.Active = flag("isDefault")
		case "office":
			row.Key = jsonKey(get("name"))
			row.Account = get("connectedAs")
			if row.Account == "" {
				row.Account = get("name")
			}
			row.Context = get("appTenant")
			row.Active = flag("active")
		case "hetzner":
			row.Key = jsonKey(get("name"))
			row.Account = get("name")
			row.Context = "Project"
			row.Active = flag("active")
		case "gcloud":
			row.Key = jsonKey(get("account"))
			row.Account = get("account")
			row.Active = get("status") == "ACTIVE"
		}
		if idx, ok := indices[row.id()]; ok {
			if row.Active {
				rows[idx] = row
			}
			continue
		}
		indices[row.id()] = len(rows)
		rows = append(rows, row)
	}
	return rows, nil
}

// A failed account query marks every account of its service.
func serviceFailure(err error) string {
	var preparation *toolPreparationError
	if errors.As(err, &preparation) {
		return "Tool not ready"
	}
	return "Check login"
}

func (a *app) resolve(members []member) []account {
	checked := time.Now().UTC()
	admin := a.admin()
	found := map[string]account{}
	queried := map[string]bool{}
	failed := map[string]string{}
	for _, m := range members {
		if queried[m.Service] {
			continue
		}
		queried[m.Service] = true
		rows, err := admin.accounts(m.Service)
		if err != nil {
			failed[m.Service] = serviceFailure(err)
			continue
		}
		for _, r := range rows {
			found[r.id()] = r
		}
	}
	result := make([]account, 0, len(members))
	for _, m := range members {
		r, ok := found[m.id()]
		state := "Missing account"
		if failed[m.Service] != "" {
			state = failed[m.Service]
		} else if ok {
			state = r.State
			if tokenService(m.Service) {
				records, err := a.tokenLogins(m.Service)
				if err == nil {
					_, err = records[m.selector()].verify(m.Service)
				}
				if err != nil {
					state = "Check login"
				}
			}
			if r.Account != m.Account || tokenService(m.Service) && r.Context != m.Context {
				state = "Check identity"
			}
		}
		result = append(result, account{member: m, Active: r.Active, State: state})
	}
	a.rememberHealthAt(result, checked)
	return result
}

func ready(state string) bool { return state == "Ready" }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Describe prints a session's accounts: overview checks their status, context
// adds selectors for agents and fails when an account needs attention.
// Overview without a session checks every connected login.
func (a *app) describe(name string, context bool) error {
	s := a.session
	if name != "" {
		found, err := a.findSession(name)
		if err != nil {
			return err
		}
		s = &found
	}
	if s == nil {
		if context {
			return errNoSession
		}
		rows, failed := a.logins()
		for _, service := range services {
			if failed[service] != "" {
				rows = append(rows, account{member: member{Service: service}, State: failed[service]})
			}
		}
		a.printAccounts(rows)
		return attention(rows)
	}
	if len(s.Accounts) == 0 && context {
		return fmt.Errorf("session %q has no accounts; select them with commander session edit %s", s.Name, quote(s.Name))
	}
	rows := a.resolve(s.Accounts)
	if context {
		if err := attention(rows); err != nil {
			return err
		}
	}
	fmt.Fprintln(a.out, "session:", s.Name)
	fmt.Fprintln(a.out, "repositories:", strings.Join(s.Repositories, ", "))
	if !context {
		a.printAccounts(rows)
		return attention(rows)
	}
	fmt.Fprintln(a.out, "\nScope: selected accounts and repositories only. Verify identity before actions.\nUse commander exec --account SELECTOR -- TOOL ARGS when a service has several accounts.\nEach command gets isolated login state. Never switch a global active account.\nSelections grant no permission to change resources or send messages.")
	for _, r := range rows {
		selector := r.ref()
		fmt.Fprintf(a.out, "\n%s: %s\nselector: %s\n", r.Service, jsonKey(r.Account), quote(selector))
		if r.Context != "" {
			fmt.Fprintf(a.out, "context: %s\n", jsonKey(r.Context))
		}
		if tokenService(r.Service) {
			fmt.Fprintf(a.out, "verify: ./commander exec -- %s whoami --account %s\n", r.Service, quote(selector))
		}
	}
	return nil
}

func attention(rows []account) error {
	invalid := []string{}
	for _, r := range rows {
		if !ready(r.State) {
			name, _ := r.display()
			invalid = append(invalid, strings.TrimSpace(serviceName(r.Service)+" "+name)+": "+strings.ToLower(r.State))
		}
	}
	if len(invalid) > 0 {
		return errors.New("accounts need attention: " + strings.Join(invalid, "; ") + "; open ./commander and press [l] for logins or [c] to check again")
	}
	return nil
}

func (a *app) printAccounts(rows []account) {
	width := 0
	if a.terminal() {
		width, _ = a.dimensions()
		width = max(1, width-2)
	}
	fmt.Fprintln(a.out)
	for _, line := range accountLines(rows, width) {
		if width > 0 {
			fmt.Fprintln(a.out, a.renderLine(line, width))
		} else {
			fmt.Fprintln(a.out, line.plain())
		}
	}
}

const githubScopes = "repo,workflow,admin:org,admin:org_hook,admin:public_key,admin:repo_hook,admin:gpg_key,admin:ssh_signing_key,admin:enterprise,gist,notifications,user,delete_repo,write:packages,delete:packages,project,codespace,read:audit_log"

func (a *app) officeSettings() error {
	for _, pair := range [][2]string{{"copyDeviceCodeToClipboard", "false"}, {"autoOpenLinksInBrowser", "false"}, {"authType", "deviceCode"}} {
		if _, err := a.run("m365", []string{"cli", "config", "set", "--key", pair[0], "--value", pair[1], "--output", "none"}, true, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) officeOrganization() error {
	fmt.Fprintln(a.out, "set up microsoft 365 with an administrator. follow the browser sign-in.")
	name := "commander-setup-" + rand.Text()
	if _, err := a.run("m365", []string{"login", "--authType", "deviceCode", "--appId", "04b07795-8ddb-461a-bbee-02f9e1bf7b46", "--tenant", "organizations", "--connectionName", name, "--output", "text"}, false, nil); err != nil {
		return err
	}
	_, setupErr := a.run("m365", []string{"cli", "app", "add", "--name", "Commander", "--scopes", "all", "--saveToConfig", "--output", "text"}, false, nil)
	_, cleanupErr := a.run("m365", []string{"connection", "remove", "--name", name, "--force", "--output", "none"}, true, nil)
	return errors.Join(setupErr, cleanupErr)
}

func (a *app) login(service string) error {
	if tokenService(service) {
		return a.loginToken(service)
	}
	switch service {
	case "github":
		unlock, err := a.credentialLock("github")
		if err != nil {
			return err
		}
		defer unlock()
		return a.githubLogin("github.com", map[string]string{"GH_PROMPT_DISABLED": "1"})
	case "azure":
		_, err := a.run("az", []string{"login", "--use-device-code", "--allow-no-subscriptions", "--output", "none"}, false, map[string]string{"AZURE_CORE_LOGIN_EXPERIENCE_V2": "off"})
		return err
	case "office":
		if err := a.officeSettings(); err != nil {
			return err
		}
		id, err := a.run("m365", []string{"cli", "config", "get", "--key", "clientId", "--output", "text"}, true, nil)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(id)) == "" {
			if err := a.officeOrganization(); err != nil {
				return err
			}
			id, err = a.run("m365", []string{"cli", "config", "get", "--key", "clientId", "--output", "text"}, true, nil)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(id)) == "" {
				return errors.New("Office setup was not completed; add the login again")
			}
		}
		fmt.Fprintln(a.out, "sign in with the microsoft 365 account to add.")
		_, err = a.run("m365", []string{"login", "--authType", "deviceCode", "--output", "text"}, false, nil)
		return err
	case "gcloud":
		_, err := a.run("gcloud", []string{"auth", "login", "--no-launch-browser", "--enable-gdrive-access", "--brief"}, false, nil)
		return err
	case "hetzner":
		fmt.Fprintln(a.out, "create a project token at https://console.hetzner.cloud")
		token, err := a.password("project token", false)
		if err != nil {
			return err
		}
		if len(token) != 64 || strings.IndexFunc(token, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 {
			return errors.New("a Hetzner token contains 64 letters or digits")
		}
		name, err := a.prompt("project name", "", nil)
		if err != nil {
			return err
		}
		if name == "" || strings.HasPrefix(name, "-") || strings.IndexFunc(name, func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.-", r))
		}) >= 0 {
			return errors.New("use letters, digits, dots, underscores or dashes for the project name")
		}
		rows, err := a.accounts(service)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.selector() == name {
				return errors.New("that project name already exists")
			}
		}
		_, err = a.run("hcloud", []string{"context", "create", "--token-from-env", name}, true, map[string]string{"HCLOUD_TOKEN": token})
		return err
	}
	return errors.New("unknown service")
}

func (a *app) githubLogin(host string, extra map[string]string) error {
	_, err := a.run("gh", []string{"auth", "login", "--hostname", host, "--git-protocol", "https", "--web", "--clipboard=false", "--expiring", "--insecure-storage", "--scopes", githubScopes}, false, extra)
	return err
}

// Reconnect verifies the requested identity before replacing its credentials.
// A cancelled login or a different browser account leaves saved logins intact.
func (a *app) reconnectGitHub(m member) error {
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	refreshUnlock, err := a.credentialLock("github")
	if err != nil {
		return err
	}
	defer refreshUnlock()
	root, err := os.MkdirTemp(filepath.Join(a.root, ".secrets"), ".login-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	host, user := m.github()
	extra := map[string]string{"GH_CONFIG_DIR": filepath.Join(root, ".secrets", ".config", "gh"), "GH_PROMPT_DISABLED": "1"}
	fmt.Fprintf(a.out, "sign in as %s on %s in your browser.\n\n", clean(user), clean(host))
	if err = a.githubLogin(host, extra); err != nil {
		return err
	}
	identity, err := a.run("gh", []string{"api", "--hostname", host, "user", "--jq", ".login"}, true, extra)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(identity)) != user {
		return fmt.Errorf("sign in as %s; the browser used a different account", clean(user))
	}
	state, err := newStorage(root).state("github")
	if err != nil {
		return err
	}
	selected, err := selectLogins("github", state, []member{m})
	if err != nil {
		return err
	}
	store := newStorage(a.root)
	current, err := store.state("github")
	if err != nil {
		return err
	}
	merged := mergeCredentialState("github", current, selected)
	if err = store.publish(map[string][]byte{".secrets/.config/gh/hosts.yml": encode(merged)}); err != nil {
		return err
	}
	a.rememberHealth([]account{{member: m, State: "Ready"}})
	return nil
}

func (a *app) deleteLogin(m member) error {
	if codingService(m.Service) {
		unlock, err := a.nativeLoginLock(m, false)
		if err != nil {
			return err
		}
		defer unlock()
		dir, err := a.codingDir(m)
		if err != nil {
			return err
		}
		return os.RemoveAll(dir)
	}
	if tokenService(m.Service) {
		records, err := a.tokenLogins(m.Service)
		if err != nil {
			return err
		}
		delete(records, m.selector())
		return a.writeTokenLogins(m.Service, records)
	}
	var args []string
	switch m.Service {
	case "github":
		unlock, err := a.credentialLock("github")
		if err != nil {
			return err
		}
		defer unlock()
		host, user := m.github()
		args = []string{"auth", "logout", "--hostname", host, "--user", user}
	case "azure":
		args = []string{"logout", "--username", m.Account, "--output", "none"}
	case "office":
		args = []string{"connection", "remove", "--name", m.selector(), "--force", "--output", "none"}
	case "hetzner":
		args = []string{"context", "delete", m.selector()}
	case "gcloud":
		args = []string{"auth", "revoke", m.selector(), "--quiet"}
	}
	_, err := a.run(serviceTools[m.Service], args, true, map[string]string{"GH_PROMPT_DISABLED": "1"})
	return err
}
