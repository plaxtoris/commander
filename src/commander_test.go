package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func testApp(t *testing.T) *app {
	t.Helper()
	a, err := newApp(t.TempDir(), os.Stdin, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ensure(); err != nil {
		t.Fatal(err)
	}
	return a
}
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func addSession(t *testing.T, a *app, name string, members ...member) session {
	t.Helper()
	s, err := a.newSession(name, members, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func useSession(t *testing.T, a *app, name string, members ...member) {
	t.Helper()
	s := addSession(t, a, name, members...)
	a.session = &s
}
func ghMember(user string) member {
	return member{Service: "github", Key: jsonKey([]string{"github.com", user}), Account: user + " @ github.com", Context: ""}
}
func hMember(name string) member {
	return member{Service: "hetzner", Key: jsonKey(name), Account: name, Context: "Project"}
}

func TestEnsureAndRelocation(t *testing.T) {
	a := testApp(t)
	// A user initializing Git in the installation must not stage native logins.
	cmd := exec.Command("git", "init", "--quiet", a.root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	cmd = exec.Command("git", "-C", a.root, "check-ignore", ".secrets/.config/gh/hosts.yml", "workspaces/project/.env")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private installation files are not ignored: %v %s", err, output)
	}
	addSession(t, a, "work", hMember("project"))
	mustWrite(t, filepath.Join(a.root, "AGENTS.md"), []byte("my instructions\n"))
	if err := a.ensure(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(a.root, "AGENTS.md"))
	if string(data) != "my instructions\n" {
		t.Fatal("instructions overwritten outside startup or setup")
	}
	for path, mode := range map[string]os.FileMode{".secrets": 0700, ".sessions": 0700} {
		info, err := os.Stat(filepath.Join(a.root, path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s permissions: %v", path, err)
		}
	}
	newRoot := filepath.Join(t.TempDir(), "moved commander")
	if err := os.Rename(a.root, newRoot); err != nil {
		t.Fatal(err)
	}
	b, err := newApp(newRoot, os.Stdin, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.findSession("work")
	if err != nil || len(s.Accounts) != 1 {
		t.Fatal(s, err)
	}
	env := strings.Join(b.environment("gh", nil), "\n")
	if strings.Contains(env, a.root+"/.secrets") || !strings.Contains(env, newRoot+"/.secrets") {
		t.Fatal("relocation retained old native paths")
	}
}

func TestSymlinkRejection(t *testing.T) {
	for _, name := range []string{".secrets", ".sessions", "AGENTS.md"} {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			os.RemoveAll(filepath.Join(a.root, name))
			target := t.TempDir()
			if name == "AGENTS.md" {
				target = filepath.Join(target, "instructions")
				mustWrite(t, target, []byte("external instructions\n"))
			}
			if err := os.Symlink(target, filepath.Join(a.root, name)); err != nil {
				t.Fatal(err)
			}
			err := a.ensure()
			if name == "AGENTS.md" {
				err = a.setup(false)
			}
			if err == nil {
				t.Fatal("accepted symlink")
			}
			if name == "AGENTS.md" {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "external instructions\n" {
					t.Fatal("changed symlink target", err)
				}
			}
		})
	}
}

func TestArchiveSelectionValidation(t *testing.T) {
	good := selections{Version: 1, Profiles: map[string][]member{"work": {ghMember("user")}}}
	if err := validateSelections(good); err != nil {
		t.Fatal(err)
	}
	for _, p := range []selections{
		{Version: 2, Profiles: map[string][]member{}},
		{Version: 1, Profiles: map[string][]member{"bad\nname": {}}},
		{Version: 1, Profiles: map[string][]member{"work": nil}},
		{Version: 1, Profiles: map[string][]member{"work": {ghMember("user"), ghMember("user")}}},
		{Version: 1, Profiles: map[string][]member{"work": {}}, Scopes: map[string][]string{"work": {"../*"}}},
	} {
		if validateSelections(p) == nil {
			t.Errorf("accepted invalid selection: %+v", p)
		}
	}
}

func TestContextScopeAndIdentity(t *testing.T) {
	a := testApp(t)
	work := addSession(t, a, "work", ghMember("allowed"))
	calls := 0
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		calls++
		if tool != "gh" || !reflect.DeepEqual(args, []string{"auth", "status", "--json", "hosts"}) {
			t.Fatal(tool, args)
		}
		return []byte(`{"hosts":{"github.com":[{"login":"allowed","active":false,"state":"success","oauth_token":"do-not-print"},{"login":"other","active":true,"state":"success"}]}}`), nil
	}
	if err := a.describe("work", true); err != nil {
		t.Fatal(err)
	}
	output := a.out.(*bytes.Buffer).String()
	if calls != 1 || strings.Contains(output, "other") || strings.Contains(output, "do-not-print") || !strings.Contains(output, "selector: 'allowed'") {
		t.Fatal(output)
	}
	a.out = &bytes.Buffer{}
	if err := a.describe("missing", true); err == nil || calls != 1 {
		t.Fatal("missing session queried native accounts")
	}
	addSession(t, a, "empty")
	if err := a.describe("empty", true); err == nil || calls != 1 {
		t.Fatal("empty session queried native accounts")
	}
	if err := a.describe("", true); err == nil || calls != 1 {
		t.Fatal("context without a session queried native accounts")
	}
	work.Accounts = []member{ghMember("missing")}
	if err := a.saveSession(work); err != nil {
		t.Fatal(err)
	}
	a.out = &bytes.Buffer{}
	if err := a.describe("work", true); err == nil || a.out.(*bytes.Buffer).Len() != 0 {
		t.Fatal("printed partial invalid context")
	}
}

func TestAccountsEveryProviderAndNoSecretOutput(t *testing.T) {
	fixtures := map[string]string{
		"az":     `[{"id":"sub","name":"Production","user":{"name":"alice"},"isDefault":true,"token":"hidden"}]`,
		"m365":   `[{"name":"connection","connectedAs":"alice","appTenant":"tenant","active":true,"accessToken":"hidden"}]`,
		"hcloud": `[{"name":"project","active":true,"token":"hidden"}]`,
		"gcloud": `[{"account":"alice","status":"ACTIVE","token":"hidden"}]`,
		"gh":     `{"hosts":{"github.com":[{"login":"alice","state":"success","active":false},{"login":"alice","state":"success","active":true}]}}`,
	}
	a := testApp(t)
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		return []byte(fixtures[tool]), nil
	}
	for _, service := range services {
		if tokenService(service) {
			continue
		} // Built-in providers have HTTP fixture tests.
		rows, err := a.accounts(service)
		if err != nil || len(rows) != 1 || rows[0].State != "Ready" {
			t.Fatal(service, rows, err)
		}
		if strings.Contains(string(encode(rows)), "hidden") {
			t.Fatal("leaked native field")
		}
	}
}

func TestEnvironmentIsolation(t *testing.T) {
	a := testApp(t)
	t.Setenv("GH_TOKEN", "wrong")
	t.Setenv("HCLOUD_TOKEN", "wrong")
	t.Setenv("GH_CONFIG_DIR", "/wrong")
	t.Setenv("AZURE_CONFIG_DIR", "/wrong")
	t.Setenv("CLIMICROSOFT365_APPID", "wrong")
	t.Setenv("PYTHONPATH", "/wrong")
	envMap := func(tool string) map[string]string {
		out := map[string]string{}
		for _, line := range a.environment(tool, nil) {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
		return out
	}
	gh := envMap("gh")
	for _, key := range []string{"GH_TOKEN", "HCLOUD_TOKEN", "CLIMICROSOFT365_APPID", "PYTHONPATH"} {
		if _, ok := gh[key]; ok {
			t.Fatal("inherited", key)
		}
	}
	if gh["HOME"] != os.Getenv("HOME") || gh["GH_CONFIG_DIR"] != filepath.Join(a.root, ".secrets", ".config", "gh") {
		t.Fatal("wrong native home")
	}
	office := envMap("m365")
	if office["HOME"] != filepath.Join(a.root, ".secrets") {
		t.Fatal("Office storage not isolated")
	}
	if gh["CODEX_HOME"] != os.Getenv("CODEX_HOME") {
		t.Fatal("changed agent home")
	}
}

func TestNativeHelper(t *testing.T) {
	if os.Getenv("COMMANDER_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, s := range args {
		if s == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 0 && args[0] == "fail" {
		os.Exit(37)
	}
	json.NewEncoder(os.Stdout).Encode(args)
	os.Exit(0)
}

func TestExecPreservesArgumentsAndExitStatus(t *testing.T) {
	a := testApp(t)
	exe, _ := os.Executable()
	t.Setenv("COMMANDER_TEST_HELPER", "1")
	args := []string{"a b", "$(never-run)", "`never-run`", "line\nbreak", "--flag", "", "quote'\""}
	command := append([]string{"exec", "--", exe, "-test.run=TestNativeHelper", "--"}, args...)
	if err := a.command(command); err != nil {
		t.Fatal(err)
	}
	var got []string
	if json.Unmarshal(a.out.(*bytes.Buffer).Bytes(), &got) != nil || !reflect.DeepEqual(got, args) {
		t.Fatal(got)
	}
	err := a.command([]string{"exec", "--", exe, "-test.run=TestNativeHelper", "--", "fail"})
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 37 {
		t.Fatal(err)
	}
}

func TestLocksAndProjectBoundary(t *testing.T) {
	a := testApp(t)
	unlock, err := a.lock("native")
	if err != nil {
		t.Fatal(err)
	}
	if release, err := a.lock("native"); err == nil {
		release()
		t.Fatal("accepted concurrent lock")
	}
	unlock()
	release, err := a.lock("native")
	if err != nil {
		t.Fatal(err)
	}
	release()
	out := t.TempDir()
	os.Symlink(out, filepath.Join(a.root, "workspaces", "outside"))
	if err := a.up("outside"); err == nil {
		t.Fatal("accepted outside project")
	}
	path := filepath.Join(a.root, "workspaces", "project with spaces")
	os.MkdirAll(path, 0700)
	if resolved, err := a.projectPath("project with spaces"); err != nil || resolved != path {
		t.Fatal(resolved, err)
	}
	if _, err := a.projectPath("outside"); err == nil {
		t.Fatal("accepted outside project")
	}

}

func tarBytes(t *testing.T, entries []*tar.Header, contents []string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for i, header := range entries {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if i < len(contents) && header.Size > 0 {
			if _, err := io.WriteString(tw, contents[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buffer.Bytes()
}

func TestToolDownloadAndAtomicInstall(t *testing.T) {
	a := testApp(t)
	payload := tarBytes(t, []*tar.Header{{Name: "release/bin/tool", Typeflag: tar.TypeReg, Mode: 0755, Size: 4}}, []string{"test"})
	sum := sha256.Sum256(payload)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write(payload) }))
	defer server.Close()
	spec := download{Version: "test", URL: server.URL, SHA256: hex.EncodeToString(sum[:]), Strip: 1}
	if err := a.installArchive("fixture", spec, false); err != nil {
		t.Fatal(err)
	}
	if err := a.installArchive("fixture", spec, false); err != nil || requests != 1 {
		t.Fatal("setup was not idempotent", err, requests)
	}
	spec.SHA256 = strings.Repeat("0", 64)
	if err := a.installArchive("fixture", spec, false); err == nil {
		t.Fatal("accepted corrupt download")
	}
	data, _ := os.ReadFile(filepath.Join(a.root, "tools", "fixture", "bin", "tool"))
	if string(data) != "test" {
		t.Fatal("failed download damaged working tool")
	}
}

func TestUnsafeToolArchives(t *testing.T) {
	cases := [][]*tar.Header{
		{{Name: "../outside", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "/outside", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "directory"}, {Name: "link/file", Typeflag: tar.TypeReg, Size: 1}},
	}
	for i, headers := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(t.TempDir(), "archive")
			mustWrite(t, archive, tarBytes(t, headers, []string{"x", "x"}))
			if err := extract(archive, root, 0); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}

func TestCatalogAndGeneratedInstructions(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"gh", "hcloud", "node", "uv", "gcloud", "docker", "compose"} {
		asset := a.catalog.Archives[name]
		if len(asset.SHA256) != 64 || !strings.HasPrefix(asset.URL, "https://") || asset.Version == "" {
			t.Fatal(name)
		}
	}
	data := []byte(agentInstructions)
	for _, wanted := range []string{"./commander context", "./commander exec --", "workspaces/", ".secrets/"} {
		if !bytes.Contains(data, []byte(wanted)) {
			t.Fatal("missing instructions:", wanted)
		}
	}
	if bytes.Contains(data, []byte("make context")) {
		t.Fatal("legacy instructions embedded")
	}
}

func TestInterruptedToolReplacementRecovers(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "tool")
	previous := destination + ".previous"
	mustWrite(t, filepath.Join(previous, ".commander-version"), []byte("previous"))
	mustWrite(t, filepath.Join(previous, "tool"), []byte("old"))
	stage := filepath.Join(root, "staged")
	mustWrite(t, filepath.Join(stage, ".commander-version"), []byte("new"))
	mustWrite(t, filepath.Join(stage, "tool"), []byte("new"))
	if err := replaceDirectory(stage, destination); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(destination, "tool"))
	if string(data) != "new" {
		t.Fatal("update did not recover")
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatal("backup retained")
	}
}

func TestUIHelper(t *testing.T) {
	if os.Getenv("COMMANDER_UI_FIXTURE") == "" {
		return
	}
	a, err := newApp(os.Getenv("COMMANDER_UI_FIXTURE"), os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		panic(err)
	}
	if err = a.ensure(); err != nil {
		panic(err)
	}
	if os.Getenv("COMMANDER_RESTARTED") == a.root {
		if instructions := os.Getenv("COMMANDER_UI_UPDATED_INSTRUCTIONS"); instructions != "" {
			agentInstructions = instructions
		}
	}
	if endpoint := os.Getenv("COMMANDER_UI_SERVICE_URL"); endpoint != "" {
		serviceHTTP = redirectServiceFixture(endpoint)
	}
	if url := os.Getenv("COMMANDER_UI_UPDATE_URL"); url != "" {
		releaseURL = url
		a.releaseRun = func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("private release fixture unavailable")
		}
		if url := os.Getenv("COMMANDER_UI_TOOL_URL"); url != "" {
			spec := a.catalog.Archives["hcloud"]
			spec.URL, spec.SHA256 = url, os.Getenv("COMMANDER_UI_TOOL_SUM")
			a.catalog.Archives["hcloud"] = spec
		}
	}
	created := ""
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		if tool == "git" {
			return []byte("git version fixture"), nil
		}
		if tool == "gh" && args[0] == "auth" {
			return []byte(`{"hosts":{"github.com":[{"login":"alice","state":"success","active":true}]}}`), nil
		}
		if tool == "gh" && args[0] == "api" && extra["GH_CONFIG_DIR"] != "" {
			return []byte("acme\nbad/name\n"), nil
		}
		if tool != "hcloud" {
			return nil, errors.New("fixture supports Hetzner and GitHub only")
		}
		if len(args) > 1 && args[1] == "list" {
			rows := []any{object{"name": "existing", "active": true}}
			if created != "" {
				rows = append(rows, object{"name": created, "active": true})
			}
			return json.Marshal(rows)
		}
		if len(args) > 1 && args[1] == "create" {
			if extra["HCLOUD_TOKEN"] != strings.Repeat("a", 64) {
				return nil, errors.New("incorrect hidden token")
			}
			created = args[len(args)-1]
			marker := filepath.Join(a.root, "tools", ".requested", "hcloud")
			os.MkdirAll(filepath.Dir(marker), 0700)
			os.WriteFile(marker, nil, 0600)
			return nil, nil
		}
		return nil, errors.New("unexpected native fixture operation")
	}
	if os.Getenv("COMMANDER_UI_UPDATE_URL") != "" {
		err = a.command(nil)
	} else {
		err = a.menu()
	}
	if err != nil && !errors.Is(err, errCancelled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

var terminalControls = regexp.MustCompile(`\x1b\[[?0-9;]*[A-Za-z]`)

type terminalSession struct {
	file    *os.File
	command *exec.Cmd
	chunks  chan string
	output  string
	seen    int
}

func openUI(t *testing.T, root string, size ...*pty.Winsize) *terminalSession {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=TestUIHelper")
	cmd.Env = append(os.Environ(), "COMMANDER_UI_FIXTURE="+root)
	winsize := &pty.Winsize{Rows: 32, Cols: 100}
	if len(size) > 0 {
		winsize = size[0]
	}
	f, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		t.Fatal(err)
	}
	s := &terminalSession{file: f, command: cmd, chunks: make(chan string, 100)}
	go func() {
		defer close(s.chunks)
		buffer := make([]byte, 4096)
		for {
			n, err := f.Read(buffer)
			if n > 0 {
				s.chunks <- string(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		f.Close()
	})
	return s
}
func (s *terminalSession) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case chunk, ok := <-s.chunks:
			if !ok {
				t.Fatalf("terminal closed before %q: %s", want, s.output)
			}
			s.output += chunk
			if strings.Contains(terminalControls.ReplaceAllString(s.output[s.seen:], ""), want) {
				s.seen = len(s.output)
				return
			}
		case <-deadline:
			t.Fatalf("terminal timeout before %q: %s", want, s.output)
		}
	}
}
func (s *terminalSession) send(t *testing.T, value string) {
	t.Helper()
	if _, err := s.file.WriteString(value); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveNewSessionWithHiddenLogin(t *testing.T) {
	root := t.TempDir()
	session := openUI(t, root)
	session.wait(t, "[q]uit")
	session.send(t, "n")
	session.wait(t, "name:")
	session.send(t, "work\r")
	session.wait(t, "+ add account")
	session.send(t, "a")
	session.wait(t, "google cloud")
	session.send(t, "\x1b[B\x1b[B\x1b[B\r")
	session.wait(t, "project token:")
	session.send(t, strings.Repeat("a", 64)+"\n")
	session.wait(t, "project name:")
	session.send(t, "newproject\n")
	session.wait(t, "[x] newproject")
	session.send(t, "\x1b[H\r")
	session.wait(t, "hetzner newproject")
	session.send(t, "q")
	if err := session.command.Wait(); err != nil {
		t.Fatal(err)
	}
	for chunk := range session.chunks {
		session.output += chunk
	}
	if strings.Contains(session.output, strings.Repeat("a", 64)) {
		t.Fatal("token was echoed")
	}
	a, _ := newApp(root, os.Stdin, io.Discard, io.Discard)
	s, err := a.findSession("work")
	if err != nil || len(s.Accounts) != 1 || s.Accounts[0].Account != "newproject" {
		t.Fatal("session did not keep the new login", s, err)
	}
}

func TestInteractiveCancelKeepsSession(t *testing.T) {
	a := testApp(t)
	addSession(t, a, "work")
	session := openUI(t, a.root)
	session.wait(t, "1 session")
	session.send(t, "e")
	session.wait(t, "no accounts yet")
	session.send(t, "\x1b")
	session.wait(t, "[q]uit")
	session.send(t, "d")
	session.wait(t, "delete session work?")
	session.send(t, "n")
	session.wait(t, "[q]uit")
	session.send(t, "q")
	if err := session.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if s, err := a.findSession("work"); err != nil || len(s.Accounts) != 0 {
		t.Fatal(s, err)
	}
}

func TestBrowserLoginCommandsAndOfficeCancellation(t *testing.T) {
	for _, service := range []string{"github", "azure", "gcloud"} {
		t.Run(service, func(t *testing.T) {
			a := testApp(t)
			calls := 0
			a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
				calls++
				if tool != serviceTools[service] || capture {
					t.Fatal("native login prompts hidden")
				}
				joined := strings.Join(args, " ")
				if service == "github" && !strings.Contains(joined, "--web") {
					t.Fatal(args)
				}
				if service == "azure" && !strings.Contains(joined, "--use-device-code") {
					t.Fatal(args)
				}
				if service == "gcloud" && !strings.Contains(joined, "--no-launch-browser") {
					t.Fatal(args)
				}
				return nil, nil
			}
			if err := a.login(service); err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("office-", cancelled), func(t *testing.T) {
			a := testApp(t)
			appReady := false
			userLogins := 0
			removed := false
			a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if strings.HasPrefix(joined, "cli config get") {
					if appReady {
						return []byte("fixture-app"), nil
					}
					return nil, nil
				}
				if strings.HasPrefix(joined, "cli app add") {
					if !cancelled {
						appReady = true
					}
				}
				if strings.HasPrefix(joined, "connection remove") {
					removed = true
				}
				if strings.HasPrefix(joined, "login") && !strings.Contains(joined, "--appId") {
					userLogins++
					if !strings.Contains(joined, "--output text") {
						t.Fatal("device code output hidden")
					}
				}
				return nil, nil
			}
			err := a.login("office")
			if cancelled {
				if err == nil || userLogins != 0 {
					t.Fatal("continued after cancelled organization setup")
				}
			} else if err != nil || userLogins != 1 {
				t.Fatal(err, userLogins)
			}
			if !removed {
				t.Fatal("bootstrap connection retained")
			}
		})
	}
}

func TestRepositoryInit(t *testing.T) {
	script, err := os.ReadFile("../gitinit.sh")
	if os.IsNotExist(err) {
		t.Skip("local repository reset helper is not distributed")
	}
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint("rejected-push-", rejected), func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "working copy")
			remote := filepath.Join(root, "remote.git")
			bin := filepath.Join(root, "bin")
			log := filepath.Join(root, "api.log")
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("COMMANDER_RESET_LOG", log)
			t.Setenv("COMMANDER_RESET_REMOTE", remote)
			t.Setenv("COMMANDER_RESET_GIT", realGit)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "--quiet", "--initial-branch=main", work)
			git("init", "--quiet", "--bare", "--initial-branch=main", remote)
			git("-C", work, "config", "user.name", "Fixture")
			git("-C", work, "config", "user.email", "fixture@example.invalid")
			mustWrite(t, filepath.Join(work, "gitinit.sh"), script)
			mustWrite(t, filepath.Join(work, ".gitignore"), []byte(".secrets/\ngitinit.sh\n"))
			mustWrite(t, filepath.Join(work, "content"), []byte("first\n"))
			git("-C", work, "add", ".")
			git("-C", work, "commit", "--quiet", "-m", "first")
			git("-C", work, "branch", "old-branch")
			git("-C", work, "tag", "old-tag")
			mustWrite(t, filepath.Join(work, "content"), []byte("second\n"))
			git("-C", work, "commit", "--quiet", "-am", "second")
			previous := git("-C", work, "rev-parse", "HEAD")
			git("-C", work, "tag", "latest")
			const url = "https://github.com/fixture/repo.git"
			git("-C", work, "remote", "add", "origin", url)
			git("-C", work, "config", "url.file://"+remote+".insteadOf", url)
			git("-C", work, "push", "--quiet", "origin", "--all")
			git("-C", work, "push", "--quiet", "origin", "--tags")
			mustWrite(t, filepath.Join(work, "content"), []byte("stashed\n"))
			git("-C", work, "stash", "push", "--quiet")
			mustWrite(t, filepath.Join(work, "content"), []byte("current\n"))
			mustWrite(t, filepath.Join(work, "new-file"), []byte("included\n"))
			mustWrite(t, filepath.Join(work, ".secrets", "fixture"), []byte("private fixture\n"))
			mustWrite(t, filepath.Join(bin, "gh"), []byte(`#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$COMMANDER_RESET_LOG"
[[ $1 == api && $2 == --hostname && $3 == github.com ]]
shift 3
case "$*" in
  'user --jq .login') echo fixture ;;
  'repos/fixture/repo --jq .default_branch') echo main ;;
  --paginate*)
    if [[ $(git --git-dir="$COMMANDER_RESET_REMOTE" log -1 --format=%s main) == init ]]; then
      commit=$(git --git-dir="$COMMANDER_RESET_REMOTE" rev-parse main)
      [[ "$*" == *"select(.head_sha != \"$commit\")"* ]]
    fi
    printf '11\tin_progress\n12\tcompleted\n'
    ;;
  '--method POST repos/fixture/repo/actions/runs/11/cancel') ;;
  'repos/fixture/repo/actions/runs/11 --jq .status') echo completed ;;
  '--method DELETE repos/fixture/repo/actions/runs/11'|'--method DELETE repos/fixture/repo/actions/runs/12')
    [[ $(git --git-dir="$COMMANDER_RESET_REMOTE" log -1 --format=%s main) == init ]]
    ;;
  *) printf 'Unexpected fixture API call: %s\n' "$*" >&2; exit 1 ;;
esac
`))
			if err := os.Chmod(filepath.Join(bin, "gh"), 0755); err != nil {
				t.Fatal(err)
			}
			// Newer Git also updates remote tracking refs when pushing by URL.
			// Reproduce that behavior even on the older development image.
			mustWrite(t, filepath.Join(bin, "git"), []byte(`#!/bin/bash
set -eu
"$COMMANDER_RESET_GIT" "$@"
for arg in "$@"; do
  if [[ $arg == push ]]; then
    "$COMMANDER_RESET_GIT" update-ref -d refs/remotes/origin/old-branch
    "$COMMANDER_RESET_GIT" update-ref refs/remotes/origin/main "$("$COMMANDER_RESET_GIT" --git-dir="$COMMANDER_RESET_REMOTE" rev-parse main)"
    break
  fi
done
`))
			if err := os.Chmod(filepath.Join(bin, "git"), 0755); err != nil {
				t.Fatal(err)
			}
			if rejected {
				hook := filepath.Join(remote, "hooks", "pre-receive")
				mustWrite(t, hook, []byte("#!/bin/sh\nexit 1\n"))
				os.Chmod(hook, 0755)
			}
			cmd := exec.Command("bash", filepath.Join(work, "gitinit.sh"))
			cmd.Dir = work
			output, err := cmd.CombinedOutput()
			calls, _ := os.ReadFile(log)
			if rejected {
				if err == nil || git("-C", work, "rev-parse", "HEAD") != previous || git("--git-dir="+remote, "rev-parse", "main") != previous {
					t.Fatalf("failed push changed history: %v\n%s", err, output)
				}
				if bytes.Contains(calls, []byte("--method DELETE")) {
					t.Fatal("deleted Actions history before push succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("gitinit.sh: %v\n%s", err, output)
			}
			if git("-C", work, "rev-list", "--all", "--count") != "1" || git("--git-dir="+remote, "rev-list", "--all", "--count") != "1" {
				t.Fatal("old history is still reachable")
			}
			if git("-C", work, "log", "--format=%s") != "init" || git("-C", work, "status", "--porcelain") != "" {
				t.Fatal("snapshot is not one clean init commit")
			}
			if git("--git-dir="+remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main\nrefs/tags/latest" {
				t.Fatal("old remote branches or tags remain")
			}
			if git("-C", work, "show", "HEAD:content") != "current" || git("-C", work, "show", "HEAD:new-file") != "included" || git("-C", work, "ls-files", ".secrets") != "" {
				t.Fatal("snapshot changed working files or included private files")
			}
			if _, err := os.Stat(filepath.Join(work, ".secrets", "fixture")); err != nil {
				t.Fatal("private files were removed", err)
			}
			for _, id := range []string{"11", "12"} {
				if !bytes.Contains(calls, []byte("--method DELETE repos/fixture/repo/actions/runs/"+id)) {
					t.Fatal("old Actions run was not deleted", id)
				}
			}
		})
	}
}
