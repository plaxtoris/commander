package main

import (
	"archive/tar"
	"bytes"
	"context"
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
	"time"

	"golang.org/x/sys/unix"
)

// Ready tool fixtures keep startup tests offline while exercising real setup.
func preparedTools(t *testing.T, a *app) {
	t.Helper()
	for name, entry := range map[string]string{"gh": "bin/gh", "hcloud": "hcloud", "node": "bin/node", "uv": "uv", "gcloud": "bin/gcloud", "docker": "docker", "compose": "docker-compose"} {
		path := filepath.Join(a.root, "tools", name, entry)
		mustWrite(t, path, []byte("#!/bin/sh\nexit 99\n"))
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(a.root, "tools", name, ".commander-version"), []byte(a.catalog.Archives[name].SHA256))
	}
	python := filepath.Join(a.root, "tools", "python", "cpython-"+a.catalog.Packages["python"]+"-fixture", "bin", "python3")
	mustWrite(t, python, []byte("#!/bin/sh\nexit 99\n"))
	if err := os.Chmod(python, 0755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		"m365/.commander-version":                                      a.catalog.Packages["m365"],
		"m365/node_modules/@pnp/cli-microsoft365/dist/index.js":        "fixture",
		"devcontainer/.commander-version":                              a.catalog.Packages["devcontainer"],
		"devcontainer/node_modules/@devcontainers/cli/devcontainer.js": "fixture",
		"azure/.commander-version":                                     a.catalog.Packages["azure"],
		"azure/azure/cli/__main__.py":                                  "fixture",
	} {
		mustWrite(t, filepath.Join(a.root, "tools", path), []byte(data))
	}
}

func useRelease(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	previous := releaseURL
	releaseURL = server.URL + "/"
	t.Cleanup(func() { releaseURL = previous; server.Close() })
}

func TestUpdateValidation(t *testing.T) {
	for _, mode := range []string{"new", "current", "corrupt", "invalid checksum", "duplicate checksum", "cannot execute"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			original := []byte("original executable")
			binary := []byte("#!/bin/sh\nexit 0\n")
			if mode == "cannot execute" {
				binary = []byte("not an executable")
			}
			if mode == "current" {
				original = binary
			}
			mustWrite(t, filepath.Join(a.root, "commander"), original)
			sum := sha256.Sum256(binary)
			// Releases list every machine and the name older installs update from.
			other := sha256.Sum256([]byte("other machine"))
			manifest := fmt.Sprintf("%x  commander\n%x  commander-other\n%x  %s\n", other, other, sum, releaseAsset)
			switch mode {
			case "corrupt":
				binary = []byte("corrupted download")
			case "invalid checksum":
				manifest = strings.Repeat("z", 64) + "  " + releaseAsset + "\n"
			case "duplicate checksum":
				manifest += manifest
			}
			var downloads atomic.Int32
			useRelease(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/SHA256SUMS":
					stages, _ := filepath.Glob(filepath.Join(a.root, ".update-*"))
					if len(stages) != 0 {
						t.Error("created a staging directory before finding an update")
					}
					fmt.Fprint(w, manifest)
				case "/" + releaseAsset:
					downloads.Add(1)
					w.Write(binary)
				default:
					http.NotFound(w, r)
				}
			})
			updated, err := a.update()
			if mode == "new" || mode == "current" {
				if err != nil || updated != (mode == "new") {
					t.Fatal(updated, err)
				}
			} else if err == nil || updated {
				t.Fatal("accepted invalid update", updated, err)
			}
			if mode == "current" && downloads.Load() != 0 {
				t.Fatal("downloaded an unchanged executable")
			}
			want := original
			if mode == "new" {
				want = binary
			}
			after, err := os.ReadFile(filepath.Join(a.root, "commander"))
			if err != nil || !bytes.Equal(after, want) {
				t.Fatal("unexpected executable after update", err)
			}
			unlock, err := a.lock("update")
			if err != nil {
				t.Fatal("update lock retained", err)
			}
			unlock()
		})
	}
}

func TestStagedReleaseAcrossChecks(t *testing.T) {
	for _, mode := range []string{"manual", "damaged", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			destination := filepath.Join(a.root, "commander")
			original := []byte("original")
			binary := []byte("#!/bin/sh\nexit 0\n")
			mustWrite(t, destination, original)
			var checks, downloads atomic.Int32
			useRelease(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/SHA256SUMS" {
					checks.Add(1)
					fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(binary), releaseAsset)
					return
				}
				downloads.Add(1)
				w.Write(binary)
			})
			for range 2 {
				if err := a.backgroundUpdate(); err != nil {
					t.Fatal(err)
				}
			}
			old := time.Now().Add(-updateInterval)
			if err := os.Chtimes(filepath.Join(a.root, "tools", ".update-checked"), old, old); err != nil {
				t.Fatal(err)
			}
			if err := a.backgroundUpdate(); err != nil || checks.Load() != 2 || downloads.Load() != 1 {
				t.Fatal("unexpected hourly release traffic", checks.Load(), downloads.Load(), err)
			}
			switch mode {
			case "manual":
				if applied, err := a.update(); !applied || err != nil || checks.Load() != 3 || downloads.Load() != 1 {
					t.Fatal("manual update did not use the verified download", applied, err)
				}
				original = binary
			case "damaged":
				mustWrite(t, filepath.Join(a.root, pendingRelease, releaseAsset), []byte("damaged"))
				if applied, err := a.applyPendingRelease(); applied || err == nil {
					t.Fatal("accepted damaged release")
				}
			case "replaced":
				original = []byte("installed separately")
				mustWrite(t, destination, original)
				if applied, err := a.applyPendingRelease(); applied || err != nil {
					t.Fatal("overwrote a newer installation", err)
				}
			}
			if data, err := os.ReadFile(destination); err != nil || !bytes.Equal(data, original) {
				t.Fatal("incorrect installed release", err)
			}
			if _, err := os.Lstat(filepath.Join(a.root, pendingRelease)); !os.IsNotExist(err) {
				t.Fatal("left an obsolete pending release", err)
			}
		})
	}
}

func TestUpdateCheckTimeout(t *testing.T) {
	useRelease(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	previous := releaseCheckTimeout
	releaseCheckTimeout = 30 * time.Millisecond
	t.Cleanup(func() { releaseCheckTimeout = previous })
	start := time.Now()
	updated, err := testApp(t).update()
	if err == nil || updated || !strings.Contains(err.Error(), "timed out") || time.Since(start) > time.Second {
		t.Fatal("unbounded update check", updated, err)
	}
}

func TestSetupOnlyUsedToolsAndResumes(t *testing.T) {
	a := testApp(t)
	archives := make(map[string]download, len(a.catalog.Archives))
	for name, spec := range a.catalog.Archives {
		archives[name] = spec
	}
	a.catalog.Archives = archives
	payload := tarBytes(t, []*tar.Header{{Name: "hcloud", Mode: 0755, Size: 3, Typeflag: tar.TypeReg}}, []string{"new"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(payload)
	}))
	defer server.Close()
	a.catalog.Archives["hcloud"] = download{Version: "fixture", URL: server.URL, SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}
	if err := a.setup(); err != nil || requests.Load() != 0 {
		t.Fatal("fresh setup downloaded tools", err)
	}
	if err := a.prepareTool("hcloud"); err == nil {
		t.Fatal("ignored failed installation")
	}
	running, err := a.installationLock("native", true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer running()
	if err := a.setup(); err != nil {
		t.Fatal("did not resume requested tool while a session command runs", err)
	}
	if err := a.setup(); err != nil || requests.Load() != 2 {
		t.Fatal("downloaded ready tool", err, requests.Load())
	}
	data, err := os.ReadFile(filepath.Join(a.root, "tools", "hcloud", "hcloud"))
	if err != nil || string(data) != "new" {
		t.Fatal("tool not installed", err)
	}
	for _, name := range []string{"gh", "azure", "node", "docker", "gcloud"} {
		if _, err := os.Stat(filepath.Join(a.root, "tools", name)); !os.IsNotExist(err) {
			t.Fatal("installed unused tool", name)
		}
	}
}

func TestSetupAndManualUpdateRefreshAgentInstructions(t *testing.T) {
	for _, command := range []string{"setup", "update"} {
		t.Run(command, func(t *testing.T) {
			a := testApp(t)
			binary := []byte("installed fixture")
			mustWrite(t, filepath.Join(a.root, "commander"), binary)
			useRelease(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(binary), releaseAsset)
			})
			preserved := []string{"CLAUDE.md", "GEMINI.md", ".gitignore", "MEMORY.md", "workspaces/work/project/AGENTS.md"}
			for _, path := range preserved {
				mustWrite(t, filepath.Join(a.root, path), []byte("local instructions\n"))
			}
			path := filepath.Join(a.root, "AGENTS.md")
			for _, missing := range []bool{false, true} {
				if missing {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					mustWrite(t, path, []byte("outdated instructions\n"))
				}
				if err := a.command([]string{command}); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != agentInstructions {
					t.Fatal("instructions were not regenerated", err)
				}
			}
			for _, path := range preserved {
				data, err := os.ReadFile(filepath.Join(a.root, path))
				if err != nil || string(data) != "local instructions\n" {
					t.Fatal("changed unmanaged file", path, err)
				}
			}
		})
	}
}

func TestPrivateUpdateUsesSelectedIdentity(t *testing.T) {
	for _, identity := range []string{"selected", "other", "no profile"} {
		t.Run(identity, func(t *testing.T) {
			a := testApp(t)
			if identity != "no profile" {
				useSession(t, a, "work", ghMember("selected"))
				a.session.Repositories = []string{"plaxtoris/commander"}
			}
			useRelease(t, http.NotFound)
			binary := []byte("#!/bin/sh\nexit 0\n")
			sum := sha256.Sum256(binary)
			calls := []string{}
			a.releaseRun = func(ctx context.Context, args []string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("private update lacks a timeout")
				}
				if unlock, err := a.lock("native"); err == nil {
					unlock()
					t.Fatal("native identity was not locked")
				}
				calls = append(calls, args[0])
				if args[0] == "api" {
					if !reflect.DeepEqual(args, []string{"api", "--hostname", "github.com", "user", "--jq", ".login"}) {
						t.Fatal(args)
					}
					return []byte(identity + "\n"), nil
				}
				if identity != "selected" {
					t.Fatal("download used an unselected identity")
				}
				asset, stage := args[6], args[8]
				data := binary
				if asset == "SHA256SUMS" {
					data = []byte(fmt.Sprintf("%x  %s\n", sum, releaseAsset))
				}
				mustWrite(t, filepath.Join(stage, asset), data)
				return nil, nil
			}
			updated, err := a.update()
			if identity == "selected" {
				if err != nil || !updated || !reflect.DeepEqual(calls, []string{"api", "release", "release"}) {
					t.Fatal(updated, err, calls)
				}
				calls = nil
				if updated, err := a.update(); updated || err != nil || !reflect.DeepEqual(calls, []string{"api", "release"}) {
					t.Fatal("private update downloaded an unchanged binary", updated, err, calls)
				}
			} else if err == nil || updated {
				t.Fatal("accepted an unselected identity")
			}
			if identity == "no profile" && len(calls) != 0 {
				t.Fatal("used GitHub without a selected profile")
			}
		})
	}
}

func TestUpdateCleanupKeepsActiveAndRecoveryFolders(t *testing.T) {
	a := testApp(t)
	stage := filepath.Join(a.root, ".update-42")
	mustWrite(t, filepath.Join(stage, "download"), []byte("fixture"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(a.root, ".update-43")); err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(a.root, ".import-11", "recovery")
	mustWrite(t, recovery, nil)
	unlock, err := a.lock("update")
	if err != nil {
		t.Fatal(err)
	}
	a.cleanupAfterUpdate()
	unlock()
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("removed active update", err)
	}
	a.cleanupAfterUpdate()
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("left abandoned update", err)
	}
	for _, path := range []string{outside, filepath.Join(a.root, ".update-43"), recovery} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("removed unrelated path", path, err)
		}
	}
}

func TestUpdateCleanupRequiresSameHostAndAnEarlierBoot(t *testing.T) {
	a := testApp(t)
	host := strings.Repeat("a", 32)
	boot := host + strings.Repeat("b", 32)
	for _, item := range []struct{ name, owner string }{
		{".run-11", host + strings.Repeat("c", 32)},
		{".run-12", boot},
		{".run-13", ""},
		{".run-14", strings.Repeat("d", 64)},
		{".run-project", host + strings.Repeat("c", 32)},
	} {
		path := filepath.Join(a.root, ".secrets", item.name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if item.owner != "" {
			if err := unix.Lsetxattr(path, runtimeBootAttr, []byte(item.owner), 0); errors.Is(err, unix.ENOTSUP) {
				t.Skip("fixture filesystem does not support extended attributes")
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := a.pruneUpdateDirs(".secrets", boot, ".run-", ".docker-"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".run-11", ".run-12", ".run-13", ".run-14", ".run-project"} {
		_, err := os.Stat(filepath.Join(a.root, ".secrets", name))
		if (name == ".run-11" && !os.IsNotExist(err)) || (name != ".run-11" && err != nil) {
			t.Fatal("unexpected cleanup result", name, err)
		}
	}
}

// Child processes use the same local release server as the menu fixture.
func installUpdateWorker(t *testing.T, root string) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\n" + quote(exe) + " -test.run=^TestUIHelper$ -- \"$@\"\nprintf done > \"$COMMANDER_HOME/workspaces/worker-done\"\n")
	path := filepath.Join(root, "commander")
	mustWrite(t, path, script)
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	return script
}

func waitUpdateWorker(t *testing.T, root string) {
	t.Helper()
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(filepath.Join(root, "workspaces", "worker-done")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background update did not finish")
}

func TestInteractiveStartupCurrentAndOffline(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint(offline), func(t *testing.T) {
			a := testApp(t)
			addSession(t, a, "work")
			binary := installUpdateWorker(t, a.root)
			// Exercise setup and update after moving the portable installation.
			moved := filepath.Join(t.TempDir(), "Commander moved")
			if err := os.Rename(a.root, moved); err != nil {
				t.Fatal(err)
			}
			var checks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				if offline {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(binary), releaseAsset)
			}))
			defer server.Close()
			t.Setenv("COMMANDER_UI_UPDATE_URL", server.URL+"/")
			staging := filepath.Join(moved, ".update-15")
			mustWrite(t, filepath.Join(staging, "download"), nil)
			for i := 0; i < 2; i++ {
				mustWrite(t, filepath.Join(moved, "AGENTS.md"), []byte("outdated instructions\n"))
				s := openUI(t, moved)
				s.wait(t, "[q] quit")
				if strings.Contains(s.output, "updating…") || strings.Contains(s.output, "update skipped:") {
					t.Fatal("background updates wrote to the menu", s.output)
				}
				s.send(t, "q")
				if err := s.command.Wait(); err != nil {
					t.Fatal(err)
				}
				waitUpdateWorker(t, moved)
				if _, err := os.Stat(staging); (offline && err != nil) || (!offline && !os.IsNotExist(err)) {
					t.Fatal("unexpected automatic cleanup result", err)
				}
				data, err := os.ReadFile(filepath.Join(moved, "AGENTS.md"))
				if err != nil || string(data) != agentInstructions {
					t.Fatal("startup did not refresh instructions after relocation", err)
				}
			}
			if checks.Load() != 1 {
				t.Fatal("repeated update check within one hour", checks.Load())
			}
		})
	}
}

func TestRestartHelper(t *testing.T) {
	if root := os.Getenv("COMMANDER_RESTART_FIXTURE"); root != "" {
		a, err := newApp(root, os.Stdin, os.Stdout, os.Stderr)
		if err == nil {
			err = a.restartCommander([]string{"spaces in argument", "literal $value"})
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestRestartPreservesProcessIO(t *testing.T) {
	a := testApp(t)
	path := filepath.Join(a.root, "commander")
	mustWrite(t, path, []byte("#!/bin/sh\nread -r line\nprintf '%s\\n' \"$PWD\" \"$1\" \"$2\" \"$line\"\nprintf 'fixture stderr' >&2\nexit 37\n"))
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRestartHelper$")
	cmd.Env = append(os.Environ(), "COMMANDER_RESTART_FIXTURE="+a.root)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("stdin fixture\n")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 37 {
		t.Fatal("restart lost exit code", err)
	}
	if out.String() != cmd.Dir+"\nspaces in argument\nliteral $value\nstdin fixture\n" || stderr.String() != "fixture stderr" {
		t.Fatal("restart changed arguments, cwd or streams", out.String(), stderr.String())
	}
}

func TestRuntimeMigration(t *testing.T) {
	a := testApp(t)
	parent := filepath.Join(a.root, ".secrets")
	boot := runtimeBoot()
	if boot == "" {
		t.Skip("requires boot identification")
	}
	if err := unix.Lsetxattr(parent, runtimeMigrationAttr, []byte(boot), 0); err != nil {
		t.Skipf("requires extended attributes: %v", err)
	}
	legacyDir := filepath.Join(parent, ".docker-712")
	mustWrite(t, filepath.Join(legacyDir, "fixture"), nil)
	legacyLock := filepath.Join(parent, ".account-0123456789abcdef01234567.lock")
	mustWrite(t, legacyLock, nil)
	mustWrite(t, filepath.Join(parent, ".memory.lock"), []byte("keep nonempty"))
	a.cleanupAfterUpdate()
	for _, path := range []string{legacyDir, legacyLock} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed same boot legacy entry", path, err)
		}
	}
	previous := strings.Repeat("0", 32)
	if previous == boot[32:] {
		previous = strings.Repeat("1", 32)
	}
	if err := unix.Lsetxattr(parent, runtimeMigrationAttr, []byte(boot[:32]+previous), 0); err != nil {
		t.Fatal(err)
	}
	a.cleanupAfterUpdate()
	for _, path := range []string{legacyDir, legacyLock, filepath.Join(parent, ".update.lock")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("kept legacy entry", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, ".memory.lock")); err != nil {
		t.Fatal("removed nonempty entry", err)
	}
	unlock, err := a.installationLock("native", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := a.installationLock("native", false, false); !errors.Is(err, unix.EWOULDBLOCK) {
		if release != nil {
			release()
		}
		t.Fatal("exclusive lock must contend with a shared lock", err)
	}
	for _, name := range []string{"native", "setup"} {
		release, err := a.installationLock(name, name == "native", false)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if info, err := os.Stat(filepath.Join(parent, ".locks")); err != nil || info.Size() != 0 {
		t.Fatal("expected a single empty lock file", info, err)
	}
}
