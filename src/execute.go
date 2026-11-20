package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func serviceFor(tool string) string {
	for service, command := range serviceTools {
		if tool == command {
			return service
		}
	}
	if tokenService(tool) {
		return tool
	}
	return ""
}

func argument(args []string, flags ...string) string {
	for i, v := range args {
		for _, flag := range flags {
			if v == flag && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(v, flag+"=") {
				return strings.TrimPrefix(v, flag+"=")
			}
		}
	}
	return ""
}

func (a *app) selectedMember(service string, args []string) (member, error) {
	selector := a.account
	if service == "github" && selector == "" && a.session != nil {
		repo := argument(args, "--repo", "-R")
		if len(args) > 0 && args[0] == "api" {
			repo = ""
			for _, arg := range args[1:] {
				parts := strings.Split(strings.TrimPrefix(strings.SplitN(arg, "?", 2)[0], "/"), "/")
				if len(parts) >= 3 && parts[0] == "repos" {
					repo = parts[1] + "/" + parts[2]
					break
				}
				if len(parts) >= 2 && parts[0] == "orgs" {
					repo = parts[1] + "/*"
					break
				}
			}
			return a.repoMember(repo)
		}
		if len(args) >= 3 && args[0] == "repo" {
			switch args[1] {
			case "list":
				repo = args[2] + "/*"
			case "view", "clone", "edit", "delete", "create":
				if !strings.HasPrefix(args[2], "-") {
					repo = args[2]
				}
			}
		}
		if owner := argument(args, "--org", "--owner"); owner != "" {
			repo = owner + "/*"
		}
		if repo == "" && len(args) > 0 && args[0] != "auth" {
			repo = a.workingRepository()
		}
		return a.repoMember(repo)
	}
	if selector == "" {
		switch service {
		case "azure":
			selector = argument(args, "--subscription")
		case "hetzner":
			selector = argument(args, "--context")
		case "gcloud":
			selector = argument(args, "--account")
		}
	}
	return a.sessionAccount(service, selector)
}

// An explicit login wins, followed by an exact repository and then its owner.
func (a *app) repoMember(repo string) (member, error) {
	if a.account != "" || a.session == nil {
		return a.sessionAccount("github", a.account)
	}
	repo = strings.ToLower(strings.TrimSuffix(repo, ".git"))
	if !strings.HasSuffix(repo, "/*") {
		repo = repositoryName(repo)
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, scope := range []string{repo, owner + "/*"} {
		if !validRepositoryScope(scope) {
			continue
		}
		var matches []member
		for _, m := range a.session.Accounts {
			if m.Service != "github" {
				continue
			}
			for _, r := range m.Repos {
				if strings.EqualFold(scope, r) {
					matches = append(matches, m)
					break
				}
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return member{}, errors.New("choose one github login for " + scope + " in the session")
		}
	}
	return a.sessionAccount("github", "")
}

func (a *app) sessionAccount(service, selector string) (member, error) {
	if a.session == nil {
		return member{}, errNoSession
	}
	selected := []member{}
	for _, m := range a.session.Accounts {
		if m.Service != service {
			continue
		}
		if selector == "" || m.selector() == selector || m.Account == selector || m.ref() == selector {
			selected = append(selected, m)
		}
	}
	switch {
	case len(selected) == 1:
		return selected[0], nil
	case len(selected) > 1:
		return member{}, fmt.Errorf("session %s has several %s accounts; choose one with --account SELECTOR from commander context", a.session.Name, serviceName(service))
	case selector != "":
		return member{}, fmt.Errorf("%s account %s is not in session %s; see commander context", serviceName(service), quote(selector), a.session.Name)
	}
	return member{}, fmt.Errorf("session %s has no %s account; add one with commander session edit %s", a.session.Name, serviceName(service), quote(a.session.Name))
}

func repositoryName(value string) string {
	value = strings.TrimSuffix(value, ".git")
	if strings.HasPrefix(value, "git@github.com:") {
		value = strings.TrimPrefix(value, "git@github.com:")
	}
	if u, err := url.Parse(value); err == nil && u.Host != "" {
		if u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		value = strings.TrimPrefix(u.Path, "/")
	}
	if validRepositoryScope(value) && !strings.HasSuffix(value, "/*") {
		return value
	}
	return ""
}

func (a *app) workingRepository() string {
	git, err := a.lookupTool("git")
	if err != nil {
		return ""
	}
	cmd := exec.Command(git, "remote", "get-url", "origin")
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return repositoryName(strings.TrimSpace(string(b)))
}

func (a *app) checkGitHub(args []string, m member) error {
	if len(args) == 0 {
		return errors.New("supply a GitHub command")
	}
	host, _ := m.github()
	if h := argument(args, "--hostname"); h != "" && h != host {
		return errors.New("GitHub host is outside the selected account")
	}
	if args[0] == "auth" {
		if len(args) > 1 && args[1] == "status" {
			return nil
		}
		return errors.New("manage logins in the commander menu, outside a session")
	}
	repo := argument(args, "--repo", "-R")
	if owner := argument(args, "--org", "--owner"); owner != "" {
		allowed := false
		for _, scope := range a.session.Repositories {
			allowed = allowed || strings.EqualFold(scope, owner+"/*")
		}
		if !allowed {
			return errors.New("owner is outside the session selection")
		}
	}
	if !contains([]string{"api", "repo", "pr", "issue", "run", "workflow", "release", "secret", "variable", "label", "cache", "ruleset"}, args[0]) {
		return errors.New("use a repository command or explicit repository REST endpoint within this session")
	}
	if args[0] == "api" {
		endpoint := ""
		for _, v := range args[1:] {
			v = strings.TrimPrefix(v, "/")
			if strings.HasPrefix(v, "repos/") || strings.HasPrefix(v, "orgs/") || v == "user" || v == "/user" {
				endpoint = strings.TrimPrefix(v, "/")
				break
			}
		}
		if endpoint == "user" && contains([]string{"", "GET"}, argument(args, "--method", "-X")) && argument(args, "--field", "-F", "--raw-field", "-f", "--input") == "" {
			return nil
		}
		path := strings.Split(strings.SplitN(endpoint, "?", 2)[0], "/")
		for _, part := range path {
			if part == "." || part == ".." || strings.Contains(part, "%") {
				return errors.New("invalid GitHub API path")
			}
		}
		if len(path) >= 3 && path[0] == "repos" {
			repo = path[1] + "/" + path[2]
		} else if len(path) >= 2 && path[0] == "orgs" {
			for _, allowed := range a.session.Repositories {
				if strings.EqualFold(allowed, path[1]+"/*") {
					return nil
				}
			}
			return errors.New("organization is outside the session selection")
		} else {
			return errors.New("use an explicit repository REST endpoint within the session selection")
		}
	}
	if len(args) >= 2 && args[0] == "repo" && args[1] == "list" {
		if len(args) < 3 {
			return errors.New("name the repository owner")
		}
		for _, r := range a.session.Repositories {
			if strings.EqualFold(r, args[2]+"/*") {
				return nil
			}
		}
		return errors.New("owner is outside the session selection")
	}
	if len(args) >= 3 && args[0] == "repo" && !strings.HasPrefix(args[2], "-") && (args[1] == "clone" || args[1] == "view" || args[1] == "edit" || args[1] == "delete") {
		repo = args[2]
	}
	if len(args) >= 3 && args[0] == "repo" && args[1] == "create" {
		repo = args[2]
		if !strings.Contains(repo, "/") {
			_, user := m.github()
			repo = user + "/" + repo
		}
	}
	if repo == "" {
		repo = a.workingRepository()
	}
	if repo == "" {
		return errors.New("name the repository with --repo OWNER/NAME")
	}
	if !a.session.allows(repositoryName(repo)) {
		return fmt.Errorf("repository %s is outside session %s", quote(repo), a.session.Name)
	}
	return nil
}

func (a *app) execute(tool string, args []string) error {
	agent := agentService(tool)
	scopedAgent := agent != "" && !(len(args) == 1 && contains([]string{"--version", "-v", "--help", "-h"}, args[0]))
	if scopedAgent && a.session == nil {
		return errNoSession
	}
	if tool == "git" {
		if err := a.checkGit(args); err != nil {
			return err
		}
	}
	service := serviceFor(tool)
	if a.session == nil {
		if service != "" && !(tool == "gh" && len(args) > 0 && args[0] == "auth") {
			return errNoSession
		}
		if service != "" {
			unlock, err := a.lock("native")
			if err != nil {
				return err
			}
			defer unlock()
		}
		_, err := a.run(tool, args, false, nil)
		return err
	}
	unlock, err := a.sessionLock(a.session.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err = a.sessionWrappers(); err != nil {
		return err
	}
	if scopedAgent {
		m, err := a.sessionAccount(agent, a.account)
		if err != nil {
			return err
		}
		return a.runAgent(m, args)
	}
	nativeUnlock, err := fileLock(filepath.Join(a.root, ".secrets", ".native.lock"), true, true)
	if err != nil {
		return err
	}
	defer nativeUnlock()
	if service == "" || tokenService(service) {
		_, err = a.run(tool, args, false, nil)
		return err
	}
	m, err := a.selectedMember(service, args)
	if err != nil {
		return err
	}
	if tool == "gh" {
		if err = a.checkGitHub(args, m); err != nil {
			return err
		}
	}
	if err = a.prepareTool(tool); err != nil {
		return err
	}
	if service == "github" {
		token, err := a.githubToken(m)
		if err != nil {
			return err
		}
		host, _ := m.github()
		_, err = a.nativeRunContext(context.Background(), tool, args, false, map[string]string{"GH_TOKEN": token, "GH_HOST": host})
		return err
	}
	root, extra, finish, err := a.accountRuntime(m)
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	_, runErr := a.nativeRunContext(context.Background(), tool, args, false, extra)
	if err = finish(); err != nil {
		return err
	}
	return runErr
}

func (a *app) credentialLock(key string) (func(), error) {
	sum := sha256.Sum256([]byte(key))
	return fileLock(filepath.Join(a.root, ".secrets", fmt.Sprintf(".account-%x.lock", sum[:12])), false, true)
}

// One account owns its mutable refresh state. GitHub callers receive an access
// token after this short operation, so long workflow watches do not serialize.
func (a *app) accountRuntime(m member) (string, map[string]string, func() error, error) {
	key := m.Service + ":" + m.Account
	if m.Service == "github" {
		key = "github"
	}
	unlock, err := a.credentialLock(key)
	if err != nil {
		return "", nil, nil, err
	}
	fail := func(err error) (string, map[string]string, func() error, error) { unlock(); return "", nil, nil, err }
	readUnlock, err := a.credentialLock("write")
	if err != nil {
		return fail(err)
	}
	store := newStorage(a.root)
	state, err := store.state(m.Service)
	readUnlock()
	if err != nil {
		return fail(err)
	}
	selected, err := selectLogins(m.Service, state, []member{m})
	if err != nil {
		return fail(err)
	}
	root, err := os.MkdirTemp(filepath.Join(a.root, ".secrets"), ".run-")
	if err != nil {
		return fail(err)
	}
	local := newStorage(root)
	files, err := encodeLogins(m.Service, selected)
	if err == nil {
		for name, data := range files {
			path := filepath.Join(root, ".secrets", name)
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				break
			}
			if err = atomicWrite(path, data, 0600); err != nil {
				break
			}
		}
	}
	if err != nil {
		os.RemoveAll(root)
		return fail(err)
	}
	p := filepath.Join(root, ".secrets")
	extra := map[string]string{"GH_CONFIG_DIR": filepath.Join(p, ".config", "gh"), "AZURE_CONFIG_DIR": filepath.Join(p, ".azure"), "HCLOUD_CONFIG": filepath.Join(p, ".config", "hcloud", "cli.toml"), "CLOUDSDK_CONFIG": filepath.Join(p, ".config", "gcloud")}
	if m.Service == "office" {
		extra["HOME"] = p
		extra["XDG_CONFIG_HOME"] = filepath.Join(p, ".config")
	}
	if m.Service == "gcloud" {
		extra["CLOUDSDK_CORE_ACCOUNT"] = m.Account
	}
	finish := func() error {
		defer unlock()
		updated, err := local.state(m.Service)
		if err != nil {
			return err
		}
		updated, err = selectLogins(m.Service, updated, []member{m})
		if err != nil {
			return err
		}
		writeUnlock, err := a.credentialLock("write")
		if err != nil {
			return err
		}
		defer writeUnlock()
		currentStore := newStorage(a.root)
		current, err := currentStore.state(m.Service)
		if err != nil {
			return err
		}
		merged := mergeCredentialState(m.Service, current, updated)
		encoded, err := encodeLogins(m.Service, merged)
		if err != nil {
			return err
		}
		publish := map[string][]byte{}
		for name, data := range encoded {
			if _, err = currentStore.read(name); err != nil {
				return err
			}
			publish[filepath.Join(".secrets", name)] = data
		}
		return currentStore.publish(publish)
	}
	return root, extra, finish, nil
}

func mergeCredentialState(service string, current, updated object) object {
	merge := func(dst, src object) object {
		for k, v := range src {
			dst[k] = v
		}
		return dst
	}
	rows := func(old, next []any, key string) []any {
		for _, v := range next {
			found := false
			for i, row := range old {
				if obj(row)[key] == obj(v)[key] {
					v = clone(v)
					for _, flag := range []string{"isDefault", "active"} {
						if value, ok := obj(row)[flag]; ok {
							obj(v)[flag] = value
						}
					}
					old[i] = v
					found = true
					break
				}
			}
			if !found {
				if len(old) > 0 {
					v = clone(v)
					for _, flag := range []string{"isDefault", "active"} {
						if _, ok := obj(v)[flag]; ok {
							obj(v)[flag] = false
						}
					}
				}
				old = append(old, v)
			}
		}
		return old
	}
	switch service {
	case "github":
		for host, v := range updated {
			next := obj(v)
			saved := obj(current[host])
			if len(saved) == 0 {
				current[host] = next
				continue
			}
			saved["users"] = merge(obj(saved["users"]), obj(next["users"]))
			if saved["user"] == next["user"] {
				delete(saved, "oauth_token")
				if token, ok := next["oauth_token"]; ok {
					saved["oauth_token"] = token
				}
			}
		}
	case "azure", "office":
		cache := obj(current["cache"])
		for kind, v := range obj(updated["cache"]) {
			cache[kind] = merge(obj(cache[kind]), obj(v))
		}
		current["cache"] = cache
		if service == "azure" {
			p := obj(current["profile"])
			p["subscriptions"] = rows(list(p["subscriptions"]), list(obj(updated["profile"])["subscriptions"]), "id")
			current["profile"] = p
		} else {
			current["connections"] = rows(list(current["connections"]), list(updated["connections"]), "name")
			name := textValue(obj(current["current"])["name"])
			if name == "" {
				current["current"] = updated["current"]
			} else {
				for _, row := range list(current["connections"]) {
					if obj(row)["name"] == name {
						current["current"] = row
						break
					}
				}
			}
		}
	case "hetzner":
		current["contexts"] = rows(list(current["contexts"]), list(updated["contexts"]), "name")
		if textValue(current["active_context"]) == "" {
			current["active_context"] = updated["active_context"]
		}
	default:
		merge(current, updated)
	}
	return current
}

func (a *app) githubToken(m member) (string, error) {
	if err := a.prepareTool("gh"); err != nil {
		return "", err
	}
	root, extra, finish, err := a.accountRuntime(m)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	host, user := m.github()
	data, runErr := a.run("gh", []string{"auth", "token", "--hostname", host, "--user", user}, true, extra)
	if err = finish(); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", runErr
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errFormat
	}
	return token, nil
}

func (a *app) gitCredential(args []string) error {
	if len(args) != 1 {
		return errors.New("Git credential helper requires an operation")
	}
	if args[0] != "get" {
		return nil
	}
	return a.writeGitCredential(a.in, a.out)
}

func (a *app) writeGitCredential(input io.Reader, output io.Writer) error {
	if a.session == nil {
		return errNoSession
	}
	unlock, err := a.sessionLock(a.session.ID, true, true)
	if err != nil {
		return err
	}
	defer unlock()
	nativeUnlock, err := fileLock(filepath.Join(a.root, ".secrets", ".native.lock"), true, true)
	if err != nil {
		return err
	}
	defer nativeUnlock()
	fields := map[string]string{}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	if scanner.Err() != nil {
		return scanner.Err()
	}
	if fields["protocol"] != "https" || !a.session.allows(fields["path"]) {
		return errors.New("Git repository is outside the session selection")
	}
	m, err := a.repoMember(fields["path"])
	if err != nil {
		return err
	}
	host, user := m.github()
	if fields["host"] != host {
		return errors.New("Git host is outside the selected account")
	}
	token, err := a.githubToken(m)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "username=%s\npassword=%s\n\n", user, token)
	return nil
}
