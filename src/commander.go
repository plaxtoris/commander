package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var build = "development"

func main() {
	syscall.Umask(0077)
	root := os.Getenv("COMMANDER_HOME")
	if root == "" {
		path, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = filepath.Dir(path)
	}
	a, err := newApp(root, os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		err = a.command(os.Args[1:])
	}
	if err == nil || errors.Is(err, errCancelled) {
		return
	}
	code := 1
	var status *exec.ExitError
	if errors.As(err, &status) {
		code = status.ExitCode()
		if code < 0 {
			code = 128 + int(status.Sys().(syscall.WaitStatus).Signal())
		}
	} else {
		fmt.Fprintln(os.Stderr, "commander:", err)
	}
	os.Exit(code)
}

//go:embed AGENTS.template.md
var agentInstructions string

var errCancelled = errors.New("cancelled")

type app struct {
	root        string
	in          *os.File
	out, errOut io.Writer
	catalog     catalog
	// Tests replace native commands with local fixtures. Production uses nativeRun.
	run        func(string, []string, bool, map[string]string) ([]byte, error)
	releaseRun func(context.Context, []string) ([]byte, error)
}

func newApp(root string, in *os.File, out, errOut io.Writer) (*app, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	a := &app{root: root, in: in, out: out, errOut: errOut, catalog: toolCatalog}
	a.run = a.nativeRun
	a.releaseRun = func(ctx context.Context, args []string) ([]byte, error) {
		return a.nativeRunContext(ctx, "gh", args, true, map[string]string{"GH_HOST": "github.com", "GH_PROMPT_DISABLED": "1"})
	}
	return a, nil
}

func (a *app) command(args []string) error {
	if len(args) == 0 {
		if !a.terminal() {
			return errors.New("open Commander in a terminal; use help, context or overview for text output")
		}
		return a.start()
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprintln(a.out, `commander

  commander                         update automatically, then open the menu
  commander setup                   repair used tools and prepare this folder
  commander context [profile]       account scope for your agent
  commander overview [profile]      check profile accounts
  commander exec -- tool [args...]   run a native tool with commander logins
  commander up project              start a project devcontainer
  commander export [file.zip]        export a profile and selected logins
  commander import file.zip         import a profile and its logins
  commander update                  update commander and installed tools
  commander doctor                  check the installation
  commander version                 show the installed build

start your agent yourself in the commander folder.
projects live in workspaces/. no shell configuration is required.`)
		return nil
	case "version", "--version":
		fmt.Fprintln(a.out, "Commander", build, runtime.GOOS+"/"+runtime.GOARCH)
		return nil
	case "setup":
		if len(args) != 1 {
			return errors.New("usage: commander setup")
		}
		return a.setup(false)
	case "context", "overview":
		if len(args) > 2 {
			return errors.New("usage: commander " + args[0] + " [PROFILE]")
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		return a.describe(name, args[0] == "context")
	case "exec":
		args = args[1:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			return errors.New("usage: commander exec -- TOOL [ARGS...]")
		}
		if err := a.ensure(); err != nil {
			return err
		}
		unlock, err := a.lock("native")
		if err != nil {
			return err
		}
		defer unlock()
		_, err = a.run(args[0], args[1:], false, nil)
		return err
	case "up":
		if len(args) != 2 {
			return errors.New("usage: commander up PROJECT")
		}
		return a.up(args[1])
	case "doctor":
		return a.doctor()
	case "update":
		if len(args) != 1 {
			return errors.New("usage: commander update")
		}
		updated, err := a.update()
		if err != nil {
			return err
		}
		if updated {
			return a.restartCommander([]string{"setup"})
		}
		return a.setup(false)
	case "export", "import":
		return a.transfer(args)
	default:
		return fmt.Errorf("unknown command %q; run commander help", args[0])
	}
}

func (a *app) ensure() error {
	if err := os.MkdirAll(a.root, 0755); err != nil {
		return err
	}
	for _, name := range []string{".secrets", "tools", "workspaces"} {
		path := filepath.Join(a.root, name)
		if err := noSymlinks(a.root, path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if name == ".secrets" {
			if err := os.Chmod(path, 0700); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", ".gitignore"} {
		data := []byte("@AGENTS.md\n")
		if name == "AGENTS.md" {
			data = []byte(agentInstructions)
		} else if name == ".gitignore" {
			data = []byte("/.secrets/\n/tools/\n/workspaces/\n/profiles.json\n/commander\n/*.zip\n")
		}
		file, err := os.OpenFile(filepath.Join(a.root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func noSymlinks(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("path is outside Commander")
	}
	current := root
	for _, part := range append([]string{""}, strings.Split(rel, string(os.PathSeparator))...) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed path must not be a symbolic link: %s", current)
		}
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".commander-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (a *app) lock(name string) (func(), error) {
	path := filepath.Join(a.root, ".secrets", "."+name+".lock")
	if err := noSymlinks(a.root, path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another Commander operation is using this installation; finish it and retry")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (a *app) environment(tool string, extra map[string]string) []string {
	env := map[string]string{}
	for _, line := range os.Environ() {
		key, value, _ := strings.Cut(line, "=")
		env[key] = value
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_DEBUG", "GH_PROMPT_DISABLED", "HCLOUD_TOKEN", "HCLOUD_CONTEXT", "HCLOUD_DEBUG", "HCLOUD_ENDPOINT", "HCLOUD_HETZNER_ENDPOINT", "CLOUDSDK_AUTH_ACCESS_TOKEN", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT", "GOOGLE_APPLICATION_CREDENTIALS", "PYTHONPATH", "PYTHONHOME", "DOCKER_AUTH_CONFIG"} {
		delete(env, key)
	}
	for key := range env {
		if strings.HasPrefix(key, "CLIMICROSOFT365_") {
			delete(env, key)
		}
	}
	store := filepath.Join(a.root, ".secrets")
	env["GH_CONFIG_DIR"] = filepath.Join(store, ".config", "gh")
	env["HCLOUD_CONFIG"] = filepath.Join(store, ".config", "hcloud", "cli.toml")
	env["AZURE_CONFIG_DIR"] = filepath.Join(store, ".azure")
	env["CLOUDSDK_CONFIG"] = filepath.Join(store, ".config", "gcloud")
	env["DOCKER_CONFIG"] = filepath.Join(store, ".config", "docker")
	env["COMMANDER_WORKSPACE"] = filepath.Join(a.root, "workspaces")
	// Git inherits Commander credentials without changing the user's Git config.
	count, parseErr := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if parseErr != nil {
		count = 0
	}
	for _, value := range []string{"", "!" + quote(filepath.Join(a.root, "tools", "gh", "bin", "gh")) + " auth git-credential"} {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(count)] = "credential.https://github.com.helper"
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = value
		count++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	env["PATH"] = strings.Join([]string{filepath.Join(a.root, "tools", "node", "bin"), filepath.Join(a.packageDir("m365"), "node_modules", ".bin"), filepath.Join(a.packageDir("devcontainer"), "node_modules", ".bin"), filepath.Join(a.root, "tools", "gh", "bin"), filepath.Join(a.root, "tools", "hcloud"), filepath.Join(a.root, "tools", "docker"), env["PATH"]}, string(os.PathListSeparator))
	if python, err := a.python(); err == nil {
		env["CLOUDSDK_PYTHON"] = python
	}
	if tool == "az" {
		env["PYTHONPATH"] = filepath.Join(a.root, "tools", "azure")
	}
	if tool == "m365" {
		env["HOME"] = store
		env["XDG_CONFIG_HOME"] = filepath.Join(store, ".config")
	}
	for key, value := range extra {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func (a *app) toolCommand(tool string, args []string) (string, []string, error) {
	dir := filepath.Join(a.root, "tools")
	var path string
	switch tool {
	case "gh":
		path = filepath.Join(dir, "gh", "bin", "gh")
	case "hcloud":
		path = filepath.Join(dir, "hcloud", "hcloud")
	case "gcloud":
		path = filepath.Join(dir, "gcloud", "bin", "gcloud")
	case "docker":
		path = filepath.Join(dir, "docker", "docker")
	case "docker-compose":
		path = filepath.Join(dir, "compose", "docker-compose")
	case "m365", "devcontainer":
		path = filepath.Join(dir, "node", "bin", "node")
		entry := filepath.Join(a.packageDir("m365"), packageEntries["m365"])
		if tool == "devcontainer" {
			entry = filepath.Join(a.packageDir("devcontainer"), packageEntries["devcontainer"])
		}
		args = append([]string{entry}, args...)
	case "az":
		var err error
		path, err = a.python()
		if err != nil {
			return "", nil, err
		}
		args = append([]string{"-m", "azure.cli"}, args...)
	default:
		var err error
		path, err = exec.LookPath(tool)
		if err != nil {
			return "", nil, fmt.Errorf("%s is not installed", tool)
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, fmt.Errorf("%s is not installed; use commander exec -- %s to prepare it", tool, tool)
	}
	return path, args, nil
}

func (a *app) nativeRun(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
	if tool == "dockerhub" || tool == "confluence" {
		return a.tokenRun(tool, args, capture)
	}
	if err := a.prepareTool(tool, false); err != nil {
		return nil, &toolPreparationError{err}
	}
	if tool == "docker" || tool == "docker-compose" || tool == "devcontainer" {
		config, cleanup, err := a.dockerConfig("")
		if err != nil {
			return nil, err
		}
		defer cleanup()
		merged := map[string]string{}
		for k, v := range extra {
			merged[k] = v
		}
		merged["DOCKER_CONFIG"] = config
		extra = merged
	}
	return a.nativeRunContext(context.Background(), tool, args, capture, extra)
}

func (a *app) nativeRunContext(ctx context.Context, tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
	path, args, err := a.toolCommand(tool, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = a.environment(tool, extra)
	cmd.Stdin = a.in
	if capture {
		// Native errors can contain tokens. Never include captured output in errors.
		data, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%s could not complete the command; check its login with commander overview", tool)
		}
		return data, nil
	}
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	return nil, cmd.Run()
}

func (a *app) up(project string) error {
	base, err := filepath.EvalSymlinks(filepath.Join(a.root, "workspaces"))
	if err != nil {
		return err
	}
	if !filepath.IsAbs(project) {
		project = filepath.Join(base, project)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, project)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return errors.New("choose a project inside Commander workspaces")
	}
	if err := a.ensure(); err != nil {
		return err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	_, err = a.run("devcontainer", []string{"up", "--workspace-folder", project, "--docker-path", filepath.Join(a.root, "tools", "docker", "docker"), "--docker-compose-path", filepath.Join(a.root, "tools", "compose", "docker-compose")}, false, nil)
	return err
}
