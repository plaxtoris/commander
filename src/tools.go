package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type download struct {
	Version string
	URL     string
	SHA256  string
	Strip   int
	Raw     bool
}
type catalog struct {
	Packages map[string]string
	Archives map[string]download
}

var toolCatalog = catalog{
	Packages: map[string]string{
		"azure":        "2.92.0",
		"m365":         "12.0.0",
		"devcontainer": "0.90.0",
		"python":       "3.13.18",
	},
	Archives: map[string]download{
		"gh": {
			Version: "2.104.0",
			URL:     "https://github.com/cli/cli/releases/download/v2.104.0/gh_2.104.0_linux_amd64.tar.gz",
			SHA256:  "98cb101ad87dbbd3f6b2f2bfcbd43197ccd07643140121d8a9cfaa91eadbc24e",
			Strip:   1,
		},
		"hcloud": {
			Version: "1.70.2",
			URL:     "https://github.com/hetznercloud/cli/releases/download/v1.70.2/hcloud-linux-amd64.tar.gz",
			SHA256:  "b844bd28240112b0254183ba5266d0a0f59a6167c196636936459755efb88050",
		},
		"uv": {
			Version: "0.13.8",
			URL:     "https://github.com/astral-sh/uv/releases/download/0.13.8/uv-x86_64-unknown-linux-musl.tar.gz",
			SHA256:  "61fb26dabc4f0cf1d050fc6c67d40e43e4fae2994af21ebc1e050b233bf2b5ed",
			Strip:   1,
		},
		"node": {
			Version: "24.22.3",
			URL:     "https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.gz",
			SHA256:  "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2",
			Strip:   1,
		},
		"gcloud": {
			Version: "593.0.0",
			URL:     "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-593.0.0-linux-x86_64.tar.gz",
			SHA256:  "42c32b7553c712f71fcf6f124546c0519437f53755f47f9bd0c957998cd6b826",
			Strip:   1,
		},
		"docker": {
			Version: "29.9.3",
			URL:     "https://download.docker.com/linux/static/stable/x86_64/docker-29.9.3.tgz",
			SHA256:  "3fb5d1f13014c18cc54945bc177c58a9056a482dada031e17d12cf2e6922c973",
			Strip:   1,
		},
		"compose": {
			Version: "5.7.1",
			URL:     "https://github.com/docker/compose/releases/download/v5.7.1/docker-compose-linux-x86_64",
			SHA256:  "af426722a8af5de812fcee30cc1c39f0639f5f723cbadefb25e5d4db054064cf",
			Raw:     true,
		},
	},
}

var httpClient = &http.Client{Timeout: 20 * time.Minute}

func fetch(url, path, expected string) error {
	return fetchContext(context.Background(), url, path, expected)
}

func fetchContext(ctx context.Context, url, path, expected string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d from %s", response.StatusCode, url)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, 1<<30+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || n > 1<<30 {
		os.Remove(path)
		return errors.New("download was incomplete or too large")
	}
	if expected != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		os.Remove(path)
		return errors.New("download checksum did not match; no files were installed")
	}
	return nil
}

func extract(archive, destination string, strip int) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer z.Close()
	r := tar.NewReader(z)
	total := int64(0)
	count := 0
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		count++
		total += h.Size
		if count > 150000 || total > 3<<30 || h.Size < 0 {
			return errors.New("tool archive exceeds limits")
		}
		name := strings.TrimSuffix(h.Name, "/")
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return errors.New("tool archive contains an invalid path")
		}
		parts := strings.Split(name, "/")
		for _, part := range parts {
			if part == ".." {
				return errors.New("tool archive contains a parent path")
			}
		}
		if len(parts) <= strip {
			continue
		}
		name = filepath.Join(parts[strip:]...)
		if name == "." {
			continue
		}
		path := filepath.Join(destination, name)
		if err := noSymlinks(destination, path); err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			mode := os.FileMode(0644)
			if h.Mode&0111 != 0 {
				mode = 0755
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, r)
			closeErr := out.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) {
				return errors.New("tool archive contains an absolute link")
			}
			target := filepath.Join(filepath.Dir(path), h.Linkname)
			rel, err := filepath.Rel(destination, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return errors.New("tool archive link leaves its folder")
			}
			if err = os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			return fmt.Errorf("unsupported tool archive entry: %s", name)
		}
	}
}

func replaceDirectory(staged, destination string) error {
	old := destination + ".previous"
	if _, err := os.Lstat(old); err == nil {
		if err := noSymlinks(filepath.Dir(destination), old); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(old, ".commander-version")); err != nil {
			return errors.New("unexpected backup in tool folder: " + old)
		}
		if _, err := os.Stat(destination); os.IsNotExist(err) {
			if err := os.Rename(old, destination); err != nil {
				return err
			}
		} else if err := os.RemoveAll(old); err != nil {
			return err
		}
	}
	existed := false
	if _, err := os.Stat(destination); err == nil {
		if err = os.Rename(destination, old); err != nil {
			return err
		}
		existed = true
	}
	if err := os.Rename(staged, destination); err != nil {
		if existed {
			os.Rename(old, destination)
		}
		return err
	}
	if existed {
		return os.RemoveAll(old)
	}
	return nil
}

func (a *app) installArchive(name string, spec download, force bool) error {
	destination := filepath.Join(a.root, "tools", name)
	if err := noSymlinks(a.root, destination); err != nil {
		return err
	}
	stamp, err := os.ReadFile(filepath.Join(destination, ".commander-version"))
	if !force && err == nil && string(stamp) == spec.SHA256 {
		entry := archiveEntries[name]
		if entry == "" {
			return nil
		}
		if info, err := os.Stat(filepath.Join(destination, entry)); err == nil && info.Mode().Perm()&0111 != 0 {
			return nil
		}
	}
	staging, err := os.MkdirTemp(filepath.Join(a.root, "tools"), ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	archive := filepath.Join(staging, "download")
	if err = fetch(spec.URL, archive, spec.SHA256); err != nil {
		return err
	}
	unpacked := filepath.Join(staging, "tool")
	if err = os.Mkdir(unpacked, 0755); err != nil {
		return err
	}
	if spec.Raw {
		if err = os.Rename(archive, filepath.Join(unpacked, "docker-compose")); err != nil {
			return err
		}
		if err = os.Chmod(filepath.Join(unpacked, "docker-compose"), 0755); err != nil {
			return err
		}
	} else if err = extract(archive, unpacked, spec.Strip); err != nil {
		return err
	}
	if name == "docker" {
		// The engine belongs to the host. Commander only needs its client.
		entries, err := os.ReadDir(unpacked)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() != "docker" {
				if err := os.RemoveAll(filepath.Join(unpacked, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	if err = os.WriteFile(filepath.Join(unpacked, ".commander-version"), []byte(spec.SHA256), 0644); err != nil {
		return err
	}
	return replaceDirectory(unpacked, destination)
}

func (a *app) python() (string, error) {
	matches, _ := filepath.Glob(filepath.Join(a.root, "tools", "python", "cpython-"+a.catalog.Packages["python"]+"-*", "bin", "python3"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if info, err := os.Stat(matches[i]); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return matches[i], nil
		}
	}
	return "", errors.New("Python runtime is not ready; run commander setup")
}

func (a *app) installCommand(path string, args []string, extra map[string]string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = a.environment("setup", extra)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer failed: %v", err)
	}
	return nil
}

func (a *app) installPython() error {
	if _, err := a.python(); err == nil {
		return nil
	}
	return a.installCommand(filepath.Join(a.root, "tools", "uv", "uv"), []string{"--quiet", "python", "install", a.catalog.Packages["python"], "--no-bin"}, map[string]string{"UV_PYTHON_INSTALL_DIR": filepath.Join(a.root, "tools", "python"), "UV_CACHE_DIR": filepath.Join(a.root, "tools", ".cache", "uv")})
}

// Keep packages independent. Reuse the former shared npm directory until
// a package needs updating.
var packageEntries = map[string]string{
	"m365":         "node_modules/@pnp/cli-microsoft365/dist/index.js",
	"devcontainer": "node_modules/@devcontainers/cli/devcontainer.js",
	"azure":        "azure/cli/__main__.py",
}

func (a *app) packageDir(name string) string {
	dir := filepath.Join(a.root, "tools", name)
	if _, err := os.Stat(dir); !os.IsNotExist(err) || name == "azure" {
		return dir
	}
	legacy := filepath.Join(a.root, "tools", "npm")
	stamp, _ := os.ReadFile(filepath.Join(legacy, ".commander-version"))
	versions := strings.Split(string(stamp), "/")
	index := 0
	if name == "devcontainer" {
		index = 1
	}
	if len(versions) == 2 && versions[index] == a.catalog.Packages[name] {
		return legacy
	}
	return dir
}

func (a *app) packageReady(name string) bool {
	dir := a.packageDir(name)
	stamp, _ := os.ReadFile(filepath.Join(dir, ".commander-version"))
	if filepath.Base(dir) == "npm" {
		versions := strings.Split(string(stamp), "/")
		index := 0
		if name == "devcontainer" {
			index = 1
		}
		if len(versions) != 2 {
			return false
		}
		stamp = []byte(versions[index])
	}
	info, err := os.Stat(filepath.Join(dir, packageEntries[name]))
	return string(stamp) == a.catalog.Packages[name] && err == nil && info.Mode().IsRegular()
}

func (a *app) installPackage(name string, force bool) error {
	if !force && a.packageReady(name) {
		return nil
	}
	dir := filepath.Join(a.root, "tools")
	if err := noSymlinks(a.root, filepath.Join(dir, name)); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(dir, ".packages-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if name == "azure" {
		python, e := a.python()
		if e != nil {
			return e
		}
		err = a.installCommand(filepath.Join(dir, "uv", "uv"), []string{"--quiet", "pip", "install", "--python", python, "--target", stage, "azure-cli==" + a.catalog.Packages[name]}, map[string]string{"UV_CACHE_DIR": filepath.Join(dir, ".cache", "uv")})
	} else {
		pkg := "@pnp/cli-microsoft365"
		if name == "devcontainer" {
			pkg = "@devcontainers/cli"
		}
		err = a.installCommand(filepath.Join(dir, "node", "bin", "node"), []string{filepath.Join(dir, "node", "lib", "node_modules", "npm", "bin", "npm-cli.js"), "install", "--prefix", stage, "--omit=dev", "--no-audit", "--no-fund", "--loglevel=error", pkg + "@" + a.catalog.Packages[name]}, map[string]string{"npm_config_cache": filepath.Join(dir, ".cache", "npm"), "npm_config_update_notifier": "false"})
	}
	if err != nil {
		return errors.New("installation failed; retry the action")
	}
	if err = os.WriteFile(filepath.Join(stage, ".commander-version"), []byte(a.catalog.Packages[name]), 0644); err != nil {
		return err
	}
	return replaceDirectory(stage, filepath.Join(dir, name))
}

var toolDependencies = map[string][]string{
	"git": {"git"}, "gh": {"git", "gh"}, "hcloud": {"hcloud"},
	"az":     {"uv", "python", "azure"},
	"gcloud": {"uv", "python", "gcloud"},
	"m365":   {"node", "m365"},
	"docker": {"docker"}, "docker-compose": {"compose"},
	"devcontainer": {"git", "node", "docker", "compose", "devcontainer"},
}
var managedTools = []string{"git", "gh", "az", "m365", "hcloud", "gcloud", "docker", "docker-compose", "devcontainer"}
var archiveEntries = map[string]string{"gh": "bin/gh", "hcloud": "hcloud", "node": "bin/node", "uv": "uv", "gcloud": "bin/gcloud", "docker": "docker", "compose": "docker-compose"}

type toolPreparationError struct{ error }

func toolLabel(tool string) string {
	for service, command := range serviceTools {
		if tool == command {
			return serviceName(service)
		}
	}
	if tool == "devcontainer" {
		return "project tools"
	}
	if tool == "docker" {
		return "docker"
	}
	if tool == "docker-compose" {
		return "docker compose"
	}
	return tool
}

func (a *app) dependencyReady(name string) bool {
	if name == "git" {
		_, err := exec.LookPath("git")
		return err == nil
	}
	if name == "python" {
		_, err := a.python()
		return err == nil
	}
	if _, ok := packageEntries[name]; ok {
		return a.packageReady(name)
	}
	spec := a.catalog.Archives[name]
	dir := filepath.Join(a.root, "tools", name)
	stamp, _ := os.ReadFile(filepath.Join(dir, ".commander-version"))
	info, err := os.Stat(filepath.Join(dir, archiveEntries[name]))
	return string(stamp) == spec.SHA256 && err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func (a *app) toolUsed(tool string) bool {
	if _, err := os.Stat(filepath.Join(a.root, "tools", ".requested", tool)); err == nil {
		return true
	}
	deps := toolDependencies[tool]
	if len(deps) == 0 {
		return false
	}
	name := deps[len(deps)-1]
	if _, err := os.Stat(filepath.Join(a.root, "tools", name)); err == nil {
		return true
	}
	if name == "m365" || name == "devcontainer" {
		_, err := os.Stat(filepath.Join(a.root, "tools", "npm", packageEntries[name]))
		return err == nil
	}
	return false
}

func (a *app) prepareTool(tool string, force bool) error {
	deps, managed := toolDependencies[tool]
	if !managed {
		return nil
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("Commander supports Linux and WSL on x86_64")
	}
	if err := a.ensure(); err != nil {
		return err
	}
	unlock, err := a.lock("setup")
	if err != nil {
		return err
	}
	defer unlock()
	marker := filepath.Join(a.root, "tools", ".requested", tool)
	if err = noSymlinks(a.root, marker); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		return err
	}
	if err = atomicWrite(marker, []byte{}, 0600); err != nil {
		return err
	}
	announced := false
	for _, name := range deps {
		if !force && a.dependencyReady(name) {
			continue
		}
		if !announced {
			fmt.Fprintln(a.errOut, "preparing "+toolLabel(tool)+"…")
			announced = true
		}
		switch {
		case name == "git":
			err = a.installGit()
		case name == "python":
			err = a.installPython()
		case packageEntries[name] != "":
			err = a.installPackage(name, force)
		default:
			err = a.installArchive(name, a.catalog.Archives[name], force)
		}
		if err != nil {
			return fmt.Errorf("%s is not ready; retry the action: %w", tool, err)
		}
	}
	if announced {
		fmt.Fprintln(a.errOut, toolLabel(tool)+" ready.")
	}
	return os.RemoveAll(filepath.Join(a.root, "tools", ".cache"))
}

// Git stays a system package: reuse it, or install it with the native package
// manager. Never elevate Commander itself or change ownership of its folder.
func gitInstallCommands(root, interactive bool) ([][]string, error) {
	managers := []struct {
		name string
		args [][]string
	}{
		{"apt-get", [][]string{{"update"}, {"install", "-y", "--no-install-recommends", "git", "ca-certificates"}}},
		{"dnf", [][]string{{"install", "-y", "git", "ca-certificates"}}},
		{"yum", [][]string{{"install", "-y", "git", "ca-certificates"}}},
		{"zypper", [][]string{{"--non-interactive", "install", "--no-recommends", "git", "ca-certificates"}}},
		{"apk", [][]string{{"add", "--no-cache", "git", "ca-certificates"}}},
		{"pacman", [][]string{{"-S", "--needed", "--noconfirm", "git", "ca-certificates"}}},
	}
	for _, manager := range managers {
		path, err := exec.LookPath(manager.name)
		if err != nil {
			continue
		}
		prefix := []string{}
		if !root {
			sudo, err := exec.LookPath("sudo")
			if err != nil {
				return nil, errors.New("git is missing and sudo is unavailable; install git with your system package manager")
			}
			prefix = append(prefix, sudo)
			if !interactive {
				prefix = append(prefix, "-n")
			}
			prefix = append(prefix, "--")
		}
		commands := make([][]string, 0, len(manager.args))
		for _, args := range manager.args {
			command := append(append([]string{}, prefix...), path)
			commands = append(commands, append(command, args...))
		}
		return commands, nil
	}
	return nil, errors.New("git is missing; install it with your system package manager (no supported package manager found)")
}

func (a *app) installGit() error {
	if a.dependencyReady("git") {
		return nil
	}
	commands, err := gitInstallCommands(os.Geteuid() == 0, a.terminal())
	if err != nil {
		return err
	}
	fmt.Fprintln(a.errOut, "installing system git and CA certificates; sudo may request your password…")
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, a.errOut, a.errOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git installation failed (%v); retry in a terminal with sudo access, or install git yourself", err)
		}
	}
	if !a.dependencyReady("git") {
		return errors.New("git installation finished but git is still missing from PATH")
	}
	return nil
}

// Import commits the profile before preparing anything. This same operation can
// be retried without re-importing, replacing logins or switching active accounts.
func (a *app) prepareProfile(name string) error {
	name, _, err := a.profile(name)
	if err != nil {
		return err
	}
	defer a.dockerStatus()
	if _, err = a.run("git", []string{"--version"}, true, nil); err == nil {
		err = a.describe(name, false)
	}
	if err != nil {
		return fmt.Errorf("profile %q is saved, but preparation needs attention: %v; retry ./commander setup %s without importing again", name, err, quote(name))
	}
	fmt.Fprintf(a.out, "profile ready: %s · tools prepared; account status checked\n", clean(name))
	return nil
}

func (a *app) setupProfile(name string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if err := a.refreshAgentInstructions(); err != nil {
		return err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	return a.prepareProfile(name)
}

// Checking the engine must not install container tools or use a registry login.
func (a *app) dockerStatus() {
	path, _, err := a.toolCommand("docker", nil)
	if err != nil {
		path, err = exec.LookPath("docker")
	}
	if err != nil {
		fmt.Fprintln(a.out, "docker engine: not checked (CLI not installed); project containers need Docker Engine/Desktop installed separately")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "info", "--format", "{{.ServerVersion}}")
	cmd.Env = a.environment("docker", nil)
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(a.out, "docker engine: unavailable; install/start Docker Engine/Desktop and enable access for project containers")
		return
	}
	fmt.Fprintln(a.out, "docker engine: ready")
}

// Setup refreshes agent instructions and repairs only used tools, including
// interrupted installations. After an update, the restarted binary supplies
// its own embedded instructions.
func (a *app) setup(force bool) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if err := a.refreshAgentInstructions(); err != nil {
		return err
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	var failures []error
	for _, tool := range managedTools {
		if a.toolUsed(tool) {
			if err := a.prepareTool(tool, force); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (a *app) doctor() error {
	fmt.Fprintln(a.out, "commander", build, "\nfolder:", a.root, "\nprojects:", filepath.Join(a.root, "workspaces"))
	failed := false
	for _, tool := range managedTools {
		if tool != "git" && !a.toolUsed(tool) {
			fmt.Fprintln(a.out, tool+": not installed · available on demand")
			continue
		}
		args := []string{"--version"}
		if tool == "m365" || tool == "hcloud" {
			args = []string{"version"}
		}
		data, err := a.nativeRunContext(context.Background(), tool, args, true, nil)
		if err != nil {
			fmt.Fprintln(a.out, tool+": not ready")
			failed = true
			continue
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		if len(line) > 140 {
			line = line[:140]
		}
		fmt.Fprintf(a.out, "%s: %s\n", tool, clean(line))
	}
	if a.toolUsed("docker") {
		if _, err := a.nativeRunContext(context.Background(), "docker", []string{"info", "--format", "{{.ServerVersion}}"}, true, nil); err != nil {
			fmt.Fprintln(a.out, "docker engine: not running · needed for project containers")
		}
	}
	fmt.Fprintln(a.out, "dockerhub: built in\nconfluence: built in")
	if failed {
		return errors.New("run ./commander setup to finish installing tools")
	}
	return nil
}
