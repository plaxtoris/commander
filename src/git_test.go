package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGitReadableCheckouts(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestGitReadableCheckoutHelper$")
	cmd.Env = append(os.Environ(), "COMMANDER_GIT_PERMISSIONS_FIXTURE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout permissions: %v\n%s", err, output)
	}
}

func TestGitReadableCheckoutHelper(t *testing.T) {
	if os.Getenv("COMMANDER_GIT_PERMISSIONS_FIXTURE") != "1" {
		t.Skip("subprocess fixture")
	}
	syscall.Umask(0077)
	a := testApp(t)
	source := filepath.Join(a.root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "code", "module.py"), []byte("value = 1\n"))
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"add", "."}, {"commit", "-m", "fixture"}} {
		if _, err := a.gitAt(source, args...); err != nil {
			t.Fatal(err)
		}
	}
	clone := filepath.Join(a.root, "clone")
	if _, err := a.nativeRunContext(context.Background(), "git", []string{"clone", source, clone}, true, nil); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(a.root, "worktree")
	if _, err := a.gitAt(clone, "worktree", "add", "-b", "fixture", worktree); err != nil {
		t.Fatal(err)
	}
	for _, checkout := range []string{clone, worktree} {
		for name, mode := range map[string]os.FileMode{"code": 0755, "code/module.py": 0644} {
			info, err := os.Stat(filepath.Join(checkout, name))
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("%s permissions: %v %v", filepath.Join(checkout, name), info, err)
			}
		}
	}
	private := filepath.Join(a.root, "private-fixture")
	if err := os.WriteFile(private, []byte("fixture"), 0666); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(private)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("Git changed the parent permission mask", info, err)
	}
}
