package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func usePresetSession(t *testing.T, a *app, name string) {
	t.Helper()
	_, members, err := a.profile(name)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.newSession(name, members, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
}

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
	if a.sessionCommand([]string{"close", s.ID}) == nil {
		t.Fatal("closed active session")
	}
	first()
	second()
	if err = a.sessionCommand([]string{"close", s.ID}); err != nil {
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
