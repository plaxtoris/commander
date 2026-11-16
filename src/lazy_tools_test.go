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
	"strings"
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
		err = a.command([]string{"exec", "--", "gh", "a b", "$(literal)"})
		f.Close()
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != 37 {
			t.Fatal("exit code lost", err)
		}
		if got := a.out.(*bytes.Buffer).String(); got != "a b\n$(literal)\nstdin stays intact\n" {
			t.Fatal("polluted stdout", got)
		}
		feedback := a.errOut.(*bytes.Buffer).String()
		if !strings.HasSuffix(feedback, "native stderr") || (i == 0) != strings.Contains(feedback, "Preparing GitHub") {
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
	if err := a.prepareTool("m365", false); err != nil {
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
	if err := a.setup(false); err != nil || a.errOut.(*bytes.Buffer).Len() != 0 {
		t.Fatal("ready installation was not quiet", err)
	}
}

func TestIndependentUpdatesReuseLegacyPackages(t *testing.T) {
	a := testApp(t)
	preparedTools(t, a)
	fakeNode(t, a)
	if a.packageDir("m365") != a.packageDir("devcontainer") {
		t.Fatal("did not reuse legacy packages")
	}
	packages := map[string]string{}
	for name, version := range a.catalog.Packages {
		packages[name] = version
	}
	packages["m365"] = "fixture-new-version"
	a.catalog.Packages = packages
	if err := a.setup(false); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(a.packageDir("m365")) != "m365" || filepath.Base(a.packageDir("devcontainer")) != "npm" {
		t.Fatal("coupled unrelated package updates")
	}
	if !a.packageReady("m365") || !a.packageReady("devcontainer") {
		t.Fatal("package update damaged existing tools")
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
	saveProfile(t, a, "work")
	binary := []byte("installed fixture")
	mustWrite(t, filepath.Join(a.root, "commander"), binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%x  commander\n", sha256.Sum256(binary))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("COMMANDER_UI_UPDATE_URL", server.URL+"/")
	t.Setenv("COMMANDER_UI_TOOL_URL", server.URL+"/hcloud")
	t.Setenv("COMMANDER_UI_TOOL_SUM", strings.Repeat("a", 64))
	s := openUI(t, a.root)
	s.wait(t, "Esc Quit")
	if !strings.Contains(s.output, "Tools need attention") {
		t.Fatal("missing repair feedback")
	}
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal("tool failure blocked menu", err)
	}
}
