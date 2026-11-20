package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var releaseURL = "https://github.com/plaxtoris/commander/releases/download/latest/"

// Each release asset is named for the machine it runs on.
var releaseAsset = "commander-" + machine
var releaseCheckTimeout = 8 * time.Second

func (a *app) start() error {
	// A restarted process prepares the new catalog immediately, without another
	// self update check or an update loop if the rolling release changes again.
	restarted := os.Getenv("COMMANDER_RESTARTED") == a.root
	os.Unsetenv("COMMANDER_RESTARTED")
	checked := restarted
	if !restarted {
		fmt.Fprintln(a.errOut, "updating…")
		updated, err := a.update()
		if err != nil {
			fmt.Fprintln(a.errOut, "update skipped:", err)
		} else if updated {
			return a.restartCommander(nil)
		} else {
			checked = true
		}
	}
	if err := a.ensure(); err != nil {
		return err
	}
	if err := a.refreshAgentInstructions(); err != nil {
		return err
	}
	if checked {
		a.cleanupAfterUpdate()
	}
	if err := a.refreshTools(); err != nil {
		fmt.Fprintln(a.errOut, "tools need attention · retry with commander update")
	}
	return a.menu()
}

func (a *app) cleanupAfterUpdate() {
	// The update lock covers staging in earlier Commander versions as well.
	if unlock, err := a.lock("update"); err == nil {
		a.reportCleanup(a.pruneUpdateDirs(".", "", ".update-"))
		unlock()
	}
	// Subprocesses can outlive Commander. A boot ID on the directory proves
	// it is abandoned without relying on clocks, PIDs or extra files.
	if boot := runtimeBoot(); boot != "" {
		a.reportCleanup(a.pruneUpdateDirs(".secrets", boot, ".run-", ".docker-"))
		a.reportCleanup(a.pruneUpdateDirs("tools", boot, ".install-", ".packages-"))
	}
}

const runtimeBootAttr = "user.commander.boot"

var runtimeBoot = sync.OnceValue(func() string {
	readID := func(path string) string {
		data, err := os.ReadFile(path)
		id := strings.ReplaceAll(strings.TrimSpace(string(data)), "-", "")
		if err != nil || !validRuntimeID(id) {
			return ""
		}
		return id
	}
	host := readID("/etc/machine-id")
	boot := readID("/proc/sys/kernel/random/boot_id")
	if host == "" || boot == "" {
		return ""
	}
	return host + boot
})

func validRuntimeID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil && strings.ToLower(id) == id
}

func runtimeDir(parent, prefix string) (string, error) {
	dir, err := os.MkdirTemp(parent, prefix)
	if err == nil {
		if boot := runtimeBoot(); boot != "" {
			// Some filesystems lack extended attributes. The normal deferred
			// cleanup still works and the updater leaves unmarked folders alone.
			_ = unix.Lsetxattr(dir, runtimeBootAttr, []byte(boot), 0)
		}
	}
	return dir, err
}

func olderRuntimeDir(path, boot string) bool {
	var owner [64]byte
	n, err := unix.Lgetxattr(path, runtimeBootAttr, owner[:])
	if err != nil || n != len(owner) {
		return false
	}
	host, oldBoot := string(owner[:32]), string(owner[32:])
	return host == boot[:32] && validRuntimeID(oldBoot) && oldBoot != boot[32:]
}

func (a *app) reportCleanup(err error) {
	if err != nil {
		fmt.Fprintln(a.errOut, "cleanup needs attention:", err)
	}
}

func (a *app) pruneUpdateDirs(dir, boot string, prefixes ...string) error {
	parent := filepath.Join(a.root, dir)
	if err := noSymlinks(a.root, parent); err != nil {
		return err
	}
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, prefix := range prefixes {
			suffix, ok := strings.CutPrefix(entry.Name(), prefix)
			if !ok {
				continue
			}
			number, err := strconv.ParseUint(suffix, 10, 32)
			if err != nil || strconv.FormatUint(number, 10) != suffix {
				break
			}
			stat, err := entry.Info()
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				failures = append(failures, err)
				break
			}
			path := filepath.Join(parent, entry.Name())
			if stat.IsDir() && stat.Mode().Perm() == 0700 && (boot == "" || olderRuntimeDir(path, boot)) {
				if err := os.RemoveAll(path); err != nil {
					failures = append(failures, err)
				}
			}
			break
		}
	}
	return errors.Join(failures...)
}

func (a *app) restartCommander(args []string) error {
	path := filepath.Join(a.root, "commander")
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "COMMANDER_RESTARTED=") && !strings.HasPrefix(entry, "COMMANDER_HOME=") {
			env = append(env, entry)
		}
	}
	env = append(env, "COMMANDER_HOME="+a.root)
	if len(args) == 0 {
		env = append(env, "COMMANDER_RESTARTED="+a.root)
	}
	// Replace this process, preserving cwd, terminal, streams and exit status.
	if err := syscall.Exec(path, append([]string{path}, args...), env); err != nil {
		return fmt.Errorf("Commander was updated but could not restart; start ./commander again: %w", err)
	}
	return nil
}

func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Private releases may use only a GitHub identity selected in the session.
// No credentials are read or accounts switched.
func (a *app) verifyReleaseAccount(ctx context.Context) error {
	if a.session == nil || !a.session.allows("plaxtoris/commander") {
		return errors.New("private updates require a session selecting plaxtoris/commander")
	}
	selected := map[string]bool{}
	for _, m := range a.session.Accounts {
		if host, user := m.github(); m.Service == "github" && host == "github.com" {
			selected[strings.ToLower(user)] = true
		}
	}
	if len(selected) == 0 {
		return errors.New("private updates need a GitHub account in the session")
	}
	data, err := a.releaseRun(ctx, []string{"api", "--hostname", "github.com", "user", "--jq", ".login"})
	if err != nil || !selected[strings.ToLower(strings.TrimSpace(string(data)))] {
		return errors.New("could not verify a selected GitHub account for private updates")
	}
	return nil
}

func (a *app) update() (bool, error) {
	if a.localBuild {
		// The next make build supplies Commander itself. Installed tools still update.
		return false, nil
	}
	if machine == "" {
		return false, errUnsupported
	}
	if err := a.ensure(); err != nil {
		return false, err
	}
	unlock, err := a.lock("update")
	if err != nil {
		return false, err
	}
	defer unlock()
	stage, err := os.MkdirTemp(a.root, ".update-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	ctx, cancel := context.WithTimeout(context.Background(), releaseCheckTimeout)
	defer cancel()
	sums := filepath.Join(stage, "SHA256SUMS")
	private := false
	downloadRelease := func(ctx context.Context, asset string) error {
		_, err := a.releaseRun(ctx, []string{"release", "download", "latest", "--repo", "plaxtoris/commander", "--pattern", asset, "--dir", stage})
		if err != nil {
			return errors.New("could not download latest; check repository access and network")
		}
		return nil
	}
	if err = fetchContext(ctx, releaseURL+"SHA256SUMS", sums, ""); err != nil {
		if ctx.Err() != nil {
			return false, errors.New("update check timed out")
		}
		// Shared access prevents account removal during an authenticated download
		// while other sessions continue with their own selected credentials.
		release, err := fileLock(filepath.Join(a.root, ".secrets", ".native.lock"), true, true)
		if err != nil {
			return false, err
		}
		defer release()
		if err = a.verifyReleaseAccount(ctx); err != nil {
			return false, err
		}
		os.Remove(sums)
		if err = downloadRelease(ctx, "SHA256SUMS"); err != nil {
			return false, err
		}
		private = true
	}
	data, err := os.ReadFile(sums)
	if err != nil {
		return false, err
	}
	expected := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == releaseAsset {
			if expected != "" {
				return false, errors.New("release checksum is ambiguous")
			}
			expected = strings.ToLower(fields[0])
		}
	}
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return false, errors.New("release checksum is missing or invalid")
	}
	destination := filepath.Join(a.root, "commander")
	if err = noSymlinks(a.root, destination); err != nil {
		return false, err
	}
	current, err := fileChecksum(destination)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if current == expected {
		return false, nil
	}

	path := filepath.Join(stage, releaseAsset)
	if private {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		err = downloadRelease(ctx, releaseAsset)
	} else {
		err = fetch(releaseURL+releaseAsset, path, expected)
	}
	if err != nil {
		return false, err
	}
	sum, err := fileChecksum(path)
	if err != nil {
		return false, err
	}
	if sum != expected {
		return false, errors.New("release checksum did not match")
	}
	if err = os.Chmod(path, 0755); err != nil {
		return false, err
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), releaseCheckTimeout)
	defer checkCancel()
	check := exec.CommandContext(checkCtx, path, "version")
	check.Env = a.environment("setup", nil)
	if err = check.Run(); err != nil {
		return false, errors.New("downloaded Commander could not run on this system")
	}
	if err = os.Rename(path, destination); err != nil {
		return false, err
	}

	return true, nil
}
