package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func memoryCommand(t *testing.T, a *app, input []byte, args ...string) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "memory-input")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(input); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	previousIn, previousOut := a.in, a.out
	a.in, a.out = f, &out
	defer func() { a.in, a.out = previousIn, previousOut }()
	err = a.command(append([]string{"memory"}, args...))
	return out.String(), err
}

func memoryRevision(t *testing.T, a *app) string {
	t.Helper()
	out, err := memoryCommand(t, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(out, "\n")
	revision, ok := strings.CutPrefix(line, "revision: ")
	if !ok || len(revision) != 64 {
		t.Fatal("missing revision", out)
	}
	return revision
}

func TestMemoryLifecycle(t *testing.T) {
	a := testApp(t)
	a.run = func(string, []string, bool, map[string]string) ([]byte, error) {
		t.Fatal("memory invoked a native service")
		return nil, nil
	}
	path := filepath.Join(a.root, "MEMORY.md")
	initial := memoryRevision(t, a)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created a memory file", err)
	}
	input := []byte("* Prefer clear explanations.\n* Preserve names such as Zürich.\n")
	out, err := memoryCommand(t, a, input, "write", initial)
	if err != nil || !strings.Contains(out, "memory saved:") {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, input) {
		t.Fatal("saved memory differs", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("memory permissions", err)
	}
	revision := memoryRevision(t, a)
	if revision == initial {
		t.Fatal("revision did not change")
	}
	// A second session holding the old revision cannot discard these preferences.
	if _, err := memoryCommand(t, a, []byte("* Stale replacement.\n"), "write", initial); err == nil || !strings.Contains(err.Error(), "memory changed") {
		t.Fatal("accepted stale write", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(data, input) {
		t.Fatal("conflict changed memory", err)
	}
	out, err = memoryCommand(t, a, input, "write", revision)
	after, statErr := os.Stat(path)
	if err != nil || statErr != nil || out != "memory unchanged\n" || !os.SameFile(info, after) {
		t.Fatal("unchanged input rewrote memory", out, err, statErr)
	}
	if _, err := memoryCommand(t, a, nil, "write", revision); err != nil {
		t.Fatal("could not forget preferences", err)
	}
	out, err = memoryCommand(t, a, nil)
	if err != nil || !strings.Contains(out, "characters: 0/4000\n\n") {
		t.Fatal("memory was not cleared", out, err)
	}
}

func TestMemoryValidationPreservesPreviousContent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"empty", nil, true},
		{"exact ASCII limit", []byte("* " + strings.Repeat("a", 3997) + "\n"), true},
		{"exact Unicode limit", []byte("* " + strings.Repeat("😀", 3997) + "\n"), true},
		{"CRLF bullets", []byte("* One.\r\n\r\n- Two.\r\n"), true},
		{"one character over", []byte("* " + strings.Repeat("é", 3998) + "\n"), false},
		{"oversized input", []byte("* " + strings.Repeat("a", 100000)), false},
		{"invalid UTF8", []byte{'*', ' ', 0xff}, false},
		{"heading", []byte("# Preferences\n* One.\n"), false},
		{"paragraph", []byte("Prefer short answers.\n"), false},
		{"nested list", []byte("* One.\n  * Two.\n"), false},
		{"empty bullet", []byte("* \n"), false},
		{"bare marker", []byte("*"), false},
		{"terminal escape", []byte("* \x1b[31mred\n"), false},
		{"bare carriage return", []byte("* One.\rTwo.\n"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			path := filepath.Join(a.root, "MEMORY.md")
			before := []byte("* Existing preference.\n")
			mustWrite(t, path, before)
			_, err := memoryCommand(t, a, tc.data, "write", memoryRevision(t, a))
			if (err == nil) != tc.valid {
				t.Fatal("unexpected validation result", err)
			}
			want := before
			if tc.valid {
				want = tc.data
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, want) {
				t.Fatal("invalid write damaged memory or valid write changed its text", err)
			}
			if tc.valid {
				out, err := memoryCommand(t, a, nil)
				if err != nil || !strings.Contains(out, fmt.Sprintf("characters: %d/4000", utf8.RuneCount(tc.data))) {
					t.Fatal("incorrect Unicode count", out, err)
				}
			}
		})
	}
}

func TestMemoryRejectsSymlinks(t *testing.T) {
	for _, name := range []string{"MEMORY.md", ".gitignore"} {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			revision := memoryRevision(t, a)
			target := filepath.Join(t.TempDir(), "outside")
			mustWrite(t, target, []byte("outside content"))
			path := filepath.Join(a.root, name)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if name == "MEMORY.md" {
				if _, err := memoryCommand(t, a, nil); err == nil {
					t.Fatal("read followed a symlink")
				}
			}
			if _, err := memoryCommand(t, a, []byte("* New preference.\n"), "write", revision); err == nil {
				t.Fatal("write accepted a symlink")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "outside content" {
				t.Fatal("changed external file", err)
			}
		})
	}
}

func TestMemoryIgnoreUpgradeAndRelocation(t *testing.T) {
	a := testApp(t)
	ignore := filepath.Join(a.root, ".gitignore")
	mustWrite(t, ignore, []byte("# User rules\n/local-data"))
	input := []byte("* Explain maintenance costs.\n")
	if _, err := memoryCommand(t, a, input, "write", memoryRevision(t, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryCommand(t, a, input, "write", memoryRevision(t, a)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ignore)
	if err != nil || string(data) != "# User rules\n/local-data\n/MEMORY.md\n" {
		t.Fatal("ignore upgrade lost user rules or added duplicates", err)
	}
	if out, err := exec.Command("git", "init", "--quiet", a.root).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if out, err := exec.Command("git", "-C", a.root, "check-ignore", "MEMORY.md").CombinedOutput(); err != nil {
		t.Fatal("memory is not ignored", string(out), err)
	}
	// Setup refreshes generated guidance but must preserve learned preferences.
	if err := a.command([]string{"setup"}); err != nil {
		t.Fatal(err)
	}
	saveProfile(t, a, "another profile")
	before := memoryRevision(t, a)
	moved := filepath.Join(t.TempDir(), "Commander moved")
	if err := os.Rename(a.root, moved); err != nil {
		t.Fatal(err)
	}
	b, err := newApp(moved, os.Stdin, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out, err := memoryCommand(t, b, nil)
	if err != nil || !strings.HasSuffix(out, string(input)) || memoryRevision(t, b) != before {
		t.Fatal("memory did not survive setup, profile switch or relocation", err)
	}
}
