package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/creack/pty"
)

func TestOverviewShowsOnlySelectedAccounts(t *testing.T) {
	a := testApp(t)
	saveProfile(t, a, "work", ghMember("selected"))
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		return []byte(`{"hosts":{"github.com":[{"login":"selected","state":"success","active":false,"token":"fixture-secret"},{"login":"unselected","state":"success","active":true}]}}`), nil
	}
	if err := a.command([]string{"overview"}); err != nil {
		t.Fatal(err)
	}
	out := a.out.(*bytes.Buffer).String()
	for _, want := range []string{"service", "account", "context", "status", "github", "selected @ github.com", "○ saved"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	for _, unwanted := range []string{"unselected", "fixture-secret", "\x1b", "\t"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("unexpected %q in overview", unwanted)
		}
	}
}

func TestAccountTablePreservesLongIdentities(t *testing.T) {
	identity := "jörg." + strings.Repeat("long-", 20) + "@example.invalid"
	rows := []account{{member: member{Service: "office", Account: identity, Context: "Production"}, State: "Sign in again"}}
	full := accountTable(rows, true, 0)
	if !strings.Contains(full, identity) || !strings.Contains(full, "! sign in again") {
		t.Fatal(full)
	}
	for _, width := range []int{24, 40, 60, 77, 100} {
		out := accountTable(rows, true, width)
		for _, line := range strings.Split(out, "\n") {
			if !utf8.ValidString(line) || utf8.RuneCountInString(line) > width {
				t.Fatalf("table exceeds width %d: %q", width, line)
			}
		}
		if strings.Contains(out, "…") {
			t.Fatal("account identity was truncated")
		}
	}
}

func TestInteractiveProfileSwitchAndScopedRemoval(t *testing.T) {
	a := testApp(t)
	saveProfile(t, a, "work", hMember("existing"))
	if err := a.editProfiles(func(p *profiles) error {
		p.Profiles["personal"] = []member{hMember("existing")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := openUI(t, a.root)
	s.wait(t, "esc quit")
	s.send(t, "\t\r")
	s.wait(t, "choose a profile")
	s.send(t, "1\r")
	s.wait(t, "profile  personal")
	s.send(t, "\r")
	s.wait(t, "refresh")
	s.send(t, "2\r")
	s.wait(t, "choose an account")
	s.send(t, "\r")
	s.wait(t, "keep the saved login")
	s.send(t, "\r")
	s.wait(t, "no accounts yet")
	s.send(t, "\x1b")
	s.wait(t, "open accounts to add")
	s.send(t, "3\r")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	p, err := a.readProfiles()
	if err != nil || p.Active == nil || *p.Active != "personal" || len(p.Profiles["personal"]) != 0 || len(p.Profiles["work"]) != 1 {
		t.Fatal("profile removal changed another profile", p, err)
	}
}

func TestInteractiveImportPreparesProfile(t *testing.T) {
	source := testApp(t)
	mustWrite(t, filepath.Join(source.root, ".secrets", ".config", "hcloud", "cli.toml"), []byte("active_context = 'existing'\n[[contexts]]\nname = 'existing'\ntoken = 'fixture-secret'\n"))
	payload, err := source.exportPayload("imported", []member{hMember("existing")})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "profile.zip")
	if err := writeArchive(archive, payload, "fixture password"); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	s := openUI(t, a.root)
	s.wait(t, "import profile")
	s.send(t, "\x1b[B\r")
	s.wait(t, "archive path")
	s.send(t, archive+"\r")
	s.wait(t, "archive password")
	s.send(t, "fixture password\r")
	s.wait(t, "tools prepared and account status checked")
	if !strings.Contains(s.output, "profile ready: imported") || !strings.Contains(s.output, "existing") || strings.Contains(s.output, "fixture-secret") {
		t.Fatal("import did not check accounts or exposed login data", s.output)
	}
	s.send(t, "\r")
	s.wait(t, "1 account · current")
	s.send(t, "\r")
	s.wait(t, "accounts")
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveSmallTerminalAndPaging(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	a := testApp(t)
	members := []member{}
	for i := 0; i < 24; i++ {
		members = append(members, hMember(fmt.Sprintf("project-%02d", i)))
	}
	saveProfile(t, a, "work", members...)
	s := openUI(t, a.root, &pty.Winsize{Rows: 18, Cols: 44})
	s.wait(t, "esc quit")
	if strings.Contains(s.output, "project-23") || !strings.Contains(s.output, "pgup/pgdn") {
		t.Fatal("long description was not paged", s.output)
	}
	// Page through the description in a small terminal.
	s.send(t, strings.Repeat("\x1b[6~", 20))
	s.wait(t, "project-23")
	s.send(t, strings.Repeat("\x1b[5~", 20))
	s.wait(t, "profile  work")
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	for chunk := range s.chunks {
		s.output += chunk
	}
	if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(s.output) {
		t.Fatal("NO_COLOR emitted styling")
	}
	controls := regexp.MustCompile(`\x1b\[[?0-9;]*[A-Za-z]`)
	for _, frame := range strings.Split(s.output, "\x1b[H") {
		plain := controls.ReplaceAllString(frame, "")
		if strings.Count(plain, "\n") >= 18 {
			t.Fatal("menu exceeds terminal height", plain)
		}
		for _, line := range strings.Split(plain, "\n") {
			if utf8.RuneCountInString(strings.TrimSuffix(line, "\r")) >= 44 {
				t.Fatal("menu wraps past terminal width", line)
			}
		}
	}
}

func TestInteractiveAutomaticUpdate(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("checksum-valid-%t", valid), func(t *testing.T) {
			a := testApp(t)
			preparedTools(t, a)
			saveProfile(t, a, "work")
			original := []byte("original fixture executable")
			mustWrite(t, filepath.Join(a.root, "commander"), original)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binary := []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'Commander fixture'; exit 0; fi\nprintf '%s' \"$$\" > \"$COMMANDER_HOME/workspaces/update-fixture\"\nexec " + quote(exe) + " -test.run=TestUIHelper\n")
			sum := sha256.Sum256(binary)
			tool := tarBytes(t, []*tar.Header{{Name: "hcloud", Mode: 0755, Size: 3, Typeflag: tar.TypeReg}}, []string{"new"})
			toolSum := sha256.Sum256(tool)
			if !valid {
				sum = sha256.Sum256([]byte("different fixture"))
			}
			var checks, installs atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/SHA256SUMS":
					checks.Add(1)
					fmt.Fprintf(w, "%x  commander\n", sum)
				case "/commander":
					w.Write(binary)
				case "/hcloud":
					installs.Add(1)
					w.Write(tool)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("COMMANDER_UI_UPDATE_URL", server.URL+"/")
			if valid {
				t.Setenv("COMMANDER_UI_TOOL_URL", server.URL+"/hcloud")
				t.Setenv("COMMANDER_UI_TOOL_SUM", fmt.Sprintf("%x", toolSum))
			}
			s := openUI(t, a.root)
			s.wait(t, "esc quit")
			for _, unwanted := range []string{"Update Commander and all tools", "Tools and updates", "Setup"} {
				if strings.Contains(s.output, unwanted) {
					t.Fatal("unexpected menu item", unwanted)
				}
			}
			if !valid && !strings.Contains(s.output, "checksum did not match") {
				t.Fatal("failed update was not reported", s.output)
			}
			s.send(t, "3\r")
			if err := s.command.Wait(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(a.root, "commander"))
			want := original
			if valid {
				want = binary
				marker, err := os.ReadFile(filepath.Join(a.root, "workspaces", "update-fixture"))
				if err != nil || string(marker) != strconv.Itoa(s.command.Process.Pid) {
					t.Fatal("updated executable did not replace the original process", err)
				}
				installed, err := os.ReadFile(filepath.Join(a.root, "tools", "hcloud", "hcloud"))
				if err != nil || string(installed) != "new" || installs.Load() != 1 {
					t.Fatal("restart did not install the changed tool version", err)
				}
			}
			if err != nil || !bytes.Equal(after, want) {
				t.Fatal("unexpected installed executable", err)
			}
			if checks.Load() != 1 {
				t.Fatal("startup repeated the update check", checks.Load())
			}
			staging, _ := filepath.Glob(filepath.Join(a.root, ".update-*"))
			if len(staging) != 0 {
				t.Fatal("restart left update staging directories", staging)
			}
		})
	}
}

func TestInteractiveAccountPresentationAndResize(t *testing.T) {
	for _, mode := range []string{"color", "no-color", "dumb"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("NO_COLOR", "")
			if mode == "no-color" {
				t.Setenv("NO_COLOR", "1")
			}
			if mode == "dumb" {
				t.Setenv("TERM", "dumb")
			}
			a := testApp(t)
			active := hMember("existing")
			active.Context = "Production-EU"
			missing := hMember("Case.Sensitive")
			saveProfile(t, a, "Work Team", active, missing)
			s := openUI(t, a.root)
			s.wait(t, "esc quit")
			s.send(t, "1\r")
			s.wait(t, "esc back")
			frame := terminalControls.ReplaceAllString(s.output, "")
			for _, want := range []string{"commander / accounts", "Work Team", "service", "account", "context", "status", "hetzner", "Production-EU", "Case.Sensitive", "● active", "! missing account"} {
				if !strings.Contains(frame, want) {
					t.Fatalf("missing %q in account view: %s", want, frame)
				}
			}
			if mode == "color" {
				for _, code := range []string{"\x1b[1;36m", "\x1b[32m", "\x1b[33m"} {
					if !strings.Contains(s.output, code) {
						t.Fatal("missing selection or status accent", code)
					}
				}
			} else if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(s.output) {
				t.Fatal("color disablement ignored")
			}
			if err := pty.Setsize(s.file, &pty.Winsize{Rows: 24, Cols: 44}); err != nil {
				t.Fatal(err)
			}
			start := len(s.output)
			s.send(t, "\x1b[H")
			s.wait(t, "context  Production-EU")
			narrow := terminalControls.ReplaceAllString(s.output[start:], "")
			if strings.Contains(narrow, "service  account") {
				t.Fatal("kept wide table after resize")
			}
			if err := pty.Setsize(s.file, &pty.Winsize{Rows: 32, Cols: 100}); err != nil {
				t.Fatal(err)
			}
			s.send(t, "\x1b[H")
			s.wait(t, "service")
			s.send(t, "q")
			s.wait(t, "esc quit")
			s.send(t, "q")
			if err := s.command.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAccountPresentationPreservesDataAndRejectsEscapes(t *testing.T) {
	a := testApp(t)
	rows := []account{{member: member{Service: "dockerhub", Account: "Case.Sensitive@example.test", Context: "Production-EU"}, State: "Check login"}}
	for _, width := range []int{0, 24, 44, 64, 100} {
		text := accountTable(rows, true, width)
		compact := strings.ReplaceAll(text, "\n", "")
		if width == 0 && !strings.Contains(compact, "Case.Sensitive@example.test") {
			t.Fatal("changed account casing")
		}
		if !strings.Contains(text, "dockerhub") || strings.Contains(text, "Docker Hub") {
			t.Fatal("inconsistent service labels")
		}
		if strings.Contains(text, "\x1b") {
			t.Fatal("plain table contains escapes")
		}
	}
	line := uiLine{{"untrusted\x1b[31m label", "32"}}
	text := a.renderLine(line, 100)
	if strings.Contains(text, "\x1b") {
		t.Fatal("redirected output or account data emitted color escapes")
	}
}
