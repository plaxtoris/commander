package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var codingLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
var agents = map[string]string{"codex": "codex", "claude": "claude", "litellm": "pi"}
var credentialFiles = map[string]string{"codex": "auth.json", "claude": "account.json", "litellm": "endpoint.json"}

func codingService(service string) bool { return agents[service] != "" }

func agentService(tool string) string {
	for service, agent := range agents {
		if tool == agent {
			return service
		}
	}
	return ""
}

// Active agents share a lease. An update can retry the next time Commander opens.
func (a *app) agentLock(tool string, running bool) (func(), error) {
	return a.installationLock(tool+"-tool", running, running)
}

func (a *app) prepareAgent(service string) (func(), error) {
	tool := agents[service]
	if err := a.prepareTool(tool); err != nil {
		return nil, err
	}
	return a.agentLock(tool, true)
}

func (a *app) codingDir(m member) (string, error) {
	if !codingService(m.Service) || !codingLabel.MatchString(m.selector()) {
		return "", errFormat
	}
	path := filepath.Join(a.root, ".secrets", "coding", m.Service, m.selector())
	return path, noSymlinks(a.root, path)
}
func (a *app) codingAccounts(service string) ([]account, error) {
	base := filepath.Join(a.root, ".secrets", "coding", service)
	if err := noSymlinks(a.root, base); err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []account
	for _, d := range dirs {
		if !d.IsDir() || !codingLabel.MatchString(d.Name()) {
			continue
		}
		path := filepath.Join(base, d.Name(), credentialFiles[service])
		if err := noSymlinks(a.root, path); err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		rows = append(rows, account{member: member{Service: service, Key: jsonKey(d.Name()), Account: d.Name()}, State: "Ready"})
	}
	return rows, nil
}

// Give each agent its own native login and remove inherited provider credentials.
// Session wrappers and Git routing still use the normal Commander environment.
func (a *app) codingEnv(m member, profile, home string) []string {
	var env []string
	for _, line := range a.environment("shell", nil) {
		key, _, _ := strings.Cut(line, "=")
		switch {
		case contains([]string{"PATH", "TERM", "COLORTERM", "TERM_PROGRAM", "VISUAL", "EDITOR", "XAUTHORITY", "WSL_DISTRO_NAME", "WSL_INTEROP", "LANG", "SHELL", "USER", "LOGNAME", "TZ", "NO_COLOR", "DISPLAY", "WAYLAND_DISPLAY", "BROWSER", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "GH_CONFIG_DIR", "HCLOUD_CONFIG", "AZURE_CONFIG_DIR", "CLOUDSDK_CONFIG", "DOCKER_CONFIG"}, key), contains([]string{"COMMANDER_HOME", "COMMANDER_SESSION", "COMMANDER_WORKSPACE"}, key), strings.HasPrefix(key, "GIT_CONFIG_"), strings.HasPrefix(key, "GIT_AUTHOR_"), strings.HasPrefix(key, "GIT_COMMITTER_"), strings.HasPrefix(key, "LC_"):
			env = append(env, line)
		}
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	switch m.Service {
	case "codex":
		env = append(env, "CODEX_HOME="+profile)
	case "claude":
		env = append(env, "CLAUDE_CONFIG_DIR="+profile, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	case "litellm":
		env = append(env, "PI_CODING_AGENT_DIR="+home, "PI_SKIP_VERSION_CHECK=1")
	}
	return env
}
func (a *app) agentCommand(tool string, args ...string) (*exec.Cmd, error) {
	path, argv, err := a.toolCommand(tool, args)
	if err != nil {
		return nil, err
	}
	return exec.Command(path, argv...), nil
}

// Native Claude Code owns its subscription credentials. The portable record
// contains only the account type, or a user owned API key for Claude Code.
type claudeAccount struct {
	Type string `json:"type"`
	Key  string `json:"key,omitempty"`
}

func (c claudeAccount) valid() bool {
	return c.Type == "native" && c.Key == "" || c.Type == "api" && c.Key != "" && printable(c.Key, 8192)
}

func (a *app) readClaudeAccount(m member) (claudeAccount, string, error) {
	var account claudeAccount
	dir, err := a.codingDir(m)
	if err != nil {
		return account, "", err
	}
	path := filepath.Join(dir, credentialFiles["claude"])
	if err = noSymlinks(a.root, path); err != nil {
		return account, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &account) != nil || !account.valid() {
		return account, "", errors.New("reconnect the claude account in edit")
	}
	return account, filepath.Join(dir, account.Type), nil
}

// A portable subscription record survives import even when Claude needs a
// fresh browser login on this machine. Let its native client report that state.
func (a *app) claudeHealth(m member) string {
	unlock, err := a.nativeLoginLock(m, true)
	if err != nil {
		return "Could not check"
	}
	defer unlock()
	record, profile, err := a.readClaudeAccount(m)
	if err != nil {
		return "Could not check"
	}
	if record.Type == "api" {
		return "Ready"
	}
	if err = noSymlinks(a.root, profile); err != nil {
		return "Could not check"
	}
	if _, err = os.Stat(profile); os.IsNotExist(err) {
		return "Sign in again"
	}
	path, args, err := a.toolCommand("claude", []string{"auth", "status", "--json"})
	if err != nil {
		return "Tool not ready"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = profile, a.codingEnv(m, profile, profile)
	data, err := cmd.Output()
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(data, &status) == nil && status.LoggedIn != nil {
		if !*status.LoggedIn {
			return "Sign in again"
		}
		if err == nil {
			return "Ready"
		}
	}
	return "Could not check"
}

type endpointLogin struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

func (e endpointLogin) valid() bool {
	u, err := url.Parse(e.URL)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && printable(e.URL, 2048) && printable(e.Key, 8192)
}

func (e endpointLogin) authorization() string {
	if e.Key == "" {
		return ""
	}
	return "Bearer " + e.Key
}

type endpointModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Reasoning        bool   `json:"reasoning"`
	ContextWindow    int    `json:"contextWindow,omitempty"`
	MaxTokens        int    `json:"maxTokens,omitempty"`
	Compat           object `json:"compat,omitempty"`
	ThinkingLevelMap object `json:"thinkingLevelMap,omitempty"`

	// A loaded llama.cpp model can report its template without waking a model.
	inspectTemplate bool
}

// llama.cpp reports the active slot in meta.n_ctx. An unloaded router preset
// publishes its arguments while context_length can still be the training limit.
func llamaSlotContext(runtime int, args []string) int {
	if runtime > 0 {
		return runtime
	}
	if limit := tokenArgument(args, "--kv-unified-per-slot"); limit > 0 {
		return limit
	}
	ctx := tokenArgument(args, "--ctx-size", "-c", "-ctx")
	slots := tokenArgument(args, "--parallel", "-np")
	if slots > 1 && !contains(args, "--kv-unified") && !contains(args, "-kvu") {
		return ctx / slots
	}
	return ctx
}

func tokenArgument(args []string, flags ...string) int {
	value, err := strconv.ParseInt(argument(args, flags...), 10, 32)
	if err != nil || value < 1 {
		return 0
	}
	return int(value)
}

func (e endpointLogin) models() (map[string]*endpointModel, error) {
	data, err := requestService("GET", strings.TrimRight(e.URL, "/")+"/models", e.authorization(), nil)
	if err != nil {
		return nil, err
	}
	var response struct{ Data []json.RawMessage }
	if json.Unmarshal(data, &response) != nil {
		return nil, errors.New("invalid model list")
	}
	models := map[string]*endpointModel{}
	for _, entry := range response.Data {
		var row struct {
			ID                string
			Owner             string   `json:"owned_by"`
			Context           int      `json:"context_length"`
			MaxModelLen       int      `json:"max_model_len"`
			Output            int      `json:"max_output_tokens"`
			SupportsReasoning bool     `json:"supports_reasoning"`
			Params            []string `json:"supported_parameters"`
			Top               struct {
				Context int `json:"context_length"`
				Output  int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Meta struct {
				Context int `json:"n_ctx"`
			}
			Status struct {
				Value string
				Args  []string
			}
		}
		// Optional vendor metadata can be absent or malformed on a usable endpoint.
		_ = json.Unmarshal(entry, &row)
		if row.ID == "" || !printable(row.ID, 256) || models[row.ID] != nil {
			continue
		}
		m := &endpointModel{ID: row.ID, Name: row.ID, ContextWindow: max(row.Context, 0), MaxTokens: max(row.Output, 0)}
		m.Reasoning = row.SupportsReasoning || contains(row.Params, "reasoning_effort") || contains(row.Params, "reasoning")
		if row.Top.Context > 0 {
			m.ContextWindow = row.Top.Context
		} else if row.MaxModelLen > 0 {
			m.ContextWindow = row.MaxModelLen
		}
		if row.Top.Output > 0 {
			m.MaxTokens = row.Top.Output
		}
		if row.Owner == "llamacpp" {
			m.Compat = object{
				"maxTokensField": "max_tokens", "supportsStore": false, "supportsDeveloperRole": false,
				"thinkingFormat": "qwen-chat-template", "thinkingTokenBudgetField": "thinking_budget_tokens",
			}
			m.inspectTemplate = row.Status.Value == "loaded" || (row.Status.Value == "" && row.Meta.Context > 0)
			m.ContextWindow = llamaSlotContext(row.Meta.Context, row.Status.Args)
			m.MaxTokens = tokenLimit(m.MaxTokens, tokenArgument(row.Status.Args, "--n-predict", "--predict", "-n"))
		} else {
			m.Compat = object{}
			if contains(row.Params, "max_tokens") && !contains(row.Params, "max_completion_tokens") {
				m.Compat["maxTokensField"] = "max_tokens"
			}
			if contains(row.Params, "reasoning") && !contains(row.Params, "reasoning_effort") {
				m.Compat["thinkingFormat"] = "openrouter"
			}
		}
		models[m.ID] = m
	}
	if len(models) == 0 {
		return nil, errors.New("endpoint has no models")
	}
	return models, nil
}

// Keep the tightest positive limit. Missing metadata leaves the choice to the server.
func tokenLimit(a, b int) int {
	if a <= 0 {
		return max(0, b)
	}
	if b <= 0 {
		return a
	}
	return min(a, b)
}

func (e endpointLogin) applyLlamaTemplates(models map[string]*endpointModel) {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	base := strings.TrimSuffix(strings.TrimRight(e.URL, "/"), "/v1")
	for _, model := range models {
		if !model.inspectTemplate || ctx.Err() != nil {
			continue
		}
		data, err := requestServiceContext(ctx, "GET", base+"/props?autoload=false&model="+url.QueryEscape(model.ID), e.authorization(), nil)
		var props struct {
			Template string `json:"chat_template"`
			Caps     struct {
				Levels []string `json:"reasoning_effort_levels"`
			} `json:"chat_template_caps"`
		}
		if err != nil || json.Unmarshal(data, &props) != nil {
			continue
		}
		if len(props.Caps.Levels) > 0 {
			levels := object{"off": "none"}
			for _, level := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
				if contains(props.Caps.Levels, level) {
					levels[level] = level
					model.Reasoning = true
				} else {
					levels[level] = nil
				}
			}
			model.ThinkingLevelMap = levels
			delete(model.Compat, "thinkingFormat")
		} else if strings.Contains(props.Template, "enable_thinking") {
			model.Reasoning = true
		}
	}
}

// LiteLLM exposes per-model capabilities when this key can read model info.
// Model selection still works on endpoints implementing only the OpenAI API.
func (e endpointLogin) applyModelInfo(models map[string]*endpointModel) {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	data, err := requestServiceContext(ctx, "GET", strings.TrimSuffix(strings.TrimRight(e.URL, "/"), "/v1")+"/model/info", e.authorization(), nil)
	if err != nil {
		return
	}
	var info struct {
		Data []struct {
			Name string `json:"model_name"`
			Info struct {
				Context   int      `json:"max_input_tokens"`
				Output    int      `json:"max_output_tokens"`
				Reasoning bool     `json:"supports_reasoning"`
				Params    []string `json:"supported_openai_params"`
			} `json:"model_info"`
		}
	}
	var fieldError *json.UnmarshalTypeError
	if err := json.Unmarshal(data, &info); err != nil && !errors.As(err, &fieldError) {
		return
	}
	for _, m := range info.Data {
		if model := models[m.Name]; model != nil {
			model.Reasoning = model.Reasoning || m.Info.Reasoning || contains(m.Info.Params, "reasoning_effort")
			model.ContextWindow = tokenLimit(model.ContextWindow, m.Info.Context)
			model.MaxTokens = tokenLimit(model.MaxTokens, m.Info.Output)
		}
	}
}

func (a *app) connectCoding(service, label string) (member, error) {
	reconnect := label != ""
	var err error
	if !reconnect {
		label, err = a.prompt("name", "", nil)
		if err != nil {
			return member{}, err
		}
		label = strings.ToLower(label)
	}
	if !codingLabel.MatchString(label) {
		return member{}, errors.New("use a short name such as work or personal")
	}
	m := member{Service: service, Account: label, Key: jsonKey(label)}
	dir, err := a.codingDir(m)
	if err != nil {
		return member{}, err
	}
	if reconnect {
		unlock, err := a.nativeLoginLock(m, false)
		if err != nil {
			return member{}, err
		}
		defer unlock()
	}
	_, err = os.Stat(dir)
	if err != nil && !os.IsNotExist(err) {
		return member{}, err
	}
	if err == nil && !reconnect {
		return member{}, errors.New("name already in use")
	}
	existingDir := err == nil
	// Prepare the official client before starting its login.
	if service != "litellm" {
		unlock, err := a.prepareAgent(service)
		if err != nil {
			return member{}, err
		}
		defer unlock()
	}
	if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return member{}, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".login-")
	if err != nil {
		return member{}, err
	}
	defer os.RemoveAll(stage)
	if service == "litellm" {
		base, err := a.prompt("api base url", "", nil)
		if err != nil {
			return member{}, err
		}
		secret, err := a.readHidden("api key (enter for none)")
		if err != nil {
			return member{}, err
		}
		e := endpointLogin{URL: strings.TrimRight(base, "/"), Key: string(secret)}
		if !e.valid() {
			return member{}, errors.New("enter an http or https api base url")
		}
		if _, err = e.models(); err != nil {
			return member{}, err
		}
		if err = atomicWrite(filepath.Join(stage, credentialFiles[service]), encode(e), 0600); err != nil {
			return member{}, err
		}
	} else if service == "claude" {
		var record claudeAccount
		_, err = os.Stat(filepath.Join(dir, credentialFiles[service]))
		existingRecord := !os.IsNotExist(err)
		if existingRecord {
			record, _, err = a.readClaudeAccount(m)
		} else {
			var pick int
			pick, err = a.choose("claude login", []string{"subscription", "api key"}, 0)
			record.Type = []string{"native", "api"}[pick]
		}
		if err != nil {
			return member{}, err
		}
		if record.Type == "native" {
			profile := filepath.Join(stage, "native")
			if existingDir {
				profile = filepath.Join(dir, "native")
			}
			if err = noSymlinks(a.root, profile); err != nil {
				return member{}, err
			}
			if err = os.MkdirAll(profile, 0700); err != nil {
				return member{}, err
			}
			cmd, err := a.agentCommand("claude", "auth", "login")
			if err != nil {
				return member{}, err
			}
			cmd.Env = a.codingEnv(m, profile, profile)
			cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = profile, a.in, a.out, a.errOut
			if err = cmd.Run(); err != nil {
				return member{}, errors.New("claude sign in did not finish")
			}
			if existingRecord {
				return m, nil
			}
		} else {
			key, err := a.readHidden("api key")
			if err != nil {
				return member{}, err
			}
			record.Key = string(key)
			if !record.valid() {
				return member{}, errors.New("enter a claude api key")
			}
		}
		if err = atomicWrite(filepath.Join(stage, credentialFiles[service]), encode(record), 0600); err != nil {
			return member{}, err
		}
	} else {
		cmd, err := a.agentCommand("codex", "-c", `cli_auth_credentials_store="file"`, "login")
		if err != nil {
			return member{}, err
		}
		cmd.Env = a.codingEnv(m, stage, stage)
		cmd.Dir = stage
		cmd.Stdin = a.in
		cmd.Stdout = a.out
		cmd.Stderr = a.errOut
		if err = cmd.Run(); err != nil {
			return member{}, fmt.Errorf("%s sign in did not finish", service)
		}
		if info, err := os.Stat(filepath.Join(stage, credentialFiles[service])); err != nil || info.Size() == 0 {
			return member{}, errors.New("sign in did not save a local subscription login")
		}
	}
	if existingDir {
		return m, os.Rename(filepath.Join(stage, credentialFiles[service]), filepath.Join(dir, credentialFiles[service]))
	}
	return m, os.Rename(stage, dir)
}

// Only portable login records enter an encrypted archive. Native chat history,
// local settings and tools are deliberately outside the credential format.
func codingFile(name string) bool {
	p := strings.Split(name, "/")
	return len(p) == 4 && p[0] == "coding" && codingService(p[1]) && codingLabel.MatchString(p[2]) && p[3] == credentialFiles[p[1]]
}
func (s *storage) codingState(service string, members []member) (object, error) {
	out := object{}
	for _, m := range members {
		key := m.selector()
		if m.Service != service || !codingLabel.MatchString(key) {
			return nil, errFormat
		}
		v, err := s.json(filepath.Join("coding", service, key, credentialFiles[service]), nil)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out[key] = v
		}
	}
	return out, nil
}
func validCodingRecord(service string, v any) bool {
	o := obj(v)
	switch service {
	case "codex":
		return textValue(obj(o["tokens"])["access_token"]) != ""
	case "claude":
		var account claudeAccount
		return json.Unmarshal(encode(v), &account) == nil && account.valid() && len(o) <= 2 && (len(o) == 1 || o["key"] != nil)
	case "litellm":
		var e endpointLogin
		return json.Unmarshal(encode(v), &e) == nil && e.valid()
	}
	return false
}

func (a *app) pickCodingAccount(sessionName string) (member, error) {
	admin := a.admin()
	var options []string
	var picks []member
	for _, service := range services {
		if !codingService(service) {
			continue
		}
		rows, err := admin.codingAccounts(service)
		if err != nil {
			return member{}, err
		}
		for _, r := range rows {
			picks = append(picks, r.member)
			options = append(options, agents[service]+" · "+r.Account)
		}
	}
	for _, service := range services {
		if !codingService(service) {
			continue
		}
		picks = append(picks, member{Service: service})
		options = append(options, "+ "+serviceName(service))
	}
	i, err := admin.choose("agent · "+sessionName, options, 0)
	if err != nil {
		return member{}, err
	}
	m := picks[i]
	if m.Account != "" {
		return m, nil
	}
	unlock, err := admin.lock("native")
	if err != nil {
		return member{}, err
	}
	defer unlock()
	admin.clearHeading(serviceName(m.Service))
	return admin.connectCoding(m.Service, "")
}

// Codex follows the auth.json link when replacing refreshed credentials. Its
// native locks must also refer to the same account across session profiles.
func (a *app) linkCodexLogin(m member, home string) error {
	dir, err := a.codingDir(m)
	if err != nil {
		return err
	}
	for _, name := range []string{"auth.json", "auth.lock"} {
		target := filepath.Join(dir, name)
		if err = noSymlinks(a.root, target); err != nil {
			return err
		}
		if name == "auth.json" {
			if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return errors.New("codex login missing; reconnect it in edit")
			}
		}
		link := filepath.Join(home, name)
		relative, err := filepath.Rel(home, target)
		if err != nil {
			return err
		}
		if err = os.Symlink(relative, link); err != nil {
			if existing, readErr := os.Readlink(link); !os.IsExist(err) || readErr != nil || existing != relative {
				return err
			}
		}
	}
	return nil
}

// The native clients keep using these profiles while running. Concurrent
// chats share one login. Replacing it must wait until its last chat closes.
func (a *app) nativeLoginLock(m member, shared bool) (func(), error) {
	if m.Service != "codex" && m.Service != "claude" {
		return func() {}, nil
	}
	if !codingLabel.MatchString(m.selector()) {
		return nil, errFormat
	}
	unlock, err := a.installationLock(m.Service+"-"+m.selector()+".login", shared, false)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("%s · %s is in use; finish that session and retry", m.Service, m.Account)
	}
	return unlock, err
}
