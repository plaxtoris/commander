package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var containerID = regexp.MustCompile(`^[a-f0-9]{24}$`)
var dockerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
var errContainerID = errors.New("unknown container; use the ID that commander up printed in this session")

type containerProject struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Container string `json:"container"`
	Workspace string `json:"workspace"`
	Account   string `json:"account,omitempty"`
}

func (a *app) containerFile(id string) (string, error) {
	if a.session == nil {
		return "", errNoSession
	}
	if !containerID.MatchString(id) {
		return "", errContainerID
	}
	path := filepath.Join(a.root, ".sessions", a.session.ID, "containers", id+".json")
	return path, noSymlinks(a.root, path)
}

func (a *app) readContainerProject(id string) (containerProject, error) {
	var project containerProject
	path, err := a.containerFile(id)
	if err != nil {
		return project, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return project, errContainerID
	}
	if err != nil {
		return project, err
	}
	if json.Unmarshal(data, &project) != nil || project.ID != id || project.Container != "" && !dockerID.MatchString(project.Container) {
		return project, errors.New("invalid container metadata")
	}
	actual, err := a.projectPath(project.Project)
	if err != nil || actual != project.Project {
		return project, errors.New("invalid project path")
	}
	return project, nil
}

func (a *app) up(project string) error {
	if a.session == nil {
		return errNoSession
	}
	project, err := a.projectPath(project)
	if err != nil {
		return err
	}
	if err = a.ensure(); err != nil {
		return err
	}
	unlock, err := a.sessionLock(a.session.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	origin, err := a.gitAt(project, "config", "remote.origin.url")
	if err != nil {
		return err
	}
	if !a.session.allows(repositoryName(origin)) {
		return errors.New("project is outside the session repository selection")
	}
	githubAccounts := 0
	for _, m := range a.session.Accounts {
		if m.Service == "github" {
			githubAccounts++
		}
	}
	if githubAccounts > 0 {
		if _, err = a.selectedMember("github", nil); err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(project))
	id := fmt.Sprintf("%x", sum[:12])
	projectUnlock, err := a.credentialLock("container:" + a.session.ID + ":" + id)
	if err != nil {
		return err
	}
	defer projectUnlock()
	record := containerProject{ID: id, Project: project, Account: a.account}
	path, err := a.containerFile(id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err = atomicWrite(path, encode(record), 0600); err != nil {
		return err
	}
	bridge, err := a.startBridge()
	if err != nil {
		return err
	}
	args := []string{"up", "--workspace-folder", project, "--docker-path", filepath.Join(a.root, "tools", "docker", "docker"), "--docker-compose-path", filepath.Join(a.root, "tools", "compose", "docker-compose"), "--id-label", "commander.session=" + a.session.ID, "--id-label", "commander.project=" + id, "--mount", "type=bind,source=" + bridge + ",target=/run/commander"}
	// Absolute Git administrative paths must remain reachable in a worktree.
	common, err := a.gitAt(project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Join(project, ".git")); err == nil && !info.IsDir() {
		args = append(args, "--mount", "type=bind,source="+common+",target="+common)
	}
	taskDir := filepath.Join(a.root, "workspaces", ".tasks", a.session.ID)
	if filepath.Dir(project) == taskDir {
		task, err := a.readTask(filepath.Base(project))
		if err != nil {
			return err
		}
		args = append(args, "--id-label", "commander.task="+a.session.ID+"/"+task.Name)
	}
	// Lifecycle commands receive Git credentials from the first start, too.
	helper := "!/run/commander/commander bridge-client git " + id
	gitConfig := map[string]string{"GIT_CONFIG_COUNT": "4", "GIT_CONFIG_KEY_0": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_0": "", "GIT_CONFIG_KEY_1": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_1": helper, "GIT_CONFIG_KEY_2": "credential.useHttpPath", "GIT_CONFIG_VALUE_2": "true", "GIT_CONFIG_KEY_3": "safe.directory", "GIT_CONFIG_VALUE_3": "*"}
	gitConfig["GIT_CONFIG_COUNT"] = "6"
	gitConfig["GIT_CONFIG_KEY_4"] = "url.https://github.com/.insteadOf"
	gitConfig["GIT_CONFIG_VALUE_4"] = "git@github.com:"
	gitConfig["GIT_CONFIG_KEY_5"] = "url.https://github.com/.insteadOf"
	gitConfig["GIT_CONFIG_VALUE_5"] = "ssh://git@github.com/"
	for key, value := range gitConfig {
		args = append(args, "--remote-env", key+"="+value)
	}
	data, err := a.run("devcontainer", args, true, nil)
	if err != nil {
		return err
	}
	var result struct {
		Outcome   string `json:"outcome"`
		Container string `json:"containerId"`
		Workspace string `json:"remoteWorkspaceFolder"`
	}
	if json.Unmarshal(data, &result) != nil || result.Outcome != "success" || !dockerID.MatchString(result.Container) || !filepath.IsAbs(result.Workspace) {
		return errors.New("devcontainer did not return a valid container")
	}
	// Runtime configuration applies to manual docker exec and editor terminals,
	// not only devcontainer lifecycle commands. No credentials are written here.
	script := `command -v git >/dev/null
git config --system --replace-all credential.https://github.com.helper ''
git config --system --add credential.https://github.com.helper ` + quote(helper) + `
git config --system credential.useHttpPath true
git config --system --replace-all url.https://github.com/.insteadOf 'git@github.com:'
git config --system --add url.https://github.com/.insteadOf 'ssh://git@github.com/'
git config --system --add safe.directory ` + quote(result.Workspace) + "\n"
	for _, key := range []string{"user.name", "user.email"} {
		value, err := a.gitAt(project, "config", key)
		if err == nil && value != "" {
			script += "git config --system " + key + " " + quote(value) + "\n"
		}
	}
	shim := "#!/bin/sh\nexec /run/commander/commander bridge-client make " + quote(id) + " \"$@\"\n"
	script += "printf %s " + quote(shim) + " > /usr/local/bin/commander-make\nchmod 755 /usr/local/bin/commander-make\n"
	if _, err = a.run("docker", []string{"exec", "--user", "root", result.Container, "sh", "-ec", script}, false, nil); err != nil {
		return err
	}
	record.Container = result.Container
	record.Workspace = result.Workspace
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err = atomicWrite(path, encode(record), 0600); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "container ready: %s\nGit uses session %s. Attach an editor or run commander container %s -- COMMAND.\n", result.Container, a.session.Name, id)
	return nil
}

func (a *app) containerCommand(args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return errors.New("usage: commander container ID -- COMMAND [ARGS...]")
	}
	project, err := a.readContainerProject(args[0])
	if err != nil {
		return err
	}
	unlock, err := a.sessionLock(a.session.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = a.run("devcontainer", append([]string{"exec", "--workspace-folder", project.Project, "--id-label", "commander.session=" + a.session.ID, "--id-label", "commander.project=" + project.ID}, args[2:]...), false, nil)
	return err
}

func (a *app) removeSessionContainers() error {
	entries, err := os.ReadDir(filepath.Join(a.root, ".sessions", a.session.ID, "containers"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	data, err := a.run("docker", []string{"ps", "-aq", "--filter", "label=commander.session=" + a.session.ID}, true, nil)
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
