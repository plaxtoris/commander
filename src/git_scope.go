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
	if !contains([]string{"clone", "fetch", "pull", "push", "ls-remote"}, command) {
		return nil
	}
	remote := ""
	all := false
	for len(args) > 0 {
		value := args[0]
		args = args[1:]
		if value == "--all" {
			all = true
		}
		if contains([]string{"--depth", "--filter", "--branch", "-b", "--reference", "--reference-if-able", "--shallow-since", "--shallow-exclude", "--upload-pack", "--receive-pack", "--repo"}, value) {
			if len(args) == 0 {
				return errors.New("Git option needs a value")
			}
			if value == "--repo" {
				remote = args[0]
			}
			args = args[1:]
			continue
		}
		if !strings.HasPrefix(value, "-") {
			remote = value
			break
		}
	}
	remotes := []string{}
	if command == "clone" || command == "ls-remote" && remote != "" {
		remotes = append(remotes, remote)
	} else if all && command == "fetch" {
		list, err := a.gitAt(dir, "remote")
		if err != nil {
			return err
		}
		remotes = strings.Fields(list)
	} else {
		if remote == "" {
			remote = "origin"
		}
		remotes = append(remotes, remote)
	}
	for _, remote := range remotes {
		address := remote
		if value, e := a.gitAt(dir, "config", "remote."+remote+".url"); e == nil {
			address = value
		}
		repository := repositoryName(address)
		if repository == "" {
			if strings.HasPrefix(address, "/") || strings.HasPrefix(address, "./") || strings.HasPrefix(address, "../") || strings.HasPrefix(address, "file://") {
				continue
			}
			return errors.New("Git remote is outside the session repository selection")
		}
		if a.session == nil || !a.session.allows(repository) {
			return errors.New("Git repository is outside the session selection; start or edit a Commander session")
		}
	}
	return nil
}
