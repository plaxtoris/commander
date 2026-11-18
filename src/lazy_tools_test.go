package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLazyExecRelocationAndStreams(t *testing.T) {
	a := testApp(t)
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\"\ncat\nprintf 'native stderr' >&2\nexit 37\n"
	payload := tarBytes(t, []*tar.Header{{Name: "bin/gh", Mode: 0755, Size: int64(len(script)), Typeflag: tar.TypeReg}}, []string{script})
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads++; w.Write(payload) }))
	defer server.Close()
	a.catalog.Archives = map[string]download{"gh": {Version: "fixture", URL: server.URL, SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}}
	moved := filepath.Join(t.TempDir(), "Commander moved")
	if err := os.Rename(a.root, moved); err != nil {
		t.Fatal(err)
	}
	a.root = moved
	for i := 0; i < 2; i++ {
		input := filepath.Join(t.TempDir(), "input")
		mustWrite(t, input, []byte("stdin stays intact\n"))
		f, err := os.Open(input)
		if err != nil {
			t.Fatal(err)
		}
		a.in = f
		a.out, a.errOut = &bytes.Buffer{}, &bytes.Buffer{}
		_, err = a.nativeRun("gh", []string{"a b", "$(literal)"}, false, nil)
		f.Close()
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != 37 {
			t.Fatal("exit code lost", err)
		}
		if got := a.out.(*bytes.Buffer).String(); got != "a b\n$(literal)\nstdin stays intact\n" {
			t.Fatal("polluted stdout", got)
		}
		feedback := a.errOut.(*bytes.Buffer).String()
		if !strings.HasSuffix(feedback, "native stderr") || (i == 0) != strings.Contains(feedback, "preparing github") {
			t.Fatal("incorrect feedback", feedback)
		}
	}
	if downloads != 1 {
		t.Fatal("downloaded a ready tool again", downloads)
	}
	entries, _ := os.ReadDir(filepath.Join(a.root, "tools"))
	for _, entry := range entries {
		if entry.Name() != "gh" && entry.Name() != ".requested" {
			t.Fatal("unrelated installation", entry.Name())
		}
	}
}

func executableFixture(t *testing.T, path, script string) {
	t.Helper()
	mustWrite(t, path, []byte("#!/bin/sh\n"+script+"\n"))
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestGitInstallationAndRetry(t *testing.T) {
	chmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			a := testApp(t)
			bin := t.TempDir()
			log := filepath.Join(bin, "calls")
			gate := filepath.Join(bin, "fail")
			if failure {
				mustWrite(t, gate, nil)
			}
			executableFixture(t, filepath.Join(bin, "sudo"), `if [ "$1" = -n ]; then shift; fi; [ "$1" = -- ] || exit 90; shift; exec "$@"`)
			executableFixture(t, filepath.Join(bin, "apt-get"), fmt.Sprintf(`printf '%%s\n' "$*" >> %s
[ ! -f %s ] || exit 23
if [ "$1" = install ]; then
  printf '#!/bin/sh\nprintf "git version fixture\\n"\n' > %s
  %s 755 %s
fi`, quote(log), quote(gate), quote(filepath.Join(bin, "git")), quote(chmod), quote(filepath.Join(bin, "git"))))
			t.Setenv("PATH", bin)
			err := a.command([]string{"exec", "--", "git", "--version"})
			if failure {
				if err == nil || !strings.Contains(err.Error(), "git installation failed") {
					t.Fatal("missing actionable installation failure", err)
				}
				os.Remove(gate)
				if err := a.command([]string{"setup"}); err != nil {
					t.Fatal("could not resume installation", err)
				}
				if err := a.command([]string{"exec", "--", "git", "--version"}); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if a.out.(*bytes.Buffer).String() != "git version fixture\n" {
				t.Fatal("installer polluted command stdout", a.out)
			}
			before, _ := os.ReadFile(log)
			if err := a.setup(); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(log)
			if !bytes.Equal(before, after) {
				t.Fatal("existing system Git was reinstalled")
			}
			if !strings.HasSuffix(string(after), "update\ninstall -y --no-install-recommends git ca-certificates\n") {
				t.Fatal("wrong packages or installation order", string(after))
			}
		})
	}
}

func TestGitInstallerPrivilegesAndUnsupportedHost(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, err := gitInstallCommands(true, true); err == nil {
		t.Fatal("accepted an unsupported package manager")
	}
	executableFixture(t, filepath.Join(bin, "dnf"), "exit 99")
	if _, err := gitInstallCommands(false, true); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatal("missing privilege instruction", err)
	}
	executableFixture(t, filepath.Join(bin, "sudo"), "exit 99")
	for _, interactive := range []bool{false, true} {
		commands, err := gitInstallCommands(false, interactive)
		if err != nil || len(commands) != 1 {
			t.Fatal(commands, err)
		}
		want := []string{filepath.Join(bin, "sudo")}
		if !interactive {
			want = append(want, "-n")
		}
		want = append(want, "--", filepath.Join(bin, "dnf"), "install", "-y", "git", "ca-certificates")
		if !reflect.DeepEqual(commands[0], want) {
			t.Fatal("unsafe elevation command", commands)
		}
	}
}

func TestImportedSessionPreparationAndRecovery(t *testing.T) {
	source := testApp(t)
	fixtureLogins(t, source)
	members := []member{ghMember("alice"), hMember("project")}
	payload, err := source.exportPayload("imported session", members)
	if err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	addSession(t, a, "existing", member{Service: "office", Key: jsonKey("unrelated"), Account: "unrelated"})
	name, err := a.importPayload(payload, "")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	executableFixture(t, filepath.Join(bin, "git"), `printf 'git version fixture\n'`)
	executableFixture(t, filepath.Join(bin, "docker"), `printf 'private daemon diagnostics'; exit 1`)
	t.Setenv("PATH", bin)
	ghScript := "#!/bin/sh\nprintf '%s\\n' '{\"hosts\":{\"github.com\":[{\"login\":\"alice\",\"state\":\"success\",\"active\":true}]}}'\n"
	hcloudScript := "#!/bin/sh\nprintf '%s\\n' '[{\"name\":\"project\",\"active\":true}]'\n"
	archives := map[string][]byte{
		"gh":     tarBytes(t, []*tar.Header{{Name: "bin/gh", Mode: 0755, Size: int64(len(ghScript)), Typeflag: tar.TypeReg}}, []string{ghScript}),
		"hcloud": tarBytes(t, []*tar.Header{{Name: "hcloud", Mode: 0755, Size: int64(len(hcloudScript)), Typeflag: tar.TypeReg}}, []string{hcloudScript}),
	}
	var fail atomic.Bool
	fail.Store(true)
	var ghDownloads, hcloudDownloads, unexpectedDownloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tool := strings.TrimPrefix(r.URL.Path, "/")
		switch tool {
		case "gh":
			ghDownloads.Add(1)
		case "hcloud":
			hcloudDownloads.Add(1)
		default:
			unexpectedDownloads.Add(1)
		}
		if tool == "gh" && fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(archives[tool])
	}))
	defer server.Close()
	a.catalog.Archives = map[string]download{}
	for tool, data := range archives {
		a.catalog.Archives[tool] = download{URL: server.URL + "/" + tool, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	}
	if err := a.command([]string{"overview", name}); err == nil || !strings.Contains(err.Error(), "tool not ready") {
		t.Fatal("incomplete preparation reported success", err)
	}
	fail.Store(false)
	a.out.(*bytes.Buffer).Reset()
	if err := a.command([]string{"overview", name}); err != nil {
		t.Fatal("retry failed", err)
	}
	if err := a.command([]string{"overview", name}); err != nil {
		t.Fatal("repeat failed", err)
	}
	if ghDownloads.Load() != 2 || hcloudDownloads.Load() != 1 || unexpectedDownloads.Load() != 0 {
		t.Fatal("downloaded unrelated or already prepared tools", ghDownloads.Load(), hcloudDownloads.Load(), unexpectedDownloads.Load())
	}
	if all, err := a.sessions(); err != nil || len(all) != 2 || all[0].Name != "existing" || len(all[0].Accounts) != 1 {
		t.Fatal("changed existing sessions", all, err)
	}
	again, err := a.exportPayload(name, members)
	if err != nil || !reflect.DeepEqual(payload, again) {
		t.Fatal("retry changed imported logins", err)
	}
	for _, unused := range []string{"docker", "compose", "node", "m365", "azure", "gcloud", "devcontainer"} {
		if _, err := os.Stat(filepath.Join(a.root, "tools", unused)); !os.IsNotExist(err) {
			t.Fatal("installed an unrelated tool", unused)
		}
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "alice") || !strings.Contains(out, "✓ ready") || strings.Contains(out, "private daemon diagnostics") {
		t.Fatal("incorrect final status", out)
	}
}

func TestOverviewReportsInvalidLogin(t *testing.T) {
	a := testApp(t)
	addSession(t, a, "work", ghMember("alice"))
	t.Setenv("PATH", t.TempDir())
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		if tool == "git" {
			return []byte("git version fixture"), nil
		}
		return []byte(`{"hosts":{"github.com":[{"login":"alice","state":"error","token":"private-fixture"}]}}`), nil
	}
	if err := a.command([]string{"overview", "work"}); err == nil {
		t.Fatal("reported invalid login as ready")
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "sign in again") || strings.Contains(out, "private-fixture") {
		t.Fatal("misleading or unsafe status", out)
	}
}

func fakeNode(t *testing.T, a *app) {
	t.Helper()
	script := `#!/bin/sh
case "$*" in
 *@pnp/cli-microsoft365@*) entry=node_modules/@pnp/cli-microsoft365/dist/index.js ;;
 *@devcontainers/cli@*) entry=node_modules/@devcontainers/cli/devcontainer.js ;;
 *) exit 99 ;;
esac
while [ "$#" -gt 0 ]; do
 if [ "$1" = --prefix ]; then shift; dest="$1"; fi
 shift
done
mkdir -p "$(dirname "$dest/$entry")"
printf fixture > "$dest/$entry"
printf 'noisy installer output\n'
printf 'noisy installer diagnostics\n' >&2
`
	path := filepath.Join(a.root, "tools", "node", "bin", "node")
	mustWrite(t, path, []byte(script))
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(a.root, "tools", "node", ".commander-version"), []byte(a.catalog.Archives["node"].SHA256))
}

func TestMicrosoft365DoesNotInstallContainers(t *testing.T) {
	a := testApp(t)
	fakeNode(t, a)
	if err := a.prepareTool("m365"); err != nil {
		t.Fatal(err)
	}
	if !a.packageReady("m365") {
		t.Fatal("Microsoft 365 not ready")
	}
	for _, unused := range []string{"devcontainer", "docker", "compose", "azure", "python", "gh"} {
		if _, err := os.Stat(filepath.Join(a.root, "tools", unused)); !os.IsNotExist(err) {
			t.Fatal("installed unrelated dependency", unused)
		}
	}
	if a.out.(*bytes.Buffer).Len() != 0 || strings.Contains(a.errOut.(*bytes.Buffer).String(), "noisy") {
		t.Fatal("installer output leaked")
	}
	a.errOut = &bytes.Buffer{}
	if err := a.setup(); err != nil || a.errOut.(*bytes.Buffer).Len() != 0 {
		t.Fatal("ready installation was not quiet", err)
	}
}

func TestFreshDoctorDoesNotInstallTools(t *testing.T) {
	a := testApp(t)
	if err := a.doctor(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(a.root, "tools"))
	if err != nil || len(entries) != 0 {
		t.Fatal("diagnostics installed tools", err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "available on demand") {
		t.Fatal("missing on-demand status")
	}
}

func TestMenuSurvivesUsedToolDownloadFailure(t *testing.T) {
	a := testApp(t)
	preparedTools(t, a)
	addSession(t, a, "work")
	binary := []byte("installed fixture")
	mustWrite(t, filepath.Join(a.root, "commander"), binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(binary), releaseAsset)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("COMMANDER_UI_UPDATE_URL", server.URL+"/")
	t.Setenv("COMMANDER_UI_TOOL_URL", server.URL+"/hcloud")
	t.Setenv("COMMANDER_UI_TOOL_SUM", strings.Repeat("a", 64))
	s := openUI(t, a.root)
	s.wait(t, "[q]uit")
	if !strings.Contains(s.output, "tools need attention") {
		t.Fatal("missing repair feedback")
	}
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal("tool failure blocked menu", err)
	}
}
