package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type task struct {
	Name       string `json:"name"`
	Project    string `json:"project"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	Base       string `json:"base"`
	Integrated string `json:"integrated,omitempty"`
}

var taskName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (a *app) gitAt(dir string, args ...string) (string, error) {
	path, err := a.lookupTool("git")
	if err != nil {
		return "", err
	}
	cmd := gitCommand(context.Background(), path, append([]string{"-C", dir}, args...)...)
	cmd.Env = a.environment("git", nil)
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s failed in %s; work was preserved", args[0], dir)
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *app) projectPath(project string) (string, error) {
	base, err := filepath.EvalSymlinks(filepath.Join(a.root, "workspaces"))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(project) {
		project = filepath.Join(base, project)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, project)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("choose a project inside Commander workspaces")
	}
	return project, nil
}

func (a *app) taskFile(name string) (string, error) {
	if a.session == nil {
		return "", errors.New("start a session before creating a task")
	}
	if !taskName.MatchString(name) {
		return "", errors.New("task names use letters, digits, dots, dashes and underscores")
	}
	path := filepath.Join(a.root, ".sessions", a.session.ID, "tasks", name+".json")
	return path, noSymlinks(a.root, path)
}

func (a *app) readTask(name string) (task, error) {
	var t task
	path, err := a.taskFile(name)
	if err != nil {
		return t, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	if json.Unmarshal(data, &t) != nil || t.Name != name || t.Branch != "commander/"+a.session.ID+"/"+name {
		return t, errors.New("invalid task metadata")
	}
	if t.Path != filepath.Join(a.root, "workspaces", ".tasks", a.session.ID, name) {
		return t, errors.New("invalid task directory")
	}
	project, err := a.projectPath(t.Project)
	if err != nil || project != t.Project {
		return t, errors.New("invalid source project")
	}
	return t, nil
}

func (a *app) taskCommand(args []string) error {
	if a.session == nil {
		return errors.New("start a session before task work")
	}
	if len(args) == 0 {
		return errors.New("task commands: new PROJECT NAME [BASE], list, finish NAME -- CHECK [ARGS...]")
	}
	unlock, err := a.sessionLock(a.session.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	switch args[0] {
	case "list":
		entries, err := os.ReadDir(filepath.Join(a.root, ".sessions", a.session.ID, "tasks"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			t, err := a.readTask(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s  %s -> %s  %s\n", t.Name, t.Branch, t.Base, t.Path)
		}
		return nil
	case "new":
		if len(args) < 3 || len(args) > 4 {
			return errors.New("usage: commander task new PROJECT NAME [BASE]")
		}
		base := "main"
		if len(args) == 4 {
			base = args[3]
		}
		t, err := a.newTask(args[1], args[2], base)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.out, t.Path)
		return nil
	case "finish":
		if len(args) < 4 || args[2] != "--" {
			return errors.New("usage: commander task finish NAME -- CHECK [ARGS...]; checks run on the merged result before pushing")
		}
		return a.finishTask(args[1], args[3:])
	default:
		return errors.New("unknown task command")
	}
}

func (a *app) taskRepoLock(project string) (func(), error) {
	common, err := a.gitAt(project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	return a.credentialLock("repository:" + common)
}

func (a *app) newTask(project, name, base string) (task, error) {
	var t task
	path, err := a.taskFile(name)
	if err != nil {
		return t, err
	}
	_, metadataErr := os.Lstat(path)
	if metadataErr != nil && !os.IsNotExist(metadataErr) {
		return t, metadataErr
	}
	project, err = a.projectPath(project)
	if err != nil {
		return t, err
	}
	if _, err = a.gitAt(project, "check-ref-format", "--branch", base); err != nil || strings.HasPrefix(base, "-") {
		return t, errors.New("invalid target branch")
	}
	origin, err := a.gitAt(project, "config", "remote.origin.url")
	if err != nil {
		return t, err
	}
	if !a.session.allows(repositoryName(origin)) {
		return t, errors.New("repository is outside the session selection")
	}
	unlock, err := a.taskRepoLock(project)
	if err != nil {
		return t, err
	}
	defer unlock()
	if _, err = a.gitAt(project, "fetch", "origin", base); err != nil {
		return t, err
	}
	t = task{Name: name, Project: project, Path: filepath.Join(a.root, "workspaces", ".tasks", a.session.ID, name), Branch: "commander/" + a.session.ID + "/" + name, Base: base}
	if metadataErr == nil {
		previous, err := a.readTask(name)
		if err != nil || previous != t {
			return task{}, errors.New("task name already belongs to another operation")
		}
		if _, err = os.Stat(t.Path); err == nil {
			return t, nil
		}
	}
	if err = noSymlinks(a.root, t.Path); err != nil {
		return task{}, err
	}
	if err = os.MkdirAll(filepath.Dir(t.Path), 0700); err != nil {
		return task{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
		err = atomicWrite(path, encode(t), 0600)
	}
	if err != nil {
		return task{}, err
	}
	args := []string{"worktree", "add", "-b", t.Branch, t.Path, "refs/remotes/origin/" + base}
	if metadataErr == nil {
		if _, e := a.gitAt(project, "show-ref", "--verify", "--quiet", "refs/heads/"+t.Branch); e == nil {
			args = []string{"worktree", "add", t.Path, t.Branch}
		}
	}
	if _, err = a.gitAt(project, args...); err != nil {
		return task{}, err
	}
	return t, nil
}

func (a *app) finishTask(name string, check []string) error {
	t, err := a.readTask(name)
	if err != nil {
		return err
	}
	origin, err := a.gitAt(t.Project, "config", "remote.origin.url")
	if err != nil {
		return err
	}
	if !a.session.allows(repositoryName(origin)) {
		return errors.New("task repository is no longer selected in this session")
	}
	unlock, err := a.taskRepoLock(t.Project)
	if err != nil {
		return err
	}
	defer unlock()
	if t.Integrated != "" {
		if !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(t.Integrated) {
			return errors.New("invalid integrated task revision")
		}
		if _, err = a.gitAt(t.Project, "fetch", "origin", t.Base); err != nil {
			return err
		}
		if _, err = a.gitAt(t.Project, "merge-base", "--is-ancestor", t.Integrated, "refs/remotes/origin/"+t.Base); err != nil {
			return errors.New("remote target no longer contains the integrated task; nothing was removed")
		}
		return a.cleanupTask(t)
	}
	dirty, err := a.gitAt(t.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" {
		return errors.New("commit or resolve task changes before finishing; nothing was removed")
	}
	branch, err := a.gitAt(t.Path, "branch", "--show-current")
	if err != nil || branch != t.Branch {
		return errors.New("task branch changed; nothing was removed")
	}
	tip, err := a.gitAt(t.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err = a.gitAt(t.Project, "fetch", "--prune", "origin"); err != nil {
		return err
	}
	target := "refs/remotes/origin/" + t.Base
	if _, already := a.gitAt(t.Project, "merge-base", "--is-ancestor", tip, target); already != nil {
		// Integration uses a disposable detached worktree. Other sessions keep their
		// working files and indexes untouched, even while checks run.
		mergePath := filepath.Join(filepath.Dir(t.Path), ".merge-"+t.Name)
		if err = noSymlinks(a.root, mergePath); err != nil {
			return err
		}
		// Only this task owns this disposable integration checkout. A retry also
		// clears a checkout left when its previous validation was interrupted.
		if _, err = os.Stat(mergePath); err == nil {
			if branch, e := a.gitAt(mergePath, "branch", "--show-current"); e != nil || branch != "" {
				return errors.New("integration checkout was repurposed; nothing was removed")
			}
			if _, err = a.gitAt(t.Project, "worktree", "remove", "--force", mergePath); err != nil {
				return err
			}
		}
		if _, err = a.gitAt(t.Project, "worktree", "add", "--detach", mergePath, target); err != nil {
			return err
		}
		defer func() {
			a.gitAt(t.Project, "worktree", "remove", "--force", mergePath)
			os.Remove(filepath.Dir(t.Path))
			os.Remove(filepath.Dir(filepath.Dir(t.Path)))
		}()
		if _, err = a.gitAt(mergePath, "merge", "--no-edit", t.Branch); err != nil {
			return errors.New("target changed and merge needs attention; task remains intact. Merge origin/" + t.Base + " in the task and retry")
		}
		if len(check) == 0 {
			return errors.New("supply a validation command")
		}
		command, err := a.lookupTool(check[0])
		if err != nil {
			return err
		}
		cmd := exec.Command(command, check[1:]...)
		cmd.Dir = mergePath
		cmd.Env = a.environment("shell", nil)
		cmd.Stdin = a.in
		cmd.Stdout = a.out
		cmd.Stderr = a.errOut
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("merged result failed validation; task remains intact: %w", err)
		}
		if dirty, err = a.gitAt(mergePath, "status", "--porcelain"); err != nil || dirty != "" {
			return errors.New("validation modified tracked or untracked files; task remains intact")
		}
		if _, err = a.gitAt(mergePath, "push", "origin", "HEAD:refs/heads/"+t.Base); err != nil {
			return errors.New("target push failed or requires a pull request; task remains intact. Merge its PR, then finish again")
		}
		if _, err = a.gitAt(t.Project, "fetch", "origin", t.Base); err != nil {
			return err
		}
	}
	if _, err = a.gitAt(t.Project, "merge-base", "--is-ancestor", tip, target); err != nil {
		return errors.New("remote target does not contain the task; nothing was removed")
	}
	// Delete only this task's remote branch, and only after verifying integration.
	remote, err := a.gitAt(t.Project, "ls-remote", "--heads", "origin", "refs/heads/"+t.Branch)
	if err != nil {
		return err
	}
	if remote != "" {
		if strings.Fields(remote)[0] != tip {
			if _, err = a.gitAt(t.Project, "fetch", "origin", "refs/heads/"+t.Branch); err != nil {
				return err
			}
			if _, err = a.gitAt(t.Project, "merge-base", "--is-ancestor", "FETCH_HEAD", target); err != nil {
				return errors.New("remote task branch contains additional work; nothing was deleted")
			}
		}
		if _, err = a.gitAt(t.Project, "push", "--force-with-lease=refs/heads/"+t.Branch+":"+strings.Fields(remote)[0], "origin", ":refs/heads/"+t.Branch); err != nil {
			return err
		}
	}
	currentTip, err := a.gitAt(t.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	dirty, err = a.gitAt(t.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if currentTip != tip || dirty != "" {
		return errors.New("task changed during integration; new work was preserved")
	}
	// Record verified integration before destructive local cleanup. Repeating
	// finish after an interruption can complete cleanup without the worktree.
	t.Integrated = tip
	path, _ := a.taskFile(name)
	if err = atomicWrite(path, encode(t), 0600); err != nil {
		return err
	}
	return a.cleanupTask(t)
}

func (a *app) cleanupTask(t task) error {
	mergePath := filepath.Join(filepath.Dir(t.Path), ".merge-"+t.Name)
	if err := noSymlinks(a.root, mergePath); err != nil {
		return err
	}
	if _, err := os.Stat(mergePath); err == nil {
		if branch, e := a.gitAt(mergePath, "branch", "--show-current"); e != nil || branch != "" {
			return errors.New("integration checkout was repurposed; nothing was removed")
		}
		if _, err = a.gitAt(t.Project, "worktree", "remove", "--force", mergePath); err != nil {
			return err
		}
	}
	if _, err := os.Stat(t.Path); err == nil {
		tip, e := a.gitAt(t.Path, "rev-parse", "HEAD")
		dirty, d := a.gitAt(t.Path, "status", "--porcelain")
		if e != nil || d != nil || tip != t.Integrated || dirty != "" {
			return errors.New("task changed after integration; new work was preserved")
		}
	}
	if err := a.removeTaskContainers(t); err != nil {
		return err
	}
	worktrees, err := a.gitAt(t.Project, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if strings.Contains("\n"+worktrees+"\n", "\nworktree "+t.Path+"\n") {
		if _, err = a.gitAt(t.Project, "worktree", "remove", "--force", t.Path); err != nil {
			return err
		}
	}
	branches, err := a.gitAt(t.Project, "branch", "--list", t.Branch)
	if err != nil {
		return err
	}
	if branches != "" {
		tip, err := a.gitAt(t.Project, "rev-parse", "refs/heads/"+t.Branch)
		if err != nil || tip != t.Integrated {
			return errors.New("task branch changed after integration; branch was preserved")
		}
		if _, err = a.gitAt(t.Project, "branch", "-D", t.Branch); err != nil {
			return err
		}
	}
	path, _ := a.taskFile(t.Name)
	if err = os.Remove(path); err != nil {
		return err
	}
	os.Remove(filepath.Dir(path))
	os.Remove(filepath.Dir(t.Path))
	os.Remove(filepath.Dir(filepath.Dir(t.Path)))
	// Keep a clean checkout on its target current, without touching user changes.
	if current, e := a.gitAt(t.Project, "branch", "--show-current"); e == nil && current == t.Base {
		if status, e := a.gitAt(t.Project, "status", "--porcelain"); e == nil && status == "" {
			if _, e = a.gitAt(t.Project, "merge", "--ff-only", "refs/remotes/origin/"+t.Base); e != nil {
				fmt.Fprintln(a.errOut, "remote integration complete; source checkout needs a manual update")
			}
		}
	}
	fmt.Fprintln(a.out, "integrated and removed task, temporary branches, worktree and containers:", t.Name)
	return nil
}

func (a *app) removeTaskContainers(t task) error {
	if !a.toolUsed("docker") {
		return nil
	}
	data, err := a.run("docker", []string{"ps", "-aq", "--filter", "label=commander.task=" + a.session.ID + "/" + t.Name}, true, nil)
	if err != nil {
		return err
	}
	ids := strings.Fields(string(data))
	if len(ids) == 0 {
		return nil
	}
	_, err = a.run("docker", append([]string{"rm", "-f"}, ids...), false, nil)
	return err
}
