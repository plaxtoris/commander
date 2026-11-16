package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The two small HTTP CLIs share Commander's private storage and profile scope.
// Tokens never appear in command arguments, diagnostics or account listings.
type tokenLogin struct {
	User     string `json:"user"`
	Token    string `json:"token"`
	Site     string `json:"site,omitempty"`
	API      string `json:"api,omitempty"`
	Identity string `json:"identity"`
}

var serviceHTTP = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func tokenService(service string) bool { return service == "dockerhub" || service == "confluence" }
func tokenFile(service string) string  { return ".config/commander/" + service + ".json" }

func (c tokenLogin) member(service string) member {
	key := c.User
	if service == "confluence" {
		key += " @ " + c.Site
	}
	return member{Service: service, Key: jsonKey(key), Account: c.User, Context: c.Site}
}

func cloudSite(value string) (string, error) {
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Hostname(), ".atlassian.net") || (u.Path != "" && u.Path != "/" && u.Path != "/wiki" && u.Path != "/wiki/") {
		return "", errors.New("enter your Confluence Cloud URL, such as https://team.atlassian.net")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func (c tokenLogin) valid(service string) bool {
	if c.User == "" || !printable(c.User, 320) || strings.TrimSpace(c.User) != c.User || c.Token == "" || !printable(c.Token, 8192) || c.Identity == "" || !printable(c.Identity, 1024) {
		return false
	}
	if service == "dockerhub" {
		return c.Site == "" && c.API == "" && c.Identity == c.User && !strings.ContainsAny(c.User, " /:@")
	}
	site, err := cloudSite(c.Site)
	if err != nil || site != c.Site || !strings.Contains(c.User, "@") || strings.Contains(c.User, ":") {
		return false
	}
	if c.API == c.Site+"/wiki" {
		return true
	}
	const gateway = "https://api.atlassian.com/ex/confluence/"
	id := strings.TrimSuffix(strings.TrimPrefix(c.API, gateway), "/wiki")
	return strings.HasPrefix(c.API, gateway) && strings.HasSuffix(c.API, "/wiki") && validCloudID(id)
}

func validCloudID(id string) bool {
	return id != "" && len(id) <= 100 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
	}) == -1
}

func decodeTokenLogin(service, key string, value any) (tokenLogin, error) {
	var c tokenLogin
	if json.Unmarshal(encode(value), &c) != nil || !c.valid(service) || c.member(service).selector() != key {
		return c, errFormat
	}
	return c, nil
}

func (a *app) tokenLogins(service string) (map[string]tokenLogin, error) {
	state, err := newStorage(a.root).state(service)
	if err != nil {
		return nil, err
	}
	result := map[string]tokenLogin{}
	for key, value := range state {
		c, err := decodeTokenLogin(service, key, value)
		if err != nil {
			return nil, err
		}
		result[key] = c
	}
	return result, nil
}

func (a *app) writeTokenLogins(service string, records map[string]tokenLogin) error {
	path := filepath.Join(a.root, ".secrets", tokenFile(service))
	if err := noSymlinks(a.root, path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWrite(path, encode(records), 0600)
}

type serviceError int

func (e serviceError) Error() string {
	switch e {
	case 401:
		return "token rejected; reconnect this account"
	case 403:
		return "access denied; check token permissions"
	case 429:
		return "rate limit reached; try again shortly"
	}
	return fmt.Sprintf("service returned HTTP %d", e)
}

// Do not include URLs, response bodies or transport errors in diagnostics:
// services and proxies can reflect credentials into them.
func requestService(method, target, authorization string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid API request")
	}
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := serviceHTTP.Do(req)
	if err != nil {
		return nil, errors.New("service unavailable; check your connection")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, serviceError(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if err != nil || len(data) > 32<<20 {
		return nil, errors.New("API response is incomplete or too large")
	}
	return data, nil
}

func (c tokenLogin) confluenceIdentity() (string, error) {
	data, err := requestService("GET", c.API+"/rest/api/user/current", c.authorization(), nil)
	if err != nil {
		return "", err
	}
	var user struct {
		AccountID string `json:"accountId"`
		Type      string `json:"type"`
	}
	if json.Unmarshal(data, &user) != nil || user.AccountID == "" || !printable(user.AccountID, 1024) || user.Type == "anonymous" {
		return "", errors.New("could not verify Confluence identity")
	}
	return user.AccountID, nil
}

func (c tokenLogin) authorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Token))
}

func (c tokenLogin) verify(service string) (string, error) {
	if service == "dockerhub" {
		// Docker authenticates the username/PAT pair before issuing a bearer token.
		data, err := requestService("POST", "https://hub.docker.com/v2/auth/token", "", encode(object{"identifier": c.User, "secret": c.Token}))
		if err != nil {
			return "", err
		}
		var response struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(data, &response) != nil || response.AccessToken == "" {
			return "", errors.New("could not verify Docker Hub identity")
		}
		return "Bearer " + response.AccessToken, nil
	}
	identity, err := c.confluenceIdentity()
	if err != nil {
		return "", err
	}
	if identity != c.Identity {
		return "", errors.New("account identity changed; reconnect this account")
	}
	return c.authorization(), nil
}

func (a *app) loginToken(service string) error {
	c := tokenLogin{}
	var err error
	if service == "confluence" {
		site, e := a.input("confluence url", "")
		if e != nil {
			return e
		}
		c.Site, err = cloudSite(site)
		if err != nil {
			return err
		}
		c.API = c.Site + "/wiki"
		c.User, err = a.input("atlassian email", "")
		if err != nil {
			return err
		}
		fmt.Fprintln(a.out, "create a token at https://id.atlassian.com/manage-profile/security/api-tokens")
	} else {
		c.User, err = a.input("dockerhub username", "")
		if err != nil {
			return err
		}
		c.User = strings.ToLower(c.User)
		fmt.Fprintln(a.out, "create a personal token at https://app.docker.com/settings/personal-access-tokens")
	}
	c.Token, err = a.password("api token", false)
	if err != nil {
		return err
	}
	c.Identity = c.User // Validate input before sending it anywhere.
	if !c.valid(service) {
		return errors.New("check your account details and token")
	}
	fmt.Fprintln(a.errOut, "checking account…")
	if service == "confluence" {
		c.Identity, err = c.confluenceIdentity()
		// Scoped Cloud tokens use the Atlassian gateway. Discover the site's ID
		// without sending credentials and keep the authenticated host fixed.
		var status serviceError
		if errors.As(err, &status) && (status == 401 || status == 403) {
			data, e := requestService("GET", c.Site+"/_edge/tenant_info", "", nil)
			var tenant struct {
				CloudID string `json:"cloudId"`
			}
			if e != nil || json.Unmarshal(data, &tenant) != nil || !validCloudID(tenant.CloudID) {
				return errors.New("could not find this Confluence site")
			}
			c.API = "https://api.atlassian.com/ex/confluence/" + tenant.CloudID + "/wiki"
			c.Identity, err = c.confluenceIdentity()
		}
	} else {
		_, err = c.verify(service)
	}
	if err != nil {
		return err
	}
	records, err := a.tokenLogins(service)
	if err != nil {
		return err
	}
	key := c.member(service).selector()
	if previous, ok := records[key]; ok && previous.Identity != c.Identity {
		return errors.New("account identity changed; remove the saved login first")
	}
	records[key] = c
	if err = a.writeTokenLogins(service, records); err != nil {
		return err
	}
	fmt.Fprintln(a.errOut, "connected.")
	return nil
}

func (a *app) tokenAccounts(service string) ([]account, error) {
	records, err := a.tokenLogins(service)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]account, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, account{member: records[key].member(service), State: "Saved"})
	}
	return rows, nil
}

func (a *app) selectedToken(service, selector string) (tokenLogin, error) {
	_, members, err := a.profile("")
	if err != nil {
		return tokenLogin{}, err
	}
	candidates := []member{}
	for _, m := range members {
		if m.Service == service && (selector == "" || selector == m.selector()) {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return tokenLogin{}, errors.New("add this account to the current profile first")
	}
	if len(candidates) > 1 {
		return tokenLogin{}, errors.New("choose an account with --account; see commander context")
	}
	records, err := a.tokenLogins(service)
	if err != nil {
		return tokenLogin{}, err
	}
	c, ok := records[candidates[0].selector()]
	if !ok || c.member(service).Account != candidates[0].Account || c.member(service).Context != candidates[0].Context {
		return tokenLogin{}, errors.New("saved account changed; reconnect it")
	}
	return c, nil
}

// Only relative API endpoints are accepted. In particular, encoded traversal,
// redirects and absolute URLs cannot forward a saved token to another host.
func apiEndpoint(service, path string) (string, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" || !strings.HasPrefix(path, "/") || strings.Contains(u.Path, "\\") || strings.Contains(u.Path, "//") || strings.Contains(u.Path, "%") {
		return "", errors.New("use a relative API path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", errors.New("invalid API path")
		}
	}
	if service == "confluence" {
		u.Path = strings.TrimPrefix(u.Path, "/wiki")
		u.RawPath = ""
		if !strings.HasPrefix(u.Path, "/rest/api/") && !strings.HasPrefix(u.Path, "/api/v2/") {
			return "", errors.New("use /api/v2/ or /rest/api/ for Confluence")
		}
	} else if !strings.HasPrefix(u.Path, "/v2/namespaces/") {
		return "", errors.New("use /v2/namespaces/ for Docker Hub repositories and tags")
	}
	return u.String(), nil
}

func (a *app) tokenRun(service string, args []string, capture bool) ([]byte, error) {
	if service == "dockerhub" && len(args) > 0 && args[0] == "docker" {
		selector := ""
		args = args[1:]
		if len(args) >= 2 && args[0] == "--account" {
			selector, args = args[1], args[2:]
		}
		if len(args) == 0 || args[0] != "--" {
			return nil, errors.New("use dockerhub docker [--account ACCOUNT] -- DOCKER_ARGS")
		}
		if _, err := a.selectedToken(service, selector); err != nil {
			return nil, err
		}
		config, cleanup, err := a.dockerConfig(selector)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if err = a.prepareTool("docker", false); err != nil {
			return nil, err
		}
		return a.nativeRunContext(context.Background(), "docker", args[1:], capture, map[string]string{"DOCKER_CONFIG": config})
	}
	var out bytes.Buffer
	emit := func(data []byte) ([]byte, error) {
		if capture {
			return data, nil
		}
		_, err := a.out.Write(data)
		return nil, err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(&out, "%s · commander built-in cli\n\n  whoami [--account ACCOUNT]\n  api PATH [-X METHOD] [--input FILE|-] [--account ACCOUNT]\n\nuses the current profile. add tokens through commander accounts.\n", serviceName(service))
		if service == "dockerhub" {
			fmt.Fprint(&out, "  docker [--account ACCOUNT] -- DOCKER_ARGS\n\n")
			fmt.Fprintln(&out, "example: api /v2/namespaces/USERNAME/repositories")
		} else {
			fmt.Fprintln(&out, "example: api /api/v2/pages?limit=10")
		}
		return emit(out.Bytes())
	}
	if args[0] == "version" || args[0] == "--version" {
		return emit([]byte("Commander " + build + "\n"))
	}
	command, selector, method, input, path := args[0], "", "GET", "", ""
	if command != "whoami" && command != "api" {
		return nil, errors.New("use whoami or api; see --help")
	}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--account", "-X", "--method", "--input":
			flag := args[i]
			i++
			if i == len(args) {
				return nil, errors.New("missing option value; see --help")
			}
			switch flag {
			case "--account":
				selector = args[i]
			case "--input":
				input = args[i]
			default:
				method = strings.ToUpper(args[i])
			}
		default:
			if path != "" || strings.HasPrefix(args[i], "-") {
				return nil, errors.New("unexpected argument; see --help")
			}
			path = args[i]
		}
	}
	if !contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}, method) {
		return nil, errors.New("unsupported HTTP method")
	}
	if command == "whoami" && (path != "" || input != "" || method != "GET") {
		return nil, errors.New("whoami only accepts --account")
	}
	if command == "api" {
		var err error
		path, err = apiEndpoint(service, path)
		if err != nil {
			return nil, err
		}
	}
	var body []byte
	if input != "" {
		f := a.in
		if input != "-" {
			var err error
			f, err = os.Open(input)
			if err != nil {
				return nil, errors.New("could not open input file")
			}
			defer f.Close()
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(f, 32<<20+1))
		if err != nil || len(body) > 32<<20 {
			return nil, errors.New("request body is incomplete or too large")
		}
	}
	c, err := a.selectedToken(service, selector)
	if err != nil {
		return nil, err
	}
	auth, err := c.verify(service)
	if err != nil {
		return nil, err
	}
	if command == "whoami" {
		return emit(encode(object{"service": service, "account": c.User, "context": c.Site, "identity": c.Identity}))
	}
	base := c.API
	if service == "dockerhub" {
		base = "https://hub.docker.com"
	}
	data, err := requestService(method, base+path, auth, body)
	if err != nil {
		return nil, err
	}
	return emit(data)
}

// A private, short-lived Docker config supplies only the selected Hub account.
// Docker and devcontainers inherit it; other profiles and host logins stay out.
func (a *app) dockerConfig(selector string) (string, func(), error) {
	empty := func() {}
	p, err := a.readProfiles()
	if err != nil {
		return "", empty, err
	}
	hasAccount := false
	if p.Active != nil {
		for _, m := range p.Profiles[*p.Active] {
			hasAccount = hasAccount || m.Service == "dockerhub"
		}
	}
	// An explicit empty Hub entry also prevents Docker from discovering a host
	// credential helper when this profile has no Hub login.
	auths := object{"https://index.docker.io/v1/": object{}}
	if hasAccount || selector != "" {
		c, err := a.selectedToken("dockerhub", selector)
		if err != nil {
			return "", empty, err
		}
		if _, err = c.verify("dockerhub"); err != nil {
			return "", empty, err
		}
		auths["https://index.docker.io/v1/"] = object{"auth": base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Token))}
	}
	if err := a.ensure(); err != nil {
		return "", empty, err
	}
	dir, err := os.MkdirTemp(filepath.Join(a.root, ".secrets"), ".docker-")
	if err != nil {
		return "", empty, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err = atomicWrite(filepath.Join(dir, "config.json"), encode(object{"auths": auths}), 0600); err != nil {
		cleanup()
		return "", empty, err
	}
	return dir, cleanup, nil
}
