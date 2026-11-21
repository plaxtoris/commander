package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/term"
)

const codingConfigFile = "coding-config.json"
const maxAgentConfig = 128 << 10

type codingConfigs map[string]string

func agentSettings(data string) (map[string]json.RawMessage, error) {
	settings := map[string]json.RawMessage{}
	if data != "" && (json.Unmarshal([]byte(data), &settings) != nil || settings == nil) {
		return nil, errors.New("expected a JSON object")
	}
	return settings, nil
}

func (c codingConfigs) validate() error {
	for agent, data := range c {
		if agentService(agent) == "" || len(data) == 0 || len(data) > maxAgentConfig || !utf8.ValidString(data) {
			return errors.New("invalid coding agent configuration")
		}
		if agent == "codex" {
			var settings map[string]any
			if err := toml.Unmarshal([]byte(data), &settings); err != nil {
				return errors.New("invalid codex TOML configuration")
			}
		} else if _, err := agentSettings(data); err != nil {
			return fmt.Errorf("%s configuration: %w", agent, err)
		}
	}
	return nil
}

func (s *storage) codingConfigs() (codingConfigs, error) {
	configs := codingConfigs{}
	data, err := s.read(codingConfigFile)
	if err != nil {
		return nil, err
	}
	if data != nil && (json.Unmarshal(data, &configs) != nil || configs == nil) {
		return nil, errors.New("invalid saved coding agent configuration")
	}
	return configs, configs.validate()
}

func (a *app) codingConfig(args []string) error {
	if len(args) < 1 || len(args) > 2 || agentService(args[0]) == "" {
		return errors.New("usage: commander config codex|claude|pi [FILE|-]")
	}
	agent := args[0]
	store := newStorage(a.root)
	if len(args) == 1 {
		configs, err := store.codingConfigs()
		if err != nil {
			return err
		}
		_, err = io.WriteString(a.out, configs[agent])
		return err
	}
	var input io.Reader = a.in
	if args[1] != "-" {
		file, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	value, err := readAgentConfig(input)
	if err != nil {
		return err
	}
	return a.saveAgentConfig(agent, value, nil)
}

func readAgentConfig(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxAgentConfig+1))
	if err == nil && len(data) > maxAgentConfig {
		err = errors.New("coding agent configuration exceeds 128 KiB")
	}
	return string(data), err
}

func (a *app) saveAgentConfig(agent, value string, previous *string) error {
	if value != "" {
		if err := (codingConfigs{agent: value}).validate(); err != nil {
			return err
		}
	}
	var err error
	dir := filepath.Join(a.root, ".secrets")
	if err = noSymlinks(a.root, dir); err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	unlock, err := a.waitLock("coding-config")
	if err != nil {
		return err
	}
	defer unlock()
	configs, err := newStorage(a.root).codingConfigs()
	if err != nil {
		return err
	}
	if configs[agent] == value {
		return nil
	}
	if previous != nil && configs[agent] != *previous {
		return fmt.Errorf("%s settings changed while the agent was running; reapply your latest changes", agent)
	}
	if value == "" {
		delete(configs, agent)
	} else {
		configs[agent] = value
	}
	path := filepath.Join(dir, codingConfigFile)
	if len(configs) != 0 {
		return atomicWrite(path, encode(configs), 0600)
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (a *app) choose(title string, labels []string, selected int) (int, error) {
	rows := make([]row, len(labels))
	for i, label := range labels {
		rows[i] = row{pick: true, line: uiLine{{label, ""}}}
	}
	for {
		key, i, err := a.list(view{title: title, keys: "[enter] select  [esc] back", rows: rows}, selected)
		if err != nil {
			return 0, err
		}
		selected = i
		switch key {
		case "enter":
			return i, nil
		case "esc", "q":
			return 0, errCancelled
		}
	}
}

func workspaceName(name string) bool {
	return name != "" && !strings.Contains(name, "/") && !strings.HasPrefix(name, ".")
}

func (a *app) codingWorkspace(s session) (string, error) {
	if !workspaceName(s.Name) {
		return "", errors.New("rename the session before starting an agent: use a name without a slash or leading dot")
	}
	cwd := filepath.Join(a.root, "workspaces", s.Name)
	if err := noSymlinks(a.root, cwd); err != nil {
		return "", err
	}
	return cwd, os.MkdirAll(cwd, 0700)
}

func (a *app) code(s session) error {
	var choices []member
	var labels []string
	for _, m := range s.Accounts {
		if agent := agents[m.Service]; agent != "" {
			choices = append(choices, m)
			labels = append(labels, agent+" · "+m.Account)
		}
	}
	var selected member
	switch len(choices) {
	case 0:
		m, err := a.pickCodingAccount(s.Name)
		if err != nil {
			return err
		}
		if err := a.updateSession(s.ID, func(current *session) error {
			for _, existing := range current.Accounts {
				if existing.id() == m.id() {
					return nil
				}
			}
			current.Accounts = append(current.Accounts, m)
			return nil
		}); err != nil {
			return err
		}
		selected = m
	case 1:
		selected = choices[0]
	default:
		i, err := a.choose("start · "+s.Name, labels, 0)
		if err != nil {
			return err
		}
		selected = choices[i]
	}
	unlock, err := a.sessionLock(s.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := a.readSession(s.ID)
	if err != nil {
		return err
	}
	scoped := *a
	scoped.account, scoped.session = "", &current
	selected, err = scoped.sessionAccount(selected.Service, selected.ref())
	if err != nil {
		return err
	}
	if err = scoped.sessionWrappers(); err != nil {
		return err
	}
	a.rememberSession(s.ID)
	a.clear()
	return scoped.runAgent(selected, nil)
}

func (a *app) runAgent(m member, args []string) error {
	cwd, err := a.codingWorkspace(*a.session)
	if err != nil {
		return err
	}
	unlock, err := a.prepareAgent(m.Service)
	if err != nil {
		return err
	}
	defer unlock()
	// Codex requires a private home outside the system temp directory for
	// its command aliases. Use the same private run area as scoped tools.
	home, err := a.privateRuntimeDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	release, err := a.nativeLoginLock(m, true)
	if err != nil {
		return err
	}
	defer release()
	configs, err := newStorage(a.root).codingConfigs()
	if err != nil {
		return err
	}
	agent := agents[m.Service]
	config := configs[agent]
	cmd, err := a.codingCommand(m, cwd, home, args, config)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "settings.json")
	switch agent {
	case "codex":
		path = filepath.Join(home, "config.toml")
	case "claude":
		for _, env := range cmd.Env {
			if dir, ok := strings.CutPrefix(env, "CLAUDE_CONFIG_DIR="); ok {
				path = filepath.Join(dir, "settings.json")
				break
			}
		}
	}
	before, err := a.readNativeSettings(path)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, a.out, a.errOut
	// Both processes share the foreground terminal. Let the native agent
	// handle ctrl+c and return to Commander when it exits.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	if a.terminal() {
		if state, err := term.GetState(int(a.in.Fd())); err == nil {
			defer term.Restore(int(a.in.Fd()), state)
		}
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	err = cmd.Wait()
	saveErr := a.collectAgentConfig(agent, path, before, config)
	var exit *exec.ExitError
	if len(args) == 0 && a.terminal() && errors.As(err, &exit) {
		if exit.ExitCode() == 130 || exit.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT {
			err = nil
		}
	}
	return errors.Join(err, saveErr)
}

func (a *app) readNativeSettings(path string) (string, error) {
	if err := noSymlinks(a.root, path); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("expected a regular agent settings file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readAgentConfig(file)
}

func (a *app) collectAgentConfig(agent, path, before, shared string) error {
	after, err := a.readNativeSettings(path)
	if err != nil || after == before {
		return err
	}
	if agent == "claude" {
		after, err = applyClaudeSettings(shared, before, after)
		if err != nil {
			return err
		}
	}
	return a.saveAgentConfig(agent, after, &shared)
}

// Claude writes user settings in its account profile. Carry over only the
// preferences it changed during the run.
func applyClaudeSettings(shared, before, after string) (string, error) {
	settings, err := agentSettings(shared)
	if err != nil {
		return "", err
	}
	old, err := agentSettings(before)
	if err != nil {
		return "", err
	}
	current, err := agentSettings(after)
	if err != nil {
		return "", err
	}
	changed := false
	for key, value := range current {
		if !bytes.Equal(old[key], value) {
			settings[key] = value
			changed = true
		}
	}
	for key := range old {
		if _, ok := current[key]; !ok {
			delete(settings, key)
			changed = true
		}
	}
	if !changed {
		return shared, nil
	}
	return string(encode(settings)), nil
}

func (a *app) codingCommand(m member, cwd, home string, extra []string, config string) (*exec.Cmd, error) {
	profile := home
	context := a.codingContext(cwd)
	var args []string
	var key string
	switch m.Service {
	case "codex":
		if config != "" {
			if err := atomicWrite(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
				return nil, err
			}
		}
		if err := a.linkCodexLogin(m, home); err != nil {
			return nil, err
		}
		if err := atomicWrite(filepath.Join(home, "AGENTS.md"), []byte(context+"\n"), 0600); err != nil {
			return nil, err
		}
		args = []string{"--dangerously-bypass-approvals-and-sandbox", "-c", `cli_auth_credentials_store="file"`}
	case "claude":
		record, native, err := a.readClaudeAccount(m)
		if err != nil {
			return nil, err
		}
		if err = noSymlinks(a.root, native); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(native, 0700); err != nil {
			return nil, err
		}
		profile = native
		if record.Type == "api" {
			key = "ANTHROPIC_API_KEY=" + record.Key
		}
		settings, err := agentSettings(config)
		if err != nil {
			return nil, err
		}
		settings["cleanupPeriodDays"] = json.RawMessage("0")
		settings["autoMemoryEnabled"] = json.RawMessage("false")
		args = []string{"--dangerously-skip-permissions", "--setting-sources", "project,local", "--settings", strings.TrimSpace(string(encode(settings))), "--append-system-prompt", context}
	case "litellm":
		dir, err := a.codingDir(m)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, credentialFiles[m.Service])
		if err = noSymlinks(a.root, path); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var endpoint endpointLogin
		if json.Unmarshal(data, &endpoint) != nil || !endpoint.valid() {
			return nil, errors.New("invalid endpoint account")
		}
		ids, err := endpoint.models()
		if err != nil {
			return nil, err
		}
		model := ids[0]
		if config != "" {
			settings, err := agentSettings(config)
			if err != nil {
				return nil, err
			}
			var preferred string
			if json.Unmarshal(settings["defaultModel"], &preferred) == nil && contains(ids, preferred) {
				model = preferred
			}
			if err = atomicWrite(filepath.Join(home, "settings.json"), []byte(config), 0600); err != nil {
				return nil, err
			}
		}
		caps := endpoint.reasoning()
		models := make([]object, 0, len(ids))
		for _, id := range ids {
			models = append(models, object{"id": id, "name": id, "reasoning": caps[id]})
		}
		const provider = "openai-compatible"
		config := object{"api": "openai-completions", "baseUrl": endpoint.URL, "apiKey": "$COMMANDER_ENDPOINT_KEY", "models": models}
		if err = atomicWrite(filepath.Join(home, "models.json"), encode(object{"providers": object{provider: config}}), 0600); err != nil {
			return nil, err
		}
		// Pi installs its native search tools on demand. Keep only these
		// binaries across launches, outside the disposable agent profile.
		bin := filepath.Join(a.root, "tools", ".cache", "pi-bin")
		if err = noSymlinks(a.root, bin); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(bin, 0700); err != nil {
			return nil, err
		}
		if err = os.Symlink(bin, filepath.Join(home, "bin")); err != nil {
			return nil, err
		}
		if endpoint.Key == "" {
			endpoint.Key = "local"
		}
		key = "COMMANDER_ENDPOINT_KEY=" + endpoint.Key
		args = []string{"--no-session", "--provider", provider, "--model", model, "--models", provider + "/*", "--append-system-prompt", context}
	default:
		return nil, errors.New("unknown coding account")
	}
	cmd, err := a.agentCommand(agents[m.Service], append(args, extra...)...)
	if err != nil {
		return nil, err
	}
	cmd.Env = a.codingEnv(m, profile, home)
	if key != "" {
		cmd.Env = append(cmd.Env, key)
	}
	cmd.Dir = cwd
	return cmd, nil
}

func (a *app) codingContext(cwd string) string {
	scope := "No GitHub repositories are selected."
	if len(a.session.Repositories) > 0 {
		scope = "Work only on repositories matching this selection: " + strings.Join(a.session.Repositories, ", ") + "."
	}
	return fmt.Sprintf("Commander session %q. Your workspace is %s. %s Follow %s and use %s --session %s for Commander commands.", a.session.Name, cwd, scope, filepath.Join(a.root, "AGENTS.md"), quote(filepath.Join(a.root, "commander")), quote(a.session.Name))
}

// LiteLLM exposes per-model capabilities when this key can read model info.
// Model selection still works on endpoints implementing only the OpenAI API.
func (e endpointLogin) reasoning() map[string]bool {
	caps := map[string]bool{}
	auth := ""
	if e.Key != "" {
		auth = "Bearer " + e.Key
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	data, err := requestServiceContext(ctx, "GET", strings.TrimSuffix(strings.TrimRight(e.URL, "/"), "/v1")+"/model/info", auth, nil)
	if err != nil {
		return caps
	}
	var info struct {
		Data []struct {
			Name string `json:"model_name"`
			Info struct {
				Reasoning bool     `json:"supports_reasoning"`
				Params    []string `json:"supported_openai_params"`
			} `json:"model_info"`
		}
	}
	if json.Unmarshal(data, &info) == nil {
		for _, m := range info.Data {
			caps[m.Name] = m.Info.Reasoning || contains(m.Info.Params, "reasoning_effort")
		}
	}
	return caps
}
