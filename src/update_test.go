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
		"npm/.commander-version":                               a.catalog.Packages["m365"] + "/" + a.catalog.Packages["devcontainer"],
		"npm/node_modules/@pnp/cli-microsoft365/dist/index.js": "fixture",
		"npm/node_modules/@devcontainers/cli/devcontainer.js":  "fixture",
		"azure/.commander-version":                             a.catalog.Packages["azure"],
		"azure/azure/cli/__main__.py":                          "fixture",
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
			manifest := fmt.Sprintf("%x  commander\n", sum)
			switch mode {
			case "corrupt":
				binary = []byte("corrupted download")
			case "invalid checksum":
				manifest = strings.Repeat("z", 64) + "  commander\n"
			case "duplicate checksum":
				manifest += manifest
			}
			var downloads atomic.Int32
			useRelease(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/SHA256SUMS" {
					fmt.Fprint(w, manifest)
				} else {
					downloads.Add(1)
					w.Write(binary)
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
	if err := a.setup(false); err != nil || requests.Load() != 0 {
		t.Fatal("fresh setup downloaded tools", err)
	}
	if err := a.prepareTool("hcloud", false); err == nil {
		t.Fatal("ignored failed installation")
	}
	if err := a.setup(false); err != nil {
		t.Fatal("did not resume requested tool", err)
	}
	if err := a.setup(false); err != nil || requests.Load() != 2 {
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

func TestPrivateUpdateUsesSelectedIdentity(t *testing.T) {
	for _, identity := range []string{"selected", "other", "no profile"} {
		t.Run(identity, func(t *testing.T) {
			a := testApp(t)
			if identity != "no profile" {
				saveProfile(t, a, "work", ghMember("selected"))
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
					data = []byte(fmt.Sprintf("%x  commander\n", sum))
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

func TestInteractiveStartupCurrentAndOffline(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint(offline), func(t *testing.T) {
			a := testApp(t)
			saveProfile(t, a, "work")
			binary := []byte("installed fixture")
			mustWrite(t, filepath.Join(a.root, "commander"), binary)
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
				fmt.Fprintf(w, "%x  commander\n", sha256.Sum256(binary))
			}))
			defer server.Close()
			t.Setenv("COMMANDER_UI_UPDATE_URL", server.URL+"/")
			for i := 0; i < 2; i++ {
				s := openUI(t, moved)
				s.wait(t, "esc quit")
				if offline && !strings.Contains(s.output, "update skipped:") {
					t.Fatal("offline startup did not explain the skipped update")
				}
				s.send(t, "3\r")
				if err := s.command.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if checks.Load() != 2 {
				t.Fatal("did not check exactly once on each start", checks.Load())
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
