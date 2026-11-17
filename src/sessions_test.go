package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionSelectionsAreIndependent(t *testing.T) {
	a := testApp(t)
	private, err := a.newSession("private", []member{ghMember("shared")}, []string{"private/*"})
	if err != nil {
		t.Fatal(err)
	}
	company, err := a.newSession("company", []member{ghMember("shared")}, []string{"company/app"})
	if err != nil {
		t.Fatal(err)
	}
	if !private.allows("private/example.git") || private.allows("company/app") || !company.allows("COMPANY/app") || company.allows("company/other") {
		t.Fatal("repository scope escaped")
	}
	before, err := a.readSession(private.ID)
	if err != nil {
		t.Fatal(err)
	}
	company.Accounts = nil
	if err = a.saveSession(company); err != nil {
		t.Fatal(err)
	}
	after, err := a.readSession(private.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("one session changed another", err)
	}
	for _, invalid := range []string{"../escape", "/absolute", "owner/../repo", "*", "owner/*/nested"} {
		s := private
		s.Repositories = []string{invalid}
		if a.saveSession(s) == nil {
			t.Fatal("accepted invalid scope", invalid)
		}
	}
}

func TestSessionCommandsCanOverlapAndPreventClose(t *testing.T) {
	a := testApp(t)
	s, err := a.newSession("parallel", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.sessionLock(s.ID, true, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.sessionLock(s.ID, true, false)
	if err != nil {
		t.Fatal("parallel command blocked", err)
	}
	if a.sessionCommand([]string{"delete", s.Name}) == nil {
		t.Fatal("closed active session")
	}
	first()
	second()
	if err = a.sessionCommand([]string{"delete", s.Name}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionEnvironmentDoesNotExposeGlobalStores(t *testing.T) {
	a := testApp(t)
	s, err := a.newSession("isolated", []member{ghMember("selected")}, []string{"owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	env := map[string]string{}
	for _, v := range a.environment("git", nil) {
		for i, c := range v {
			if c == '=' {
				env[v[:i]] = v[i+1:]
				break
			}
		}
	}
	for _, key := range []string{"GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "CLOUDSDK_CONFIG", "HCLOUD_CONFIG", "DOCKER_CONFIG"} {
		if len(env[key]) == 0 || filepath.HasPrefix(env[key], filepath.Join(a.root, ".secrets")) {
			t.Fatal("global store exposed", key)
		}
	}
	if env["COMMANDER_SESSION"] != s.ID {
		t.Fatal("session identity missing")
	}
	if err = a.sessionWrappers(s); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(a.root, ".sessions", s.ID, "bin", "gh")); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubScopeRejectsOtherRepositories(t *testing.T) {
	a := testApp(t)
	s := session{Repositories: []string{"company/app"}}
	a.session = &s
	m := ghMember("shared")
	for _, args := range [][]string{{"api", "repos/company/app/actions/runs"}, {"pr", "list", "--repo", "company/app"}, {"api", "user"}} {
		if err := a.checkGitHub(args, m); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"api", "repos/company/other"}, {"api", "repos/company/app/../../other"}, {"api", "graphql"}, {"auth", "switch"}, {"repo", "list", "company"}, {"pr", "list", "-R", "private/app"}} {
		if a.checkGitHub(args, m) == nil {
			t.Fatal("accepted outside scope", args)
		}
	}
}

func TestRefreshedCredentialDoesNotChangeAnotherAccount(t *testing.T) {
	current := object{"github.com": object{"user": "other", "oauth_token": "other-token", "users": object{"other": object{"oauth_token": "other-token"}, "selected": object{"oauth_token": "old"}}}}
	updated := object{"github.com": object{"user": "selected", "oauth_token": "new", "users": object{"selected": object{"oauth_token": "new"}}}}
	result := mergeCredentialState("github", current, updated)
	host := obj(result["github.com"])
	if host["user"] != "other" || host["oauth_token"] != "other-token" || obj(obj(host["users"])["selected"])["oauth_token"] != "new" {
		t.Fatal("refresh changed account selection", result)
	}
}

func TestGitNetworkCommandsRespectRepositoryScope(t *testing.T) {
	a := testApp(t)
	s, err := a.newSession("work", nil, []string{"company/app"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	for _, args := range [][]string{{"clone", "https://github.com/company/app.git"}, {"ls-remote", "git@github.com:company/app.git"}, {"status"}, {"clone", "/tmp/local.git"}} {
		if err := a.checkGit(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"clone", "https://github.com/private/app.git"}, {"clone", "git@github.com:private/app.git"}, {"ls-remote", "https://github.com/company/other.git"}} {
		if a.checkGit(args) == nil {
			t.Fatal("unselected remote accepted", args)
		}
	}
}

func TestGitChecksPushURLsAndDefaultRemote(t *testing.T) {
	a := testApp(t)
	s, err := a.newSession("work", nil, []string{"company/app"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"remote", "add", "origin", "git@github.com:company/app.git"}, {"remote", "add", "private", "https://github.com/private/app.git"}} {
		if _, err := a.gitAt(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	url, err := a.gitAt(dir, "remote", "get-url", "origin")
	if err != nil || url != "https://github.com/company/app.git" {
		t.Fatal("SSH remote did not select scoped HTTPS", url, err)
	}
	if err := a.checkGit([]string{"-C", dir, "push", "origin", "main"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", dir, "fetch", "--multiple", "origin", "private"}, {"-C", dir, "push", "--repo=private", "main"}} {
		if a.checkGit(args) == nil {
			t.Fatal("unselected secondary remote accepted", args)
		}
	}
	for _, key := range []string{"remote.pushDefault", "branch.main.remote", "branch.main.pushRemote"} {
		if _, err := a.gitAt(dir, "config", key, "private"); err != nil {
			t.Fatal(err)
		}
		if a.checkGit([]string{"-C", dir, "push"}) == nil {
			t.Fatal("unselected default remote accepted", key)
		}
		a.gitAt(dir, "config", "--unset", key)
	}
	if _, err := a.gitAt(dir, "config", "remote.origin.pushurl", "https://github.com/private/app.git"); err != nil {
		t.Fatal(err)
	}
	if a.checkGit([]string{"-C", dir, "push", "origin", "main"}) == nil {
		t.Fatal("unselected push URL accepted")
	}
}

func TestSessionNamesRenameAndDelete(t *testing.T) {
	a := testApp(t)
	work := addSession(t, a, "Work")
	if _, err := a.newSession("work", nil, nil); err == nil {
		t.Fatal("accepted a name differing only in case")
	}
	if err := a.sessionCommand([]string{"new", "other", "--repos", "acme/*,acme/tool"}); err != nil {
		t.Fatal(err)
	}
	if err := a.sessionCommand([]string{"rename", "Work", "other"}); err == nil {
		t.Fatal("renamed onto an existing name")
	}
	if err := a.sessionCommand([]string{"rename", "Work", "client"}); err != nil {
		t.Fatal(err)
	}
	renamed, err := a.findSession("client")
	if err != nil || renamed.ID != work.ID {
		t.Fatal("rename changed the session ID", err)
	}
	if _, err = a.findSession("Work"); err == nil {
		t.Fatal("old name still resolves")
	}
	if byID, err := a.findSession(work.ID); err != nil || byID.Name != "client" {
		t.Fatal("ID no longer resolves", err)
	}
	a.out = &bytes.Buffer{}
	if err = a.sessionCommand([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	if got := a.out.(*bytes.Buffer).String(); got != "client  no accounts\nother   no accounts  acme/* acme/tool\n" {
		t.Fatalf("list output: %q", got)
	}
	if err = a.command([]string{"--session", "other", "context"}); err == nil || !strings.Contains(err.Error(), `session "other" has no accounts`) {
		t.Fatal("--session did not resolve the name", err)
	}
	if err = a.sessionCommand([]string{"delete", "client"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := a.sessions(); len(all) != 1 || all[0].Name != "other" {
		t.Fatal("delete removed the wrong session", all)
	}
}

func TestInteractiveRepositoriesFromOrganizations(t *testing.T) {
	a := testApp(t)
	mustWrite(t, filepath.Join(a.root, ".secrets/.config/gh/hosts.yml"), []byte("github.com:\n  user: alice\n  oauth_token: alice-secret\n  git_protocol: https\n  users:\n    alice:\n      oauth_token: alice-secret\n"))
	mustWrite(t, filepath.Join(a.root, "tools", ".requested", "gh"), nil)
	s, err := a.newSession("work", []member{ghMember("alice")}, []string{"alice/tool"})
	if err != nil {
		t.Fatal(err)
	}
	ui := openUI(t, a.root)
	ui.wait(t, "[q]uit")
	ui.send(t, "e")
	ui.wait(t, "+ add account")
	frame := terminalControls.ReplaceAllString(ui.output, "")
	for _, want := range []string{"[x] alice", "repositories", "[ ] alice/*", "[ ] acme/*", "[x] alice/tool"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q: %s", want, frame)
		}
	}
	if strings.Contains(frame, "bad/name") || strings.Contains(frame, "alice-secret") {
		t.Fatal("showed an invalid owner or a token")
	}
	// alice, alice/*, acme/*, alice/tool: tick acme/* and untick alice/tool.
	ui.send(t, "\x1b[B\x1b[B \x1b[B \r")
	ui.wait(t, "work saved")
	ui.send(t, "q")
	if err := ui.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if s, err = a.readSession(s.ID); err != nil || !reflect.DeepEqual(s.Repositories, []string{"acme/*"}) {
		t.Fatal("repositories not saved", s.Repositories, err)
	}
}
