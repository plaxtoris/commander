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
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
			cmd, err := scoped.codingCommand(codex, home, home, nil)
			if err != nil {
				t.Error(err)
				return
			}
			data, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil || string(data) != configs["codex"] || !contains(cmd.Env, "COMMANDER_SESSION="+s.ID) {
				t.Errorf("incorrect codex settings or scope: %v", err)
			}
			cmd, err = scoped.codingCommand(claude, home, home, nil)
			if err != nil {
				t.Error(err)
				return
			}
			var settings object
			err = json.Unmarshal([]byte(argument(cmd.Args, "--settings")), &settings)
			if err != nil || settings["model"] != "shared-model" || settings["autoMemoryEnabled"] != false || settings["cleanupPeriodDays"] != float64(0) {
				t.Errorf("incorrect claude settings: %v, %v", settings, err)
			}
		})
	}
	writers.Wait()

	snapshot, err := a.exportSnapshot(nil, false)
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
	// Parallel sessions sharing a login must get fresh histories and keep a
	// credential that was refreshed or reconnected by another session.
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
	cmd, err := a.codingCommand(m, temp, temp, nil)
	if err != nil || cmd.Path != bin || !contains(cmd.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "native")) || !contains(cmd.Args, "--dangerously-skip-permissions") {
		t.Fatal("subscription must launch its own native client", err)
	}
	mustWrite(t, marker, encode(claudeAccount{Type: "api", Key: "test-key"}))
	cmd, err = a.codingCommand(m, temp, temp, nil)
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
test ! -e "$CODEX_HOME/history"
touch "$CODEX_HOME/history"
printf 'native login %s\n' "$(basename "$(dirname "$(readlink "$CODEX_HOME/auth.json")")")"
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
	// A single selected account launches immediately, even with other saved logins.
	ui.send(t, "\r")
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
	cmd, err := a.codingCommand(m, tmp, tmp, nil)
	if err != nil || !contains(cmd.Args, "--no-session") || argument(cmd.Args, "--provider") != "openai-compatible" || argument(cmd.Args, "--model") != "test-model" || !contains(cmd.Env, "COMMANDER_ENDPOINT_KEY=fixture") {
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
		cmd, err = a.codingCommand(m, tmp, home, nil)
		if err != nil || argument(cmd.Args, "--model") != tc.want {
			t.Fatalf("configured %s, want %s: %v", tc.configured, tc.want, err)
		}
		if raw, err := os.ReadFile(filepath.Join(home, "settings.json")); err != nil || string(raw) != settings {
			t.Fatalf("pi settings = %s: %v", raw, err)
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
