package main

import (
	"archive/tar"
	"compress/gzip"
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
	response, err := httpClient.Get(url)
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
		entry := map[string]string{"gh": "bin/gh", "hcloud": "hcloud", "node": "bin/node", "uv": "uv", "gcloud": "bin/gcloud", "docker": "docker", "compose": "docker-compose"}[name]
		if entry == "" {
			return nil
		}
		if info, err := os.Stat(filepath.Join(destination, entry)); err == nil && info.Mode().Perm()&0111 != 0 {
			return nil
		}
	}
	fmt.Fprintln(a.out, "Installing", name, spec.Version+"...")
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
	cmd.Stdin = a.in
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	return cmd.Run()
}

func (a *app) installPython() error {
	if _, err := a.python(); err == nil {
		return nil
	}
	fmt.Fprintln(a.out, "Installing Python runtime...")
	return a.installCommand(filepath.Join(a.root, "tools", "uv", "uv"), []string{"--quiet", "python", "install", a.catalog.Packages["python"], "--no-bin"}, map[string]string{"UV_PYTHON_INSTALL_DIR": filepath.Join(a.root, "tools", "python"), "UV_CACHE_DIR": filepath.Join(a.root, "tools", ".cache", "uv")})
}

func (a *app) installPackages(force bool) error {
	tools := filepath.Join(a.root, "tools")
	for _, name := range []string{"npm", "azure"} {
		version := a.catalog.Packages["azure"]
		if name == "npm" {
			version = a.catalog.Packages["m365"] + "/" + a.catalog.Packages["devcontainer"]
		}
		stamp := filepath.Join(tools, name, ".commander-version")
		data, _ := os.ReadFile(stamp)
		if !force && string(data) == version {
			entries := []string{"azure/cli/__main__.py"}
			if name == "npm" {
				entries = []string{"node_modules/@pnp/cli-microsoft365/dist/index.js", "node_modules/@devcontainers/cli/devcontainer.js"}
			}
			ready := true
			for _, entry := range entries {
				if _, err := os.Stat(filepath.Join(tools, name, entry)); err != nil {
					ready = false
				}
			}
			if ready {
				continue
			}
		}
		stage, err := os.MkdirTemp(tools, ".packages-")
		if err != nil {
			return err
		}
		var installErr error
		if name == "npm" {
			fmt.Fprintln(a.out, "Installing Microsoft 365 and Dev Containers...")
			installErr = a.installCommand(filepath.Join(tools, "node", "bin", "node"), []string{filepath.Join(tools, "node", "lib", "node_modules", "npm", "bin", "npm-cli.js"), "install", "--prefix", stage, "--omit=dev", "--no-audit", "--no-fund", "--loglevel=error", "@pnp/cli-microsoft365@" + a.catalog.Packages["m365"], "@devcontainers/cli@" + a.catalog.Packages["devcontainer"]}, map[string]string{"npm_config_cache": filepath.Join(tools, ".cache", "npm"), "npm_config_update_notifier": "false"})
		} else {
			fmt.Fprintln(a.out, "Installing Azure CLI...")
			python, e := a.python()
			if e != nil {
				os.RemoveAll(stage)
				return e
			}
			installErr = a.installCommand(filepath.Join(tools, "uv", "uv"), []string{"--quiet", "pip", "install", "--python", python, "--target", stage, "azure-cli==" + a.catalog.Packages["azure"]}, map[string]string{"UV_CACHE_DIR": filepath.Join(tools, ".cache", "uv")})
		}
		if installErr == nil {
			installErr = os.WriteFile(filepath.Join(stage, ".commander-version"), []byte(version), 0644)
		}
		if installErr == nil {
			installErr = replaceDirectory(stage, filepath.Join(tools, name))
		}
		os.RemoveAll(stage)
		if installErr != nil {
			return installErr
		}
	}
	return nil
}

func (a *app) setup(force bool) error {
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
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(a.out, "Installing Git...")
		apt, err := exec.LookPath("apt-get")
		if err != nil {
			return errors.New("install Git with your system package manager, then run commander setup")
		}
		command := apt
		prefix := []string{}
		if os.Geteuid() != 0 {
			command, err = exec.LookPath("sudo")
			if err != nil {
				return errors.New("Git is missing; install it and run commander setup")
			}
			prefix = []string{apt}
		}
		if err = a.installCommand(command, append(append([]string{}, prefix...), "update"), nil); err != nil {
			return err
		}
		if err = a.installCommand(command, append(prefix, "install", "-y", "git", "ca-certificates"), nil); err != nil {
			return err
		}
	}
	for _, name := range []string{"gh", "hcloud", "node", "uv", "gcloud", "docker", "compose"} {
		spec, ok := a.catalog.Archives[name]
		if !ok {
			return fmt.Errorf("no supported download for %s", name)
		}
		if err = a.installArchive(name, spec, force); err != nil {
			return fmt.Errorf("%s setup: %w; run commander setup to resume", name, err)
		}
	}
	if err = a.installPython(); err != nil {
		return err
	}
	if err = a.installPackages(force); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(a.root, "tools", ".cache")); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Commander ready. Projects:", filepath.Join(a.root, "workspaces"))
	return nil
}

func (a *app) doctor() error {
	fmt.Fprintln(a.out, "Commander", build, "\nFolder:", a.root, "\nProjects:", filepath.Join(a.root, "workspaces"))
	failed := false
	for _, tool := range []string{"git", "gh", "az", "m365", "hcloud", "gcloud", "docker", "devcontainer"} {
		args := []string{"--version"}
		if tool == "m365" || tool == "hcloud" {
			args = []string{"version"}
		}
		data, err := a.run(tool, args, true, nil)
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
	if _, err := a.run("docker", []string{"info", "--format", "{{.ServerVersion}}"}, true, nil); err != nil {
		fmt.Fprintln(a.out, "Docker engine: not running. Only needed for project containers.")
	}
	if failed {
		return errors.New("run ./commander setup to finish installing tools")
	}
	return nil
}

var releaseURL = "https://github.com/plaxtoris/commander/releases/download/latest/"

func (a *app) update() error {
	if err := a.ensure(); err != nil {
		return err
	}
	unlock, err := a.lock("update")
	if err != nil {
		return err
	}
	defer unlock()
	stage, err := os.MkdirTemp(a.root, ".update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	const name = "commander"
	sums := filepath.Join(stage, "SHA256SUMS")
	err = fetch(releaseURL+"SHA256SUMS", sums, "")
	if err != nil {
		// Private repositories use the user's existing GitHub login without exposing tokens.
		_, err = a.run("gh", []string{"release", "download", "latest", "--repo", "plaxtoris/commander", "--pattern", "SHA256SUMS", "--pattern", name, "--dir", stage}, false, nil)
		if err != nil {
			return errors.New("could not download latest; the repository may be private or the network unavailable")
		}
	}
	data, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	expected := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			expected = fields[0]
		}
	}
	if len(expected) != 64 {
		return errors.New("release checksum is missing")
	}
	path := filepath.Join(stage, name)
	if _, err = os.Stat(path); os.IsNotExist(err) {
		if err = fetch(releaseURL+name, path, expected); err != nil {
			return err
		}
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != expected {
		return errors.New("release checksum did not match")
	}
	if err = os.Chmod(path, 0755); err != nil {
		return err
	}
	check := exec.Command(path, "version")
	check.Env = a.environment("setup", nil)
	if err = check.Run(); err != nil {
		return errors.New("downloaded Commander could not run on this system")
	}
	if err = os.Rename(path, filepath.Join(a.root, "commander")); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Commander updated. Preparing its tools...")
	cmd := exec.Command(filepath.Join(a.root, "commander"), "setup")
	cmd.Env = a.environment("setup", nil)
	cmd.Stdin = a.in
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	return cmd.Run()
}
