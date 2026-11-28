package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
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

	"golang.org/x/sys/unix"
)

var build = "development"
var buildVersion = "dev"

func main() {
	syscall.Umask(0077)
	if len(os.Args) > 1 && os.Args[1] == "bridge-client" {
		os.Exit(bridgeClient(os.Args[2:]))
	}
	path, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := filepath.Dir(path)
	root := os.Getenv("COMMANDER_HOME")
	// make build links the local command to the host's architecture binary.
	// A session ID belongs only to the installation that created it.
	alias, _ := os.Readlink(filepath.Join(dir, "commander"))
	localBuild := alias == releaseAsset && filepath.Base(path) == releaseAsset
	if localBuild && root != "" && filepath.Clean(root) != dir {
		os.Unsetenv("COMMANDER_SESSION")
		// Outer session wrappers would dispatch tools back to the original login.
		paths := filepath.SplitList(os.Getenv("PATH"))
		keep := paths[:0]
		for _, p := range paths {
			if !strings.HasPrefix(p, filepath.Join(root, ".sessions")+"/") && !strings.HasPrefix(p, filepath.Join(root, "tools")+"/") {
				keep = append(keep, p)
			}
		}
		os.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
		if strings.HasPrefix(os.Getenv("CLOUDSDK_PYTHON"), filepath.Join(root, "tools")+"/") {
			os.Unsetenv("CLOUDSDK_PYTHON")
		}
	}
	if root == "" || localBuild {
		root = dir
	}
	a, err := newApp(root, os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		a.localBuild = localBuild
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
	localBuild  bool
	in          *os.File
	out, errOut io.Writer
	catalog     catalog
	session     *session
	account     string
	// Tests replace native commands with local fixtures. Production uses nativeRun.
	run        func(string, []string, bool, map[string]string) ([]byte, error)
	releaseRun func(context.Context, []string) ([]byte, error)
	latest     func(context.Context, string, catalog) (toolRelease, error)
}

func newApp(root string, in *os.File, out, errOut io.Writer) (*app, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	a := &app{root: root, in: in, out: out, errOut: errOut, catalog: toolCatalog}
	a.latest = latestTool
	a.catalog = a.installedCatalog(a.catalog)
	a.run = a.nativeRun
	a.releaseRun = func(ctx context.Context, args []string) ([]byte, error) {
		if a.session == nil || !a.session.allows("plaxtoris/commander") {
			return nil, errors.New("private updates require a session selecting plaxtoris/commander")
		}
		m, err := a.repoMember("plaxtoris/commander")
		if err != nil {
			return nil, err
		}
		token, err := a.githubToken(m)
		if err != nil {
			return nil, err
		}
		return a.nativeRunContext(ctx, "gh", args, true, map[string]string{"GH_HOST": "github.com", "GH_PROMPT_DISABLED": "1", "GH_TOKEN": token})
	}
	return a, nil
}

const help = `commander · accounts, tools and repositories for agents

Sessions
  commander                                open the session menu
  commander menu                           open the local menu without updating
  commander --session NAME code            open a fresh native agent
  commander session [list]                 list sessions
  commander session new NAME [--accounts service=user,...] [--repos owner/*,...]
  commander session edit NAME [--accounts ...] [--repos ...]
  commander session rename NAME NEW        keeps open shells, tasks and containers
  commander session delete NAME            removes it and its containers
  commander session shell NAME             opens a shell with its selection
  commander export                         selects sessions and memory for commander.zip
  commander export SESSION                 saves a session, its logins, memory and agent settings
  commander import [FILE.zip]              picks or opens an archive to import
  commander context                        shows the session scope and selectors
  commander overview [SESSION]             checks account status

Work
  commander exec [--account ID] -- TOOL    runs a tool with the selected login
  commander task new PROJECT NAME [BASE]   creates an isolated worktree
  commander task finish NAME -- CHECK      checks, integrates and cleans up
  commander up PROJECT                     builds and connects a trusted devcontainer
  commander container ID -- COMMAND        runs a command in that container
  commander exec -- dockerhub whoami       verifies a built in client login
  commander exec -- confluence api PATH    calls a built in client API

Installation
  commander config codex|claude|pi [FILE|-] reads or replaces shared agent settings
  commander memory [write REVISION]        reads or updates shared preferences
  commander setup                          repairs used tools
  commander update                         updates Commander and used tools
  commander doctor                         checks the installation
  commander version                        shows the installed build

PROJECT is SESSION/REPO under workspaces. Outside a session shell put
--session NAME right after commander; put --account SELECTOR there too for up
or task when a repository has several matching GitHub logins. Services are github,
azure, office, hetzner, gcloud, dockerhub, confluence, codex, claude and openai-compatible.
Selections limit what trusted tools use. They grant no permission to act.
`

func (a *app) command(args []string) error {
	id := os.Getenv("COMMANDER_SESSION")
	for len(args) >= 2 && (args[0] == "--session" || args[0] == "--account") {
		if args[0] == "--session" {
			id = args[1]
		} else {
			a.account = args[1]
		}
		args = args[2:]
	}
	if id != "" {
		s, err := a.findSession(id)
		if err != nil {
			return err
		}
		a.session = &s
	}
	if len(args) == 0 {
		if !a.terminal() {
			return errors.New("open Commander in a terminal; use help, context or overview for text output")
		}
		return a.start()
	}
	switch args[0] {
	case "menu", "code":
		if len(args) != 1 {
			return errors.New("unexpected argument")
		}
		if !a.terminal() {
			return errTerminal
		}
		if err := a.ensure(); err != nil {
			return err
		}
		if args[0] == "menu" {
			if err := a.refreshAgentInstructions(); err != nil {
				return err
			}
			return a.menu()
		}
		if a.session == nil {
			return errNoSession
		}
		return a.code(*a.session)
	case "bridge-serve":
		return a.serveBridge()
	case "task":
		return a.taskCommand(args[1:])
	case "container":
		return a.containerCommand(args[1:])
	case "session":
		if err := a.ensure(); err != nil {
			return err
		}
		return a.sessionCommand(args[1:])
	case "git-credential":
		return a.gitCredential(args[1:])
	case "help", "--help", "-h":
		fmt.Fprint(a.out, help)
		return nil
	case "version", "--version":
		fmt.Fprintln(a.out, "Commander", build, runtime.GOOS+"/"+runtime.GOARCH)
		return nil
	case "config":
		return a.codingConfig(args[1:])
	case "memory":
		return a.memory(args[1:])
	case "setup":
		if len(args) != 1 {
			return errors.New("usage: commander setup")
		}
		return a.setup()
	case "context":
		if len(args) != 1 {
			return errors.New("usage: commander context")
		}
		return a.describe("", true)
	case "overview":
		if len(args) > 2 {
			return errors.New("usage: commander overview [SESSION]")
		}
		return a.describe(strings.Join(args[1:], ""), false)
	case "exec":
		args = args[1:]
		if len(args) >= 2 && args[0] == "--account" {
			a.account, args = args[1], args[2:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			return errors.New("usage: commander exec -- TOOL [ARGS...]")
		}
		if err := a.ensure(); err != nil {
			return err
		}
		return a.execute(args[0], args[1:])
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
			return a.restartCommander([]string{"update-tools"})
		}
		fallthrough
	case "update-tools":
		if err := a.ensure(); err != nil {
			return err
		}
		if err := a.refreshAgentInstructions(); err != nil {
			return err
		}
		a.cleanupAfterUpdate()
		return a.refreshTools()
	case "update-check":
		return a.backgroundUpdate()
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
	// Older installations may have an empty Claude project directory.
	syscall.Rmdir(filepath.Join(a.root, ".claude"))
	for _, name := range []string{".secrets", ".sessions", "tools", "workspaces"} {
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
			data = []byte("/.secrets/\n/.sessions/\n/tools/\n/workspaces/\n/MEMORY.md\n/commander\n/.update-*/\n/*.zip\n")
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

func (a *app) refreshAgentInstructions() error {
	a.pruneSessionWrappers()
	path := filepath.Join(a.root, "AGENTS.md")
	if err := noSymlinks(a.root, path); err != nil {
		return err
	}
	return writeGenerated(path, []byte(agentInstructions), 0644)
}

func writeGenerated(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode() == mode && info.Size() == int64(len(data)) {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			return nil
		}
	}
	return atomicWrite(path, data, mode)
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

// Installation locks live in .secrets. Long operations fail fast while another
// one runs; short state changes such as sessions and memory wait their turn.
func (a *app) lock(name string) (func(), error)     { return a.lockFile(name, false) }
func (a *app) waitLock(name string) (func(), error) { return a.lockFile(name, true) }

func (a *app) lockFile(name string, wait bool) (func(), error) {
	unlock, err := a.installationLock(name, false, wait)
	if err != nil {
		return nil, errors.New("another Commander operation is using this installation; finish it and retry")
	}
	return unlock, nil
}

func (a *app) installationLock(name string, shared, wait bool) (func(), error) {
	if !a.runtimeMigrated() {
		path := filepath.Join(a.root, ".secrets", "."+name+".lock")
		if err := noSymlinks(a.root, path); err != nil {
			return nil, err
		}
		return fileLock(path, shared, wait)
	}
	path := filepath.Join(a.root, ".secrets", ".locks")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	// Each name owns a byte range. OFD locks also contend between separate
	// opens in one process and do not allocate bytes in the lock file.
	sum := sha256.Sum256([]byte(name))
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET, Start: int64(binary.BigEndian.Uint64(sum[:8]) >> 1), Len: 1}
	if shared {
		lock.Type = unix.F_RDLCK
	}
	command := unix.F_OFD_SETLK
	if wait {
		command = unix.F_OFD_SETLKW
	}
	for {
		err = unix.FcntlFlock(uintptr(fd), command, &lock)
		if err != unix.EINTR {
			break
		}
	}
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EAGAIN) {
		err = unix.EWOULDBLOCK
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return func() { unix.Close(fd) }, nil
}

func fileLock(path string, shared, wait bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	if !wait {
		mode |= syscall.LOCK_NB
	}
	if err = syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
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
	if a.session != nil {
		store = filepath.Join(a.root, ".sessions", a.session.ID, "empty")
		env["COMMANDER_SESSION"] = a.session.ID
	}
	env["GH_CONFIG_DIR"] = filepath.Join(store, ".config", "gh")
	env["HCLOUD_CONFIG"] = filepath.Join(store, ".config", "hcloud", "cli.toml")
	env["AZURE_CONFIG_DIR"] = filepath.Join(store, ".azure")
	env["CLOUDSDK_CONFIG"] = filepath.Join(store, ".config", "gcloud")
	env["DOCKER_CONFIG"] = filepath.Join(store, ".config", "docker")
	env["COMMANDER_WORKSPACE"] = filepath.Join(a.root, "workspaces")
	env["COMMANDER_HOME"] = a.root
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
	if a.session != nil {
		env["PATH"] = a.sessionBin() + string(os.PathListSeparator) + env["PATH"]
		selector := ""
		if a.account != "" {
			selector = " --account " + quote(a.account)
		}
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(count-1)] = "!" + quote(filepath.Join(a.root, "commander")) + " --session " + quote(a.session.ID) + selector + " git-credential"
		env["GIT_CONFIG_KEY_"+strconv.Itoa(count)] = "credential.useHttpPath"
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = "true"
		count++
		// Existing SSH remotes use the same selected HTTPS account, without
		// borrowing an unrelated SSH agent or changing the repository config.
		for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/"} {
			env["GIT_CONFIG_KEY_"+strconv.Itoa(count)] = "url.https://github.com/.insteadOf"
			env["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = prefix
			count++
		}
		env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	}
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
	case "claude":
		path = filepath.Join(a.packageDir(tool), packageEntries[tool])
	case "pi", "codex", "m365", "devcontainer":
		path = filepath.Join(dir, "node", "bin", "node")
		args = append([]string{filepath.Join(a.packageDir(tool), packageEntries[tool])}, args...)
	case "az":
		var err error
		path, err = a.python()
		if err != nil {
			return "", nil, err
		}
		args = append([]string{"-m", "azure.cli"}, args...)
	default:
		var err error
		path, err = a.lookupTool(tool)
		if err != nil {
			return "", nil, fmt.Errorf("%s is not installed", tool)
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, fmt.Errorf("%s is not installed; use commander exec -- %s to prepare it", tool, tool)
	}
	return path, args, nil
}

func (a *app) lookupTool(tool string) (string, error) {
	if strings.ContainsRune(tool, os.PathSeparator) {
		return exec.LookPath(tool)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || strings.HasPrefix(dir, filepath.Join(a.root, ".sessions")+string(os.PathSeparator)) {
			continue
		}
		path := filepath.Join(dir, tool)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

func (a *app) nativeRun(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
	if tool == "dockerhub" || tool == "confluence" {
		return a.tokenRun(tool, args, capture)
	}
	if err := a.prepareTool(tool); err != nil {
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
	if tool == "git" {
		cmd = gitCommand(ctx, path, args...)
	}
	cmd.Env = a.environment(tool, extra)
	cmd.Stdin = a.in
	if capture {
		if tool == "devcontainer" {
			cmd.Stderr = a.errOut
		}
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

// Git checkouts need normal source permissions for isolated container workers.
// Change only the child mask; account operations keep Commander's private mask.
func gitCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `umask 022; exec "$@"`, "git", path}, args...)...)
}
