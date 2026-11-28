package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeAgentConfig(t *testing.T, a *app, agent, data string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings")
	mustWrite(t, path, []byte(data))
	return a.codingConfig([]string{agent, path})
}

func TestSharedAgentConfig(t *testing.T) {
	a := testApp(t)
	configs := codingConfigs{
		"codex":  "model = \"shared-model\"\n",
		"claude": `{"model":"shared-model","cleanupPeriodDays":30,"autoMemoryEnabled":true}`,
		"pi":     `{"defaultModel":"shared-model"}`,
	}

	// Terminals updating different agents must preserve each other's settings.
	var writers sync.WaitGroup
	for agent, data := range configs {
		writers.Go(func() {
			if err := writeAgentConfig(t, a, agent, data); err != nil {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	for agent, want := range configs {
		terminal := *a
		var out bytes.Buffer
		terminal.out = &out
		if err := terminal.codingConfig([]string{agent}); err != nil || out.String() != want {
			t.Fatalf("%s settings = %q, want %q: %v", agent, out.String(), want, err)
		}
	}
	if err := writeAgentConfig(t, a, "claude", `["invalid"]`); err == nil {
		t.Fatal("accepted nonobject settings")
	}

	codex := member{Service: "codex", Account: "shared", Key: jsonKey("shared")}
	claude := member{Service: "claude", Account: "shared", Key: jsonKey("shared")}
	mustWrite(t, filepath.Join(a.root, ".secrets/coding/codex/shared/auth.json"), encode(object{"tokens": object{"access_token": "fixture"}}))
	mustWrite(t, filepath.Join(a.root, ".secrets/coding/claude/shared/account.json"), encode(claudeAccount{Type: "native"}))
	executableFixture(t, filepath.Join(a.packageDir("node"), "bin/node"), "exit 0")
	executableFixture(t, filepath.Join(a.packageDir("claude"), packageEntries["claude"]), "exit 0")
	sessions := []session{addSession(t, a, "first", codex, claude), addSession(t, a, "second", codex, claude)}
	for _, s := range sessions {
		home := t.TempDir()
		writers.Go(func() {
			scoped := *a
			scoped.session = &s
			dir, err := scoped.openCodexProfile(codex, configs["codex"])
			if err != nil {
				t.Error(err)
				return
			}
			defer dir.Close()
			profile := dir.Name()
			cmd, err := scoped.codingCommand(codex, home, home, profile, nil, configs["codex"])
			if err != nil {
				t.Error(err)
				return
			}
			data, err := os.ReadFile(filepath.Join(profile, "config.toml"))
			if err != nil || string(data) != configs["codex"] || !contains(cmd.Env, "COMMANDER_SESSION="+s.ID) {
				t.Errorf("incorrect codex settings or scope: %v", err)
			}
			cmd, err = scoped.codingCommand(claude, home, home, home, nil, configs["claude"])
			if err != nil {
				t.Error(err)
				return
			}
			var settings object
			err = json.Unmarshal([]byte(argument(cmd.Args, "--settings")), &settings)
			if err != nil || settings["model"] != "shared-model" || settings["autoMemoryEnabled"] != false || settings["cleanupPeriodDays"] != float64(30) || !contains(cmd.Env, "CLAUDE_CODE_PROJECT_DIR_NAME="+s.ID) || argument(cmd.Args, "--setting-sources") != "project,local" {
				t.Errorf("incorrect claude settings: %v, %v", settings, err)
			}
		})
	}
	writers.Wait()

	memory := "* Use clear labels.\n"
	mustWrite(t, filepath.Join(a.root, "MEMORY.md"), []byte(memory))
	snapshot, err := a.exportSnapshot([]string{sessions[0].ID, sessions[1].ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "settings.zip")
	if err = writeArchive(archive, snapshot, "fixture-password", false); err != nil {
		t.Fatal(err)
	}
	snapshot, err = readArchive(archive, "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	if err = writeAgentConfig(t, target, "pi", `{"old":true}`); err != nil {
		t.Fatal(err)
	}
	selection, err := target.importChoices(snapshot, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if err = target.importSelected(snapshot, selection); err != nil {
		t.Fatal(err)
	}
	for _, original := range sessions {
		restored, err := target.findSession(original.Name)
		if err != nil || len(restored.Accounts) != 2 || restored.Accounts[0].id() != original.Accounts[0].id() || restored.Accounts[1].id() != original.Accounts[1].id() {
			t.Fatalf("imported session %s: %v", original.Name, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(target.root, "MEMORY.md")); err != nil || string(got) != memory {
		t.Fatal("round trip lost memory", err)
	}
	// Imports from older Commander versions have no settings to replace.
	snapshot.CodingConfigs = nil
	if err = target.importSelected(snapshot, selection); err != nil {
		t.Fatal(err)
	}
	// A partial archive replaces only the agents it carries.
	snapshot.CodingConfigs = codingConfigs{"pi": `{"defaultModel":"new-model"}`}
	if err = target.importSelected(snapshot, selection); err != nil {
		t.Fatal(err)
	}
	configs["pi"] = snapshot.CodingConfigs["pi"]
	got, err := newStorage(target.root).codingConfigs()
	if err != nil || !bytes.Equal(encode(got), encode(configs)) {
		t.Fatalf("imported settings = %v, want %v: %v", got, configs, err)
	}
	if err = writeAgentConfig(t, target, "pi", ""); err != nil {
		t.Fatal(err)
	}
	got, err = newStorage(target.root).codingConfigs()
	if err != nil || len(got) != 2 || got["pi"] != "" {
		t.Fatalf("settings after clear = %v: %v", got, err)
	}
}

func TestNativeSettingsFollowAgentAcrossSessions(t *testing.T) {
	a := testApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"fixture"}]}`)
	}))
	defer server.Close()
	executableFixture(t, filepath.Join(a.packageDir("node"), "bin/node"), `exec /bin/sh "$@"`)
	mustWrite(t, filepath.Join(a.packageDir("node"), ".commander-version"), []byte(a.catalog.Archives["node"].SHA256))

	for _, tc := range []struct {
		service, agent, path, settings string
	}{
		{"codex", "codex", "$CODEX_HOME/config.toml", `model = "fixture"`},
		{"claude", "claude", "$CLAUDE_CONFIG_DIR/settings.json", strings.TrimSpace(string(encode(object{"model": "fixture"})))},
		{"litellm", "pi", "$PI_CODING_AGENT_DIR/settings.json", strings.TrimSpace(string(encode(object{"defaultModel": "fixture", "packages": []string{"npm:pi-token-speed"}})))},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			verify := fmt.Sprintf(`test "$(cat "%s")" = '%s'`, tc.path, tc.settings)
			if tc.agent == "claude" {
				verify = `test ! -e "$CLAUDE_CONFIG_DIR/settings.json"
	for arg in "$@"; do
		case "$arg" in *'"model": "fixture"'*) exit 0;; esac
	done
	exit 1`
			}
			script := fmt.Sprintf(`set -eu
if [ "$PWD" = "$COMMANDER_HOME/workspaces/%s-first" ]; then
	printf '%%s\n' '%s' > "%s.new"
	/bin/mv "%s.new" "%s"
else
	%s
fi`, tc.agent, tc.settings, tc.path, tc.path, tc.path, verify)
			executableFixture(t, filepath.Join(a.packageDir(tc.agent), packageEntries[tc.agent]), script)
			if tc.agent == "pi" {
				executableFixture(t, filepath.Join(a.packageDir("pi"), "pi"), "exit 0")
			}
			mustWrite(t, filepath.Join(a.packageDir(tc.agent), ".commander-version"), []byte(a.catalog.Packages[tc.agent]))

			for _, name := range []string{"first", "second"} {
				m := member{Service: tc.service, Account: name, Key: jsonKey(name)}
				dir, err := a.codingDir(m)
				if err != nil {
					t.Fatal(err)
				}
				switch tc.agent {
				case "codex":
					mustWrite(t, filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"fixture"}}`))
				case "claude":
					mustWrite(t, filepath.Join(dir, "account.json"), encode(claudeAccount{Type: "native"}))
				case "pi":
					mustWrite(t, filepath.Join(dir, "endpoint.json"), encode(endpointLogin{URL: server.URL, Key: "fixture"}))
				}
				scoped := *a
				s := addSession(t, a, tc.agent+"-"+name, m)
				scoped.session = &s
				if err := scoped.runAgent(m, nil); err != nil {
					t.Fatalf("%s session: %v", name, err)
				}
			}
			configs, err := newStorage(a.root).codingConfigs()
			if err != nil || strings.TrimSpace(configs[tc.agent]) != tc.settings {
				t.Fatalf("settings = %q: %v", configs[tc.agent], err)
			}
		})
	}
}

func TestConcurrentCodexProfiles(t *testing.T) {
	a := testApp(t)
	m := member{Service: "codex", Account: "work", Key: jsonKey("work")}
	useSession(t, a, "first", m)
	open := func(config string) *os.File {
		t.Helper()
		dir, err := a.openCodexProfile(m, config)
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	first := open("model = 'first'")
	path := filepath.Join(first.Name(), "config.toml")
	edit := "model = 'edited'"
	mustWrite(t, path, []byte(edit))
	overlap := open("model = 'shared'")
	if got, err := os.ReadFile(path); err != nil || string(got) != edit || first.Name() != overlap.Name() {
		t.Fatal("overlapping launches must share history and preserve live edits", err)
	}
	first.Close()
	overlap.Close()
	reopened := open("")
	defer reopened.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("closed profile retained cleared shared settings", err)
	}
	useSession(t, a, "second", m)
	other := open("")
	defer other.Close()
	if other.Name() == reopened.Name() {
		t.Fatal("different sessions shared a Codex profile")
	}
}

func TestNativeSettingsKeepNewerSharedConfig(t *testing.T) {
	a := testApp(t)
	old := "model = 'old'\n"
	newer := "model = 'newer'\n"
	if err := writeAgentConfig(t, a, "codex", newer); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.root, "config.toml")
	mustWrite(t, path, []byte("model = 'stale edit'\n"))
	if err := a.collectAgentConfig("codex", path, old, old); err == nil {
		t.Fatal("accepted a stale settings update")
	}
	configs, err := newStorage(a.root).codingConfigs()
	if err != nil || configs["codex"] != newer {
		t.Fatalf("settings = %q: %v", configs["codex"], err)
	}
}

func TestCodingAccountIsolationAndArchive(t *testing.T) {
	a := testApp(t)
	t.Setenv("ANTHROPIC_API_KEY", "ambient-secret")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("ANTHROPIC_MODEL", "wrong-model")
	m := member{Service: "codex", Account: "work", Key: jsonKey("work")}
	s := addSession(t, a, "team", m)
	a.session = &s
	dir, err := a.codingDir(m)
	if err != nil {
		t.Fatal(err)
	}
	login := func(token string) []byte { return encode(object{"tokens": object{"access_token": token}}) }
	src := filepath.Join(dir, "auth.json")
	mustWrite(t, src, login("original"))
	mustWrite(t, filepath.Join(a.root, ".secrets/coding/codex/unrelated/auth.json"), []byte("damaged fixture"))
	p, err := a.sessionArchive(s)
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	if err = target.importSelected(p, importSelection{Accounts: s.Accounts, Sessions: []string{s.Name}}); err != nil {
		t.Fatal(err)
	}
	if _, err = target.findSession(s.Name); err != nil {
		t.Fatal(err)
	}
	// Parallel sessions must see credentials refreshed by another session.
	first, second := t.TempDir(), t.TempDir()
	for _, home := range []string{first, second} {
		if err := a.linkCodexLogin(m, home); err != nil {
			t.Fatal(err)
		}
	}
	targetPath, err := filepath.EvalSymlinks(filepath.Join(first, "auth.json"))
	if err != nil || targetPath != src {
		t.Fatal("invalid native credential target", err)
	}
	if err = atomicWrite(targetPath, login("refreshed"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(second, "auth.json")); !bytes.Equal(b, login("refreshed")) {
		t.Fatal("parallel session missed refresh")
	}
	if err := os.WriteFile(filepath.Join(first, "auth.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	lock1, err1 := os.Stat(filepath.Join(first, "auth.lock"))
	lock2, err2 := os.Stat(filepath.Join(second, "auth.lock"))
	if err1 != nil || err2 != nil || !os.SameFile(lock1, lock2) {
		t.Fatal("parallel sessions must share the native lock")
	}
	release, err := a.nativeLoginLock(m, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.deleteLogin(m); err == nil {
		t.Fatal("removed an active native login")
	}
	if err := a.importSelected(p, importSelection{Accounts: s.Accounts, Replace: map[string]bool{m.id(): true}}); err == nil {
		t.Fatal("replaced an active native login")
	}
	release()
	if err := a.importSelected(p, importSelection{Accounts: s.Accounts, Replace: map[string]bool{m.id(): true}}); err != nil {
		t.Fatal(err)
	}
	vars := a.codingEnv(m, first, first)
	env := strings.Join(vars, "\n")
	if strings.Contains(env, "ambient-secret") || strings.Contains(env, "wrong-model") || !contains(vars, "CODEX_HOME="+first) || !strings.Contains(env, "COMMANDER_SESSION="+s.ID) {
		t.Fatal("invalid agent environment")
	}
}

func TestGitHubAccountRoutes(t *testing.T) {
	a := testApp(t)
	work, personal := ghMember("work"), ghMember("personal")
	work.Repos = []string{"org/*"}
	personal.Repos = []string{"org/private"}
	s := addSession(t, a, "team", work, personal)
	s.Repositories = []string{"org/*"}
	a.session = &s
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"pr", "list", "-R", "ORG/private"}, "personal"}, {[]string{"api", "repos/org/other/pulls"}, "work"}, {[]string{"repo", "list", "org"}, "work"}, {[]string{"api", "orgs/org/repos"}, "work"}} {
		m, err := a.selectedMember("github", tc.args)
		if err != nil || m.ref() != tc.want {
			t.Fatal(tc, err, m)
		}
	}
	if _, err := a.selectedMember("github", []string{"api", "user"}); err == nil {
		t.Fatal("guessed identity without repo")
	}
	a.account = "work"
	if m, err := a.repoMember("org/private.git"); err != nil || m.ref() != "work" {
		t.Fatal(m, err)
	}
}

func TestClaudeSubscriptionStaysInNativeClient(t *testing.T) {
	a := testApp(t)
	m := member{Service: "claude", Account: "work", Key: jsonKey("work")}
	s := addSession(t, a, "team", m)
	a.session = &s
	dir, err := a.codingDir(m)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, credentialFiles["claude"])
	mustWrite(t, marker, encode(claudeAccount{Type: "native"}))
	mustWrite(t, filepath.Join(dir, "native", ".credentials.json"), []byte("native-only-fixture"))
	p, err := a.sessionArchive(s)
	if err != nil || bytes.Contains(encode(p), []byte("native-only-fixture")) {
		t.Fatal("native credential included in backup", err)
	}
	if !validCodingRecord("claude", object{"type": "native"}) ||
		validCodingRecord("claude", object{"type": "native", "claudeAiOauth": "secret"}) {
		t.Fatal("invalid portable subscription record")
	}
	bin := filepath.Join(a.packageDir("claude"), packageEntries["claude"])
	executableFixture(t, bin, "exit 0")
	temp := t.TempDir()
	cmd, err := a.codingCommand(m, temp, temp, temp, nil, "")
	if err != nil || cmd.Path != bin || !contains(cmd.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "native")) || os.Geteuid() == 0 && argument(cmd.Args, "--permission-mode") != "auto" || os.Geteuid() != 0 && !contains(cmd.Args, "--dangerously-skip-permissions") {
		t.Fatal("subscription must launch its own native client", err)
	}
	var settings struct {
		CleanupPeriodDays int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal([]byte(argument(cmd.Args, "--settings")), &settings); err != nil || settings.CleanupPeriodDays != 36500 {
		t.Fatal("unexpected default transcript retention", settings, err)
	}
	mustWrite(t, marker, encode(claudeAccount{Type: "api", Key: "test-key"}))
	cmd, err = a.codingCommand(m, temp, temp, temp, nil, "")
	if err != nil || cmd.Path != bin || !contains(cmd.Env, "ANTHROPIC_API_KEY=test-key") || !contains(cmd.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "api")) {
		t.Fatal("API key must launch an isolated native client", err)
	}
	if err = a.execute("pi", nil); err == nil || !strings.Contains(err.Error(), "no openai-compatible account") {
		t.Fatal("pi used an unselected account", err)
	}
	a.session = nil
	if err = a.execute("codex", nil); !errors.Is(err, errNoSession) {
		t.Fatal(err)
	}
}

func TestNativeAgentLauncher(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"first", "second"} {
		mustWrite(t, filepath.Join(a.root, ".secrets/coding/codex", name, "auth.json"), encode(object{"tokens": object{"access_token": name}}))
	}
	executableFixture(t, filepath.Join(a.packageDir("node"), "bin/node"), `exec /bin/sh "$@"`)
	mustWrite(t, filepath.Join(a.packageDir("node"), ".commander-version"), []byte(a.catalog.Archives["node"].SHA256))
	fixture := `#!/bin/sh
set -eu
test -t 0 && test -t 1 && test -t 2
test "$1" = "--dangerously-bypass-approvals-and-sandbox"
test "$PWD" = "$COMMANDER_HOME/workspaces/demo"
test -s "$CODEX_HOME/AGENTS.md"
login=$(basename "$(dirname "$(readlink "$CODEX_HOME/auth.json")")")
if test -e "$CODEX_HOME/sessions/saved"; then
	test "$(cat "$CODEX_HOME/sessions/saved")" = "$login"
	printf 'previous chat %s\n' "$login"
fi
mkdir -p "$CODEX_HOME/sessions"
printf '%s\n' "$login" > "$CODEX_HOME/sessions/saved"
printf 'native login %s\n' "$login"
read -r line
`
	mustWrite(t, filepath.Join(a.packageDir("codex"), packageEntries["codex"]), []byte(fixture))
	mustWrite(t, filepath.Join(a.packageDir("codex"), ".commander-version"), []byte(a.catalog.Packages["codex"]))
	ui := openUI(t, a.root)
	ui.wait(t, "+ new session")
	ui.send(t, "\r")
	ui.wait(t, "name:")
	ui.send(t, "demo\r")
	ui.wait(t, "accounts & repos")
	ui.send(t, "\r")
	ui.wait(t, "native login first")
	ui.send(t, "exit\n")
	ui.wait(t, "[q] quit")
	// A single selected account launches immediately and can resume its chat.
	ui.send(t, "\r")
	ui.wait(t, "previous chat first")
	ui.wait(t, "native login first")
	ui.send(t, "exit\n")
	ui.wait(t, "[q] quit")
	ui.send(t, "e")
	ui.wait(t, "[ ] second")
	ui.send(t, "j ")
	ui.wait(t, "saved")
	ui.send(t, "\x1b")
	ui.wait(t, "[q] quit")
	ui.send(t, "\r")
	ui.wait(t, "start · demo")
	ui.send(t, "j\r")
	ui.wait(t, "native login second")
	ui.send(t, "exit\n")
	ui.wait(t, "[q] quit")
	ui.send(t, "d")
	ui.wait(t, "delete session demo?")
	ui.send(t, "n")
	ui.wait(t, "[q] quit")
	ui.send(t, "q")
	if err := ui.command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestActiveAgentDefersPackageUpdate(t *testing.T) {
	a := testApp(t)
	tool := "claude"
	entry := filepath.Join(a.packageDir(tool), packageEntries[tool])
	executableFixture(t, entry, "exit 0")
	mustWrite(t, filepath.Join(a.packageDir(tool), ".commander-version"), []byte("0.0.1"))
	release, err := a.agentLock(tool, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := a.installPackage(tool); err != nil {
		t.Fatal("update blocked an active agent", err)
	}
	if b, err := os.ReadFile(filepath.Join(a.packageDir(tool), ".commander-version")); err != nil || string(b) != "0.0.1" {
		t.Fatal("replaced a running agent", err)
	}
}

func TestAgentCanStartDuringPackageDownload(t *testing.T) {
	a := testApp(t)
	const tool = "claude"
	entry := packageEntries[tool]
	executableFixture(t, filepath.Join(a.packageDir(tool), entry), "exit 0")
	version := filepath.Join(a.packageDir(tool), ".commander-version")
	mustWrite(t, version, []byte("0.0.1"))
	downloading := filepath.Join(a.root, "downloading")
	finish := filepath.Join(a.root, "finish")
	executableFixture(t, filepath.Join(a.packageDir("node"), "bin/node"),
		"touch "+quote(downloading)+"\nwhile ! test -e "+quote(finish)+"; do sleep 0.02; done\n"+
			"mkdir -p \"$4/"+filepath.Dir(entry)+"\" && touch \"$4/"+entry+"\"")
	installed := make(chan error, 1)
	go func() { installed <- a.installPackage(tool) }()
	defer func() {
		mustWrite(t, finish, nil)
		if err := <-installed; err != nil {
			t.Fatal(err)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(downloading); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	running, err := a.installationLock(tool+"-tool", true, false)
	if err != nil {
		t.Fatal("download blocked agent launch", err)
	}
	t.Cleanup(running)
	t.Cleanup(func() {
		if data, err := os.ReadFile(version); err != nil || string(data) != "0.0.1" {
			t.Error("replaced agent while running", err)
		}
	})
}

func TestExistingEndpointRunsInPi(t *testing.T) {
	a := testApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unexpected endpoint login")
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"test-model"},{"id":"z-preferred"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m := member{Service: "litellm", Account: "old", Key: jsonKey("old")}
	mustWrite(t, filepath.Join(a.root, ".secrets/coding/litellm/old/endpoint.json"), encode(endpointLogin{URL: server.URL + "/v1", Key: "fixture"}))
	s := addSession(t, a, "demo", m)
	a.session = &s
	for _, alias := range []string{"openai-compatible=old", "litellm=old"} {
		ms, err := a.parseAccounts(alias)
		if err != nil || len(ms) != 1 || ms[0].id() != m.id() {
			t.Fatal(alias, ms, err)
		}
	}
	executableFixture(t, filepath.Join(a.packageDir("node"), "bin/node"), "exit 0")
	tmp := t.TempDir()
	cmd, err := a.codingCommand(m, tmp, tmp, tmp, nil, "")
	if err != nil || !contains(cmd.Args, "--approve") || !contains(cmd.Args, "--no-session") || argument(cmd.Args, "--provider") != "openai-compatible" || argument(cmd.Args, "--model") != "test-model" || !contains(cmd.Env, "COMMANDER_ENDPOINT_KEY=fixture") {
		t.Fatal("incorrect pi command", err)
	}
	b, err := os.ReadFile(filepath.Join(tmp, "models.json"))
	if err != nil || bytes.Contains(b, []byte("fixture")) || !bytes.Contains(b, []byte(server.URL)) {
		t.Fatal("invalid pi profile", err)
	}
	for _, tc := range []struct{ configured, want string }{
		{"z-preferred", "z-preferred"},
		{"unavailable", "test-model"},
	} {
		settings := fmt.Sprintf(`{"defaultModel":%q,"defaultThinkingLevel":"high"}`, tc.configured)
		if err = writeAgentConfig(t, a, "pi", settings); err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		cmd, err = a.codingCommand(m, tmp, home, home, nil, settings)
		if err != nil || argument(cmd.Args, "--model") != tc.want {
			t.Fatalf("configured %s, want %s: %v", tc.configured, tc.want, err)
		}
		want := object{"defaultModel": tc.configured, "defaultThinkingLevel": "high", "packages": []any{"npm:pi-token-speed"}}
		raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
		var got object
		if err != nil || json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("pi settings = %s: %v", raw, err)
		}
	}
	choicePath := filepath.Join(a.root, ".sessions", s.ID, "pi", m.selector()+".json")
	for _, tc := range []struct{ saved, model, thinking string }{
		{`{"model":"test-model","thinking":"high"}`, "test-model", "high"},
		{`{"model":"unavailable","thinking":"high"}`, "z-preferred", ""},
	} {
		mustWrite(t, choicePath, []byte(tc.saved))
		home := t.TempDir()
		cmd, err := a.codingCommand(m, tmp, home, home, nil, `{"defaultModel":"z-preferred"}`)
		if err != nil || argument(cmd.Args, "--model") != tc.model || argument(cmd.Args, "--thinking") != tc.thinking || !contains(cmd.Env, "COMMANDER_PI_SELECTION="+choicePath) {
			t.Fatalf("saved selection %s: %v", tc.saved, err)
		}
	}

}

func TestPiSelectionEvents(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Pi uses Node to load extensions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "choice.json")
	mustWrite(t, filepath.Join(dir, "selection.mjs"), piSelectionExtension)
	const script = `
import register from "./selection.mjs";
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
const handlers = {};
let thinking = "medium";
register({ on: (name, handler) => handlers[name] = handler, getThinkingLevel: () => thinking });
const ctx = { model: { provider: "openai-compatible", id: "first" } };
const saved = () => JSON.parse(readFileSync(process.env.COMMANDER_PI_SELECTION, "utf8"));
handlers.session_start({ reason: "startup" }, ctx);
assert.deepEqual(saved(), { model: "first", thinking: "medium" });
ctx.model.id = "second";
handlers.model_select({}, ctx);
assert.equal(saved().model, "second");
thinking = "low";
handlers.thinking_level_select({}, ctx);
ctx.model.id = "stale";
handlers.session_start({ reason: "reload" }, ctx);
handlers.session_start({ reason: "startup" }, ctx);
assert.deepEqual(saved(), { model: "second", thinking: "low" });`
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Dir = dir
	cmd.Env = []string{"COMMANDER_PI_SELECTION=" + path}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Pi selection events: %s: %v", out, err)
	}
}

func TestBundledPiTokenSpeed(t *testing.T) {
	const source = "npm:pi-token-speed"
	other := "npm:pi-token-speed-extra"
	filter := object{"source": source + "@" + piTokenSpeedVersion, "extensions": []any{"src/index.ts"}}
	for _, tc := range []struct {
		packages, want []any
		bundled        bool
	}{
		{nil, []any{source}, true},
		{[]any{other}, []any{other, source}, true},
		{[]any{other, source}, []any{other, source}, true},
		{[]any{filter}, []any{filter}, true},
		{[]any{source + "@next"}, []any{source + "@next"}, false},
	} {
		home := t.TempDir()
		settings := map[string]json.RawMessage{}
		if tc.packages != nil {
			settings["packages"] = encode(tc.packages)
		}
		if err := installPiTokenSpeed(home, settings); err != nil {
			t.Fatal(err)
		}
		var got []any
		if err := json.Unmarshal(settings["packages"], &got); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("packages = %v, want %v: %v", got, tc.want, err)
		}
		dir := filepath.Join(home, "npm", "node_modules", "pi-token-speed")
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if !tc.bundled {
			if !os.IsNotExist(err) {
				t.Fatal("seeded the bundled version over an explicit npm tag")
			}
			continue
		}
		var manifest struct {
			Name    string
			Version string
			Pi      struct{ Extensions []string }
		}
		if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Name != "pi-token-speed" || manifest.Version != piTokenSpeedVersion || len(manifest.Pi.Extensions) != 1 {
			t.Fatal("bundled extension manifest missing or invalid", err)
		}
		if _, err := os.Stat(filepath.Join(dir, manifest.Pi.Extensions[0])); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateInstalledToolsOnlyAndPreserveWorkingVersion(t *testing.T) {
	a := testApp(t)
	search := filepath.Join(a.root, "tools", ".cache", "pi-bin", "rg")
	mustWrite(t, search, []byte("shared search tool"))
	old := a.catalog.Archives["gh"]
	body := tarBytes(t, []*tar.Header{{Name: "pkg/bin/gh", Mode: 0755, Size: 3, Typeflag: tar.TypeReg}}, []string{"new"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/2.200.0") {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer server.Close()
	old.URL = server.URL + "/" + old.Version
	a.catalog.Archives["gh"] = old
	mustWrite(t, filepath.Join(a.packageDir("gh"), "bin/gh"), []byte("old"))
	os.Chmod(filepath.Join(a.packageDir("gh"), "bin/gh"), 0755)
	mustWrite(t, filepath.Join(a.packageDir("gh"), ".commander-version"), []byte(old.SHA256))
	var checks []string
	var lock sync.Mutex
	version := "2.200.0"
	a.latest = func(_ context.Context, name string, _ catalog) (toolRelease, error) {
		lock.Lock()
		defer lock.Unlock()
		checks = append(checks, name)
		return toolRelease{Version: version, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}, nil
	}
	if err := a.refreshTools(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(search); err != nil || string(b) != "shared search tool" {
		t.Fatal("updating gh removed Pi's search tool", err)
	}
	if strings.Join(checks, ",") != "gh" {
		t.Fatal(checks)
	}
	version = "2.201.0"
	a.latest = func(context.Context, string, catalog) (toolRelease, error) {
		return toolRelease{Version: version, SHA256: strings.Repeat("a", 64)}, nil
	}
	if err := a.refreshTools(); err == nil {
		t.Fatal("missing update failure")
	}
	fresh, err := newApp(a.root, os.Stdin, io.Discard, io.Discard)
	if err != nil || fresh.catalog.Archives["gh"].Version != "2.200.0" {
		t.Fatal("lost working catalog", err)
	}
	b, _ := os.ReadFile(filepath.Join(a.packageDir("gh"), "bin/gh"))
	if string(b) != "new" {
		t.Fatal("lost working binary")
	}
	if _, err = os.Stat(a.packageDir("pi")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("installed unused pi")
	}
}

func TestGcloudUpdatesUseThePublishedArchive(t *testing.T) {
	c := toolCatalog.copy()
	for _, version := range []string{"900.0.0", "901.0.0"} {
		c.apply("gcloud", toolRelease{Version: version, SHA256: strings.Repeat("a", 64)})
		spec := c.Archives["gcloud"]
		if spec.Version != version || !strings.Contains(spec.URL, "/google-cloud-cli-linux-") {
			t.Fatal("unexpected gcloud download", spec)
		}
	}
}
