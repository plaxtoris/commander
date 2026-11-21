package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (a *app) checkGit(args []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	command := ""
	for len(args) > 0 {
		value := args[0]
		args = args[1:]
		if value == "-C" {
			if len(args) == 0 {
				return errors.New("git -C needs a directory")
			}
			if filepath.IsAbs(args[0]) {
				dir = args[0]
			} else {
				dir = filepath.Join(dir, args[0])
			}
			args = args[1:]
			continue
		}
		if value == "-c" || value == "--git-dir" || value == "--work-tree" {
			if len(args) == 0 {
				return errors.New("Git option needs a value")
			}
			args = args[1:]
			continue
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		command = value
		break
	}
	// Reading public repositories is always allowed and runs without a login,
	// because the credential helper only answers for selected repositories.
	if command != "push" {
		return nil
	}
	// Git prefers the positional repository over --repo, so check both.
	remotes := []string{}
	positional := false
	for len(args) > 0 {
		value := args[0]
		args = args[1:]
		if repo, ok := strings.CutPrefix(value, "--repo="); ok {
			remotes = append(remotes, repo)
			continue
		}
		if contains([]string{"--repo", "--receive-pack", "--exec", "-o", "--push-option"}, value) {
			if len(args) == 0 {
				return errors.New("Git option needs a value")
			}
			if value == "--repo" {
				remotes = append(remotes, args[0])
			}
			args = args[1:]
			continue
		}
		if !positional && !strings.HasPrefix(value, "-") {
			positional = true
			remotes = append(remotes, value)
		}
	}
	if len(remotes) == 0 {
		remote := "origin"
		branch, _ := a.gitAt(dir, "branch", "--show-current")
		for _, key := range []string{"branch." + branch + ".pushRemote", "remote.pushDefault", "branch." + branch + ".remote"} {
			if value, e := a.gitAt(dir, "config", key); e == nil && value != "" {
				remote = value
				break
			}
		}
		remotes = append(remotes, remote)
	}
	addresses := []string{}
	for _, remote := range remotes {
		if value, e := a.gitAt(dir, "remote", "get-url", "--all", "--push", remote); e == nil {
			addresses = append(addresses, strings.Split(value, "\n")...)
		} else {
			addresses = append(addresses, remote)
		}
	}
	for _, address := range addresses {
		repository := repositoryName(address)
		if repository == "" {
			if strings.HasPrefix(address, "/") || strings.HasPrefix(address, "./") || strings.HasPrefix(address, "../") || strings.HasPrefix(address, "file://") {
				continue
			}
			return errors.New("Git remote is outside the session repository selection")
		}
		if a.session == nil || !a.session.allows(repository) {
			return errors.New("Git push target is outside the session selection; start or edit a Commander session")
		}
	}
	return nil
}
