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
	addSession(t, a, "work", ghMember("selected"))
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		return []byte(`{"hosts":{"github.com":[{"login":"selected","state":"success","active":false,"token":"fixture-secret"},{"login":"unselected","state":"success","active":true}]}}`), nil
	}
	if err := a.command([]string{"overview", "work"}); err != nil {
		t.Fatal(err)
	}
	out := a.out.(*bytes.Buffer).String()
	for _, want := range []string{"session: work", "github\n    selected  github.com  ✓ ready"} {
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
	full := accountTable(rows, 0)
	if !strings.Contains(full, identity) || !strings.Contains(full, "! sign in again") {
		t.Fatal(full)
	}
	for _, width := range []int{24, 40, 60, 77, 100} {
		out := accountTable(rows, width)
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

func useHetzner(t *testing.T, a *app) {
	t.Helper()
	mustWrite(t, filepath.Join(a.root, "tools", ".requested", "hcloud"), nil)
}

func TestInteractiveRenameEditAndDelete(t *testing.T) {
	a := testApp(t)
	useHetzner(t, a)
	addSession(t, a, "personal", hMember("existing"))
	addSession(t, a, "work", hMember("existing"))
	s := openUI(t, a.root)
	s.wait(t, "2 sessions")
	s.send(t, "r")
	s.wait(t, "rename personal:")
	s.send(t, "\x15private\r")
	s.wait(t, "renamed to private")
	s.send(t, "\x1b[Br")
	s.wait(t, "rename work:")
	s.send(t, "\x15Private\r")
	s.wait(t, "session private already exists")
	s.send(t, "\x1b[Ae")
	s.wait(t, "[x] existing")
	s.send(t, " \r")
	s.wait(t, "private saved")
	if p, err := a.findSession("private"); err != nil || len(p.Accounts) != 0 {
		t.Fatal("edit did not save", p, err)
	}
	s.send(t, "d")
	s.wait(t, "delete session private?")
	s.send(t, "y")
	s.wait(t, "private deleted")
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	all, err := a.sessions()
	if err != nil || len(all) != 1 || all[0].Name != "work" || len(all[0].Accounts) != 1 {
		t.Fatal("changed another session", all, err)
	}
}

func TestInteractiveImport(t *testing.T) {
	source := testApp(t)
	mustWrite(t, filepath.Join(source.root, ".secrets", ".config", "hcloud", "cli.toml"), []byte("active_context = 'existing'\n[[contexts]]\nname = 'existing'\ntoken = 'fixture-secret'\n"))
	payload, err := source.exportPayload("imported", []member{hMember("existing")})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "commander.zip")
	if err := writeArchive(archive, payload, "fixture password", false); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	s := openUI(t, a.root)
	s.wait(t, "[q]uit")
	s.send(t, "i")
	s.wait(t, "archive:")
	s.send(t, archive+"\r")
	s.wait(t, "archive password:")
	s.send(t, "fixture password\r")
	s.wait(t, "imported 1 session, 1 account")
	if strings.Contains(s.output, "fixture-secret") {
		t.Fatal("import exposed login data")
	}
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if imported, err := a.findSession("imported"); err != nil || len(imported.Accounts) != 1 {
		t.Fatal(imported, err)
	}
}

func TestInteractiveExportStartsAtLastSession(t *testing.T) {
	a := testApp(t)
	mustWrite(t, filepath.Join(a.root, ".secrets", ".config", "hcloud", "cli.toml"), []byte("active_context = 'existing'\n[[contexts]]\nname = 'existing'\ntoken = 'fixture-secret'\n"))
	mustWrite(t, filepath.Join(a.root, "MEMORY.md"), []byte("* Keep replies concise.\n"))
	alpha := addSession(t, a, "alpha")
	work := addSession(t, a, "work", hMember("existing"))
	a.rememberSession(work.ID)
	t.Setenv("COMMANDER_UI_SESSION", alpha.ID)
	s := openUI(t, a.root)
	s.wait(t, "commander · session alpha")
	s.send(t, "x")
	s.wait(t, "archive password:")
	s.send(t, "abc\r")
	s.wait(t, "use at least 4 characters")
	s.send(t, "x")
	s.wait(t, "archive password:")
	s.send(t, "1234\r")
	s.wait(t, "repeat archive password:")
	s.send(t, "1234\r")
	s.wait(t, "exported work with 1 account")
	s.send(t, "x")
	s.wait(t, "replace work.zip?")
	s.send(t, "n")
	s.wait(t, "[q]uit")
	s.send(t, "x")
	s.wait(t, "replace work.zip?")
	s.send(t, "y")
	s.wait(t, "archive password:")
	s.send(t, "5678\r")
	s.wait(t, "repeat archive password:")
	s.send(t, "5678\r")
	s.wait(t, "exported work with 1 account")
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.output, "fixture-secret") {
		t.Fatal("export exposed login data")
	}
	p, err := readArchive(filepath.Join(a.root, "work.zip"), "5678")
	if err != nil || p.Memory != nil || len(p.Profile.Profiles) != 1 || len(p.Profile.Profiles["work"]) != 1 || len(p.Logins) != 1 {
		t.Fatal("archive does not hold exactly the last session", err)
	}
}

func TestInteractiveSmallTerminalAndPaging(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	a := testApp(t)
	for i := 0; i < 30; i++ {
		addSession(t, a, fmt.Sprintf("session-%02d", i), hMember("project"))
	}
	s := openUI(t, a.root, &pty.Winsize{Rows: 18, Cols: 44})
	s.wait(t, "[q]uit")
	if strings.Contains(s.output, "session-29") {
		t.Fatal("long list was not paged", s.output)
	}
	s.send(t, "\x1b[F")
	s.wait(t, "session-29")
	s.send(t, strings.Repeat("\x1b[5~", 5))
	s.wait(t, "session-00")
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
			addSession(t, a, "work")
			original := []byte("original fixture executable")
			mustWrite(t, filepath.Join(a.root, "commander"), original)
			mustWrite(t, filepath.Join(a.root, "AGENTS.md"), []byte("outdated instructions\n"))
			memory := "* Keep explanations simple.\n"
			mustWrite(t, filepath.Join(a.root, "MEMORY.md"), []byte(memory))
			updatedInstructions := agentInstructions + "\nUpdated release instructions.\n"
			t.Setenv("COMMANDER_UI_UPDATED_INSTRUCTIONS", updatedInstructions)
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
			s.wait(t, "[q]uit")
			for _, unwanted := range []string{"Update Commander and all tools", "Tools and updates", "Setup"} {
				if strings.Contains(s.output, unwanted) {
					t.Fatal("unexpected menu item", unwanted)
				}
			}
			if !valid && !strings.Contains(s.output, "checksum did not match") {
				t.Fatal("failed update was not reported", s.output)
			}
			s.send(t, "q")
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
			instructions, err := os.ReadFile(filepath.Join(a.root, "AGENTS.md"))
			wantInstructions := agentInstructions
			if valid {
				wantInstructions = updatedInstructions
			}
			if err != nil || string(instructions) != wantInstructions {
				t.Fatal("startup did not refresh instructions from the running version", err)
			}
			preferences, err := os.ReadFile(filepath.Join(a.root, "MEMORY.md"))
			if err != nil || string(preferences) != memory {
				t.Fatal("startup changed personal memory", err)
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
			useHetzner(t, a)
			addSession(t, a, "Work Team", hMember("existing"), hMember("Case.Sensitive"))
			s := openUI(t, a.root)
			s.wait(t, "hetzner existing · hetzner Case.Sensitive")
			s.send(t, "e")
			s.wait(t, "+ add account")
			frame := terminalControls.ReplaceAllString(s.output, "")
			for _, want := range []string{"commander · Work Team", "hetzner", "[x] existing", "[x] Case.Sensitive", "! missing account", "[space] select"} {
				if !strings.Contains(frame, want) {
					t.Fatalf("missing %q in edit view: %s", want, frame)
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
			s.wait(t, "[esc] cancel")
			for _, line := range strings.Split(terminalControls.ReplaceAllString(s.output[start:], ""), "\n") {
				if utf8.RuneCountInString(strings.TrimRight(line, "\r")) > 44 {
					t.Fatal("line exceeds narrow terminal", line)
				}
			}
			s.send(t, "\x1b")
			s.wait(t, "[q]uit")
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
		text := accountTable(rows, width)
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

func accountTable(rows []account, limit int) string {
	lines := accountLines(rows, limit)
	text := make([]string, len(lines))
	for i, line := range lines {
		text[i] = strings.TrimRight(line.plain(), " ")
	}
	return strings.Join(text, "\n")
}
