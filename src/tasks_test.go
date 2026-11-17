package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func taskFixture(t *testing.T) (*app, string) {
	t.Helper()
	a := testApp(t)
	origin := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", "--initial-branch=main", origin)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	project := filepath.Join(a.root, "workspaces", "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"remote", "add", "origin", "https://github.com/company/project.git"}, {"config", "url." + origin + ".insteadOf", "https://github.com/company/project.git"}} {
		if _, err := a.gitAt(project, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(project, "README.md"), []byte("fixture\n"))
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}, {"push", "-u", "origin", "main"}} {
		if _, err := a.gitAt(project, args...); err != nil {
			t.Fatal(err)
		}
	}
	return a, project
}

func taskSession(t *testing.T, a *app, name string) *app {
	t.Helper()
	b := *a
	b.out = &bytes.Buffer{}
	b.errOut = &bytes.Buffer{}
	s, err := b.newSession(name, nil, []string{"company/project"})
	if err != nil {
		t.Fatal(err)
	}
	b.session = &s
	return &b
}

func taskCommit(t *testing.T, a *app, task task, file string) {
	t.Helper()
	mustWrite(t, filepath.Join(task.Path, file), []byte(file+"\n"))
	if _, err := a.gitAt(task.Path, "add", file); err != nil {
		t.Fatal(err)
	}
	if _, err := a.gitAt(task.Path, "commit", "-m", file); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTasksIntegrateAndLeaveNoBranchesOrWorktrees(t *testing.T) {
	a, project := taskFixture(t)
	first := taskSession(t, a, "one")
	second := taskSession(t, a, "two")
	one, err := first.newTask(project, "change", "main")
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.newTask(project, "change", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskCommit(t, first, one, "one.txt")
	taskCommit(t, second, two, "two.txt")
	if _, err = first.gitAt(one.Path, "push", "origin", one.Branch); err != nil {
		t.Fatal(err)
	}
	if err = first.sessionCommand([]string{"close", first.session.ID}); err == nil {
		t.Fatal("closed session with unfinished task")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, current := range []*app{first, second} {
		wg.Add(1)
		go func(a *app) { defer wg.Done(); results <- a.finishTask("change", []string{"git", "diff", "--check"}) }(current)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"one.txt", "two.txt"} {
		if _, err = os.Stat(filepath.Join(project, file)); err != nil {
			t.Fatal("integration lost another task", err)
		}
	}
	branches, err := a.gitAt(project, "branch", "--list", "commander/*")
	if err != nil || branches != "" {
		t.Fatal("temporary branch left", branches, err)
	}
	remote, err := a.gitAt(project, "ls-remote", "--heads", "origin", "refs/heads/commander/*")
	if err != nil || remote != "" {
		t.Fatal("remote temporary branch left", remote, err)
	}
	worktrees, err := a.gitAt(project, "worktree", "list", "--porcelain")
	if err != nil || strings.Count(worktrees, "worktree ") != 1 {
		t.Fatal("worktree left", worktrees, err)
	}
	if _, err = os.Stat(filepath.Join(a.root, "workspaces", ".tasks")); !os.IsNotExist(err) {
		t.Fatal("task directory left", err)
	}
}

func TestTaskFailurePreservesWork(t *testing.T) {
	a, project := taskFixture(t)
	a = taskSession(t, a, "work")
	task, err := a.newTask(project, "change", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskCommit(t, a, task, "change.txt")
	before, _ := a.gitAt(project, "rev-parse", "main")
	if a.finishTask("change", []string{"false"}) == nil {
		t.Fatal("ignored validation failure")
	}
	after, _ := a.gitAt(project, "rev-parse", "main")
	if before != after {
		t.Fatal("published unverified result")
	}
	if _, err = a.readTask("change"); err != nil {
		t.Fatal("lost failed task", err)
	}
	mustWrite(t, filepath.Join(task.Path, "unfinished.txt"), []byte("keep\n"))
	if a.finishTask("change", []string{"true"}) == nil {
		t.Fatal("discarded uncommitted work")
	}
	if data, err := os.ReadFile(filepath.Join(task.Path, "unfinished.txt")); err != nil || string(data) != "keep\n" {
		t.Fatal("lost unfinished work", err)
	}
}

func TestInterruptedTaskCreationCanResume(t *testing.T) {
	a, project := taskFixture(t)
	a = taskSession(t, a, "work")
	task, err := a.newTask(project, "change", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.gitAt(project, "worktree", "remove", task.Path); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.newTask(project, "change", "main")
	if err != nil || resumed != task {
		t.Fatal(resumed, err)
	}
	if err = a.finishTask("change", []string{"true"}); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedIntegratedCleanupCanResume(t *testing.T) {
	for _, removeBranch := range []bool{false, true} {
		t.Run(map[bool]string{false: "worktree removed", true: "branch removed"}[removeBranch], func(t *testing.T) {
			a, project := taskFixture(t)
			a = taskSession(t, a, "work")
			task, err := a.newTask(project, "change", "main")
			if err != nil {
				t.Fatal(err)
			}
			taskCommit(t, a, task, "change.txt")
			if _, err = a.gitAt(task.Path, "push", "origin", "HEAD:main"); err != nil {
				t.Fatal(err)
			}
			task.Integrated, err = a.gitAt(task.Path, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			path, _ := a.taskFile(task.Name)
			if err = atomicWrite(path, encode(task), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = a.gitAt(project, "worktree", "remove", task.Path); err != nil {
				t.Fatal(err)
			}
			if removeBranch {
				if _, err = a.gitAt(project, "branch", "-D", task.Branch); err != nil {
					t.Fatal(err)
				}
			}
			if err = a.finishTask(task.Name, []string{"true"}); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("metadata remains", err)
			}
			branches, err := a.gitAt(project, "branch", "--list", "commander/*")
			if err != nil || branches != "" {
				t.Fatal("branch remains", branches, err)
			}
		})
	}
}
