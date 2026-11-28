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

const pendingRelease = ".update-ready"
const updateInterval = time.Hour

func (a *app) start() error {
	os.Unsetenv("COMMANDER_RESTARTED")
	if err := a.ensure(); err != nil {
		return err
	}
	if !a.localBuild {
		applied, _ := a.applyPendingRelease()
		// A second window may have launched the old inode just before the swap.
		if applied || a.runningReplaced() {
			return a.restartCommander(nil)
		}
	}
	if err := a.refreshAgentInstructions(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	delay := a.scheduleUpdate(!a.localBuild)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				timer.Reset(a.scheduleUpdate(false))
			}
		}
	}()
	return a.menu(!a.localBuild)
}

func (a *app) updateDelay() time.Duration {
	info, err := os.Stat(filepath.Join(a.root, "tools", ".update-checked"))
	if err == nil {
		if age := time.Since(info.ModTime()); age >= 0 && age < updateInterval {
			return updateInterval - age
		}
	}
	return 0
}

// The worker can finish after the menu closes. It has no terminal or input.
func (a *app) scheduleUpdate(startup bool) time.Duration {
	delay := a.updateDelay()
	if !startup && delay > 0 {
		return delay
	}
	args := []string{"update-check"}
	if startup {
		args = append(args, "startup")
	}
	cmd := exec.Command(filepath.Join(a.root, "commander"), a.scopedUpdateArgs(args)...)
	cmd.Env = append(os.Environ(), "COMMANDER_HOME="+a.root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
	if delay > 0 {
		return delay
	}
	return updateInterval
}

func (a *app) backgroundUpdate(startup bool) error {
	unlock, err := a.installationLock("update", false, false)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	if a.updateDelay() > 0 {
		defer unlock()
		if startup {
			_, err = a.stageRelease()
		}
		return err
	}
	err = a.markUpdateCheck()
	if err == nil {
		_, err = a.stageRelease()
	}
	unlock()
	if err == nil {
		a.cleanupAfterUpdate()
	}
	return errors.Join(err, a.refreshTools())
}

func (a *app) markUpdateCheck() error {
	return atomicWrite(filepath.Join(a.root, "tools", ".update-checked"), nil, 0600)
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
		a.reportCleanup(a.pruneUpdateDirs(".secrets/.runtime", boot, ".run-"))
		if a.runtimeMigrated() {
			a.reportCleanup(a.pruneRuntimeDirs(".secrets", boot, true, ".run-", ".docker-"))
			a.reportCleanup(a.pruneLegacyLocks())
		} else {
			a.reportCleanup(a.pruneUpdateDirs(".secrets", boot, ".run-", ".docker-"))
		}
		a.reportCleanup(a.pruneUpdateDirs("tools", boot, ".install-", ".packages-"))
	}
}

const runtimeBootAttr = "user.commander.boot"
const runtimeMigrationAttr = "user.commander.runtime-migration"

func (a *app) privateRuntimeDir() (string, error) {
	parent := filepath.Join(a.root, ".secrets", ".runtime")
	if err := noSymlinks(a.root, parent); err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	return runtimeDir(parent, ".run-")
}

func (a *app) runtimeMigrated() bool {
	boot := runtimeBoot()
	path := filepath.Join(a.root, ".secrets")
	if boot == "" || noSymlinks(a.root, path) != nil {
		return false
	}
	var marker [64]byte
	n, err := unix.Lgetxattr(path, runtimeMigrationAttr, marker[:])
	if err == unix.ENODATA {
		// Older processes may still create or use the legacy files until
		// this boot ends. Never unlink a lock they could still be using.
		_ = unix.Lsetxattr(path, runtimeMigrationAttr, []byte(boot), unix.XATTR_CREATE)
		return false
	}
	return err == nil && n == len(marker) && string(marker[:32]) == boot[:32] &&
		validRuntimeID(string(marker[32:])) && string(marker[32:]) != boot[32:]
}

func (a *app) pruneLegacyLocks() error {
	parent := filepath.Join(a.root, ".secrets")
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !legacyLockName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && info.Size() == 0 {
			err = os.Remove(filepath.Join(parent, entry.Name()))
		}
		if err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func legacyLockName(name string) bool {
	switch name {
	case ".coding-config.lock", ".health.lock", ".memory.lock", ".native.lock",
		".sessions.lock", ".setup.lock", ".tool-refresh.lock", ".update.lock", ".update-ready.lock",
		".claude-tool.lock", ".codex-tool.lock", ".pi-tool.lock":
		return true
	}
	if label, ok := strings.CutSuffix(name, ".login.lock"); ok {
		for _, prefix := range []string{".claude-", ".codex-"} {
			if label, ok := strings.CutPrefix(label, prefix); ok && codingLabel.MatchString(label) {
				return true
			}
		}
	}
	hash, ok := strings.CutPrefix(name, ".account-")
	hash, suffix := strings.CutSuffix(hash, ".lock")
	_, err := hex.DecodeString(hash)
	return ok && suffix && len(hash) == 24 && strings.ToLower(hash) == hash && err == nil
}

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

func olderRuntimeDir(path, boot string, unmarked bool) bool {
	var owner [64]byte
	n, err := unix.Lgetxattr(path, runtimeBootAttr, owner[:])
	if unmarked && err == unix.ENODATA {
		return true
	}
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
	return a.pruneRuntimeDirs(dir, boot, false, prefixes...)
}

func (a *app) pruneRuntimeDirs(dir, boot string, unmarked bool, prefixes ...string) error {
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
			if stat.IsDir() && stat.Mode().Perm() == 0700 && (boot == "" || olderRuntimeDir(path, boot, unmarked)) {
				if err := os.RemoveAll(path); err != nil {
					failures = append(failures, err)
				}
			}
			break
		}
	}
	return errors.Join(failures...)
}

func (a *app) scopedUpdateArgs(args []string) []string {
	if a.account != "" {
		args = append([]string{"--account", a.account}, args...)
	}
	if a.session != nil {
		args = append([]string{"--session", a.session.ID}, args...)
	}
	return args
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
	if err := syscall.Exec(path, append([]string{path}, a.scopedUpdateArgs(args)...), env); err != nil {
		return fmt.Errorf("Commander was updated but could not restart; start ./commander again: %w", err)
	}
	return nil
}

func (a *app) runningReplaced() bool {
	running, _ := os.Readlink("/proc/self/exe")
	return running == filepath.Join(a.root, "commander")+" (deleted)"
}

func (a *app) restartReady() bool {
	_, err := os.Stat(filepath.Join(a.root, pendingRelease, "checksums"))
	return err == nil || a.runningReplaced()
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
	if err := a.ensure(); err != nil {
		return false, err
	}
	unlock, err := a.waitLock("update")
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := a.markUpdateCheck(); err != nil {
		return false, err
	}
	// A window can activate the previous pending release while we fetch its
	// successor. Recheck the latest release against the new installation.
	for range 2 {
		ready, err := a.stageRelease()
		if err != nil || !ready {
			return false, err
		}
		if applied, err := a.applyPendingRelease(); applied || err != nil {
			return applied, err
		}
	}
	return false, errors.New("installation changed during update; retry commander update")
}

// Download under the update lock. Publication and activation share only a
// short local lock, so opening the menu never waits for the network.
func (a *app) stageRelease() (bool, error) {
	if a.localBuild {
		return false, nil
	}
	if machine == "" {
		return false, errUnsupported
	}
	var stage string
	defer func() {
		if stage != "" {
			os.RemoveAll(stage)
		}
	}()
	createStage := func() error {
		var err error
		stage, err = os.MkdirTemp(a.root, ".update-")
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseCheckTimeout)
	defer cancel()
	private := false
	downloadRelease := func(ctx context.Context, asset string) error {
		_, err := a.releaseRun(ctx, []string{"release", "download", "latest", "--repo", "plaxtoris/commander", "--pattern", asset, "--dir", stage})
		if err != nil {
			return errors.New("could not download latest; check repository access and network")
		}
		return nil
	}
	body, err := downloadBody(ctx, releaseURL+"SHA256SUMS")
	var data []byte
	if err == nil {
		const limit = 64 << 10
		data, err = io.ReadAll(io.LimitReader(body, limit+1))
		body.Close()
		if len(data) > limit {
			return false, errors.New("release checksum file is too large")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, errors.New("update check timed out")
		}
		// Shared access prevents account removal during an authenticated download
		// while other sessions continue with their own selected credentials.
		release, err := a.installationLock("native", true, true)
		if err != nil {
			return false, err
		}
		defer release()
		if err = a.verifyReleaseAccount(ctx); err != nil {
			return false, err
		}
		if err = createStage(); err != nil {
			return false, err
		}
		if err = downloadRelease(ctx, "SHA256SUMS"); err != nil {
			return false, err
		}
		data, err = os.ReadFile(filepath.Join(stage, "SHA256SUMS"))
		if err != nil {
			return false, err
		}
		private = true
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
	current, ready, err := a.releaseStatus(expected)
	if err != nil || ready || current == expected {
		return ready, err
	}
	if stage == "" {
		if err = createStage(); err != nil {
			return false, err
		}
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
	if err = os.WriteFile(filepath.Join(stage, "checksums"), []byte(current+"\n"+expected), 0600); err != nil {
		return false, err
	}
	publish, err := a.waitLock("update-ready")
	if err != nil {
		return false, err
	}
	defer publish()
	if err = a.clearPendingRelease(); err != nil {
		return false, err
	}
	if err = os.Rename(stage, filepath.Join(a.root, pendingRelease)); err != nil {
		return false, err
	}
	return true, nil
}

// Called under the publication lock. Only remove files this updater creates.
func (a *app) clearPendingRelease() error {
	dir := filepath.Join(a.root, pendingRelease)
	if err := noSymlinks(a.root, dir); err != nil {
		return err
	}
	// A portable installation can still hold a download for its former machine.
	for _, name := range []string{"commander-x86_64", "commander-aarch64", "checksums", "SHA256SUMS", ""} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (a *app) releaseStatus(expected string) (current string, ready bool, err error) {
	unlock, err := a.waitLock("update-ready")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	destination := filepath.Join(a.root, "commander")
	if err := noSymlinks(a.root, destination); err != nil {
		return "", false, err
	}
	current, err = fileChecksum(destination)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	if current == expected {
		return current, false, a.clearPendingRelease()
	}
	dir := filepath.Join(a.root, pendingRelease)
	path := filepath.Join(dir, releaseAsset)
	metadata := filepath.Join(dir, "checksums")
	for _, file := range []string{metadata, path} {
		if err := noSymlinks(a.root, file); err != nil {
			return "", false, err
		}
	}
	if saved, _ := os.ReadFile(metadata); string(saved) == current+"\n"+expected {
		sum, err := fileChecksum(path)
		ready = err == nil && sum == expected
	}
	return current, ready, nil
}

func (a *app) applyPendingRelease() (bool, error) {
	ready := filepath.Join(a.root, pendingRelease)
	if _, err := os.Lstat(ready); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	unlock, err := a.waitLock("update-ready")
	if err != nil {
		return false, err
	}
	defer unlock()
	metadata := filepath.Join(ready, "checksums")
	candidate := filepath.Join(ready, releaseAsset)
	destination := filepath.Join(a.root, "commander")
	for _, path := range []string{metadata, candidate, destination} {
		if err := noSymlinks(a.root, path); err != nil {
			return false, err
		}
	}
	data, err := os.ReadFile(metadata)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	before, expected, ok := strings.Cut(string(data), "\n")
	current, err := fileChecksum(destination)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if before != current {
		// Another installation replaced the original binary after the download.
		return false, a.clearPendingRelease()
	}
	got, err := fileChecksum(candidate)
	if !ok || !checksum.MatchString(expected) || err != nil || got != expected {
		return false, errors.Join(errors.New("staged release checksum did not match"), a.clearPendingRelease())
	}
	if err := os.Rename(candidate, destination); err != nil {
		return false, err
	}
	return true, a.clearPendingRelease()
}
