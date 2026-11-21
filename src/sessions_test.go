package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentSelectionEdits(t *testing.T) {
	a := testApp(t)
	start := addSession(t, a, "work", ghMember("original"))
	newLogin := []member{ghMember("next")}
	if err := a.saveSelection(start, newLogin, start.Repositories); err != nil {
		t.Fatal(err)
	}
	if err := a.saveSelection(start, start.Accounts, []string{"org/app"}); err != nil {
		t.Fatal(err)
	}
	if err := a.saveSelection(start, []member{ghMember("stale")}, []string{"org/other"}); err == nil {
		t.Fatal("overwrote a newer selection")
	}
	got, err := a.readSession(start.ID)
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].ref() != "next" || strings.Join(got.Repositories, ",") != "org/app" {
		t.Fatal(got, err)
	}
	for _, invalid := range []string{"/", "nested/name", ".tasks"} {
		if err := a.checkName(invalid, ""); err == nil {
			t.Fatal("accepted an unusable workspace name", invalid)
		}
	}
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
	if err = a.sessionWrappers(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(a.sessionBin(), "gh")); err != nil {
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

func TestAccountSelectionExplainsTheFix(t *testing.T) {
	a := testApp(t)
	if _, err := a.sessionAccount("github", ""); err != errNoSession {
		t.Fatal("missing session not explained", err)
	}
	useSession(t, a, "work", ghMember("alice"), ghMember("bob"))
	if m, err := a.sessionAccount("github", "bob"); err != nil || m.id() != ghMember("bob").id() {
		t.Fatal("selector did not pick the account", m, err)
	}
	for selector, want := range map[string]string{"": "several github accounts", "carol": "'carol' is not in session work"} {
		if _, err := a.sessionAccount("github", selector); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(selector, err)
		}
	}
	if _, err := a.sessionAccount("hetzner", ""); err == nil || !strings.Contains(err.Error(), "has no hetzner account") {
		t.Fatal(err)
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

func TestGitReadsAnyRepositoryAndPushesOnlyInScope(t *testing.T) {
	a := testApp(t)
	s, err := a.newSession("work", nil, []string{"company/app"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	for _, args := range [][]string{{"clone", "https://github.com/company/app.git"}, {"clone", "--depth", "1", "https://github.com/public/plugins.git"}, {"ls-remote", "git@github.com:public/plugins.git"}, {"fetch", "https://example.com/plugins.git"}, {"status"}, {"push", "https://github.com/company/app.git", "main"}, {"push", "/tmp/local.git", "main"}} {
		if err := a.checkGit(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"push", "https://github.com/private/app.git", "main"}, {"push", "-o", "ci.skip", "git@github.com:private/app.git"}, {"push", "--repo=https://github.com/company/app.git", "https://github.com/private/app.git"}, {"push", "https://example.com/app.git"}} {
		if a.checkGit(args) == nil {
			t.Fatal("unselected push target accepted", args)
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
	for _, args := range [][]string{{"-C", dir, "push", "--repo=private", "main"}, {"-C", dir, "push", "--repo", "origin", "private"}} {
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
	for value, want := range map[string]string{"microsoft 365=me": "unknown service 'microsoft 365'; use codex, claude, openai-compatible, github, azure, office", "github=nobody": "no github login matches 'nobody'", "hetzner": "service=selector"} {
		if err := a.sessionCommand([]string{"new", "third", "--accounts", value}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(value, err)
		}
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
	if got := a.out.(*bytes.Buffer).String(); got != "client\n  accounts       none\n\nother\n  accounts       none\n  repositories   acme/* · acme/tool\n" {
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
	ui.wait(t, "[q] quit")
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
	ui.send(t, "\x1b[B\x1b[B ")
	ui.wait(t, "[x] acme/*")
	ui.send(t, "\x1b[B ")
	ui.wait(t, "[ ] alice/tool")
	if current, err := a.readSession(s.ID); err != nil || !reflect.DeepEqual(current.Repositories, []string{"acme/*"}) {
		t.Fatal("repository edit was not saved immediately", current.Repositories, err)
	}
	ui.send(t, "\x1b")
	ui.wait(t, "[q] quit")
	ui.send(t, "q")
	if err := ui.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if s, err = a.readSession(s.ID); err != nil || !reflect.DeepEqual(s.Repositories, []string{"acme/*"}) {
		t.Fatal("repositories not saved", s.Repositories, err)
	}
}

func TestConcurrentSessionChangesStayConsistent(t *testing.T) {
	a := testApp(t)
	s := addSession(t, a, "shared")
	var wg sync.WaitGroup
	created := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, err := a.newSession("same", nil, nil); created <- err }()
		go func(i int) {
			defer wg.Done()
			a.updateSession(s.ID, func(s *session) error {
				s.Repositories = append(s.Repositories, fmt.Sprintf("owner%d/*", i))
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(created)
	successes := 0
	for err := range created {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("created duplicate or no sessions", successes)
	}
	if s, err := a.readSession(s.ID); err != nil || len(s.Repositories) != 8 {
		t.Fatal("lost concurrent updates", s.Repositories, err)
	}
}

// Each shell carries its own session. The last opened one only marks where
// the menu starts and never changes which session a command uses.
func TestLastSessionNeverChangesActiveSessions(t *testing.T) {
	a := testApp(t)
	first := addSession(t, a, "first")
	second := addSession(t, a, "second")
	if a.lastSession() != "" {
		t.Fatal("remembered a session before any shell")
	}
	shell := filepath.Join(t.TempDir(), "shell")
	mustWrite(t, shell, []byte("#!/bin/sh\necho \"shell $COMMANDER_SESSION\"\n"))
	if err := os.Chmod(shell, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	for _, s := range []session{first, second} {
		if err := a.sessionShell(s); err != nil {
			t.Fatal(err)
		}
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "shell "+first.ID+"\n") || !strings.Contains(out, "shell "+second.ID+"\n") || a.lastSession() != second.ID {
		t.Fatal("shells did not keep their own sessions", out)
	}
	for id, want := range map[string]string{first.ID: "session: first\n", "": "session: none\n"} {
		t.Setenv("COMMANDER_SESSION", id)
		a.session = nil
		if out, err := memoryCommand(t, a, nil); err != nil || !strings.HasPrefix(out, want) {
			t.Fatal("last session changed the active session", out, err)
		}
	}
	mustWrite(t, filepath.Join(a.root, ".sessions", "last"), []byte("../escape\n"))
	if a.lastSession() != "" {
		t.Fatal("accepted an invalid last session")
	}
}

func TestPruneSessionWrappersPreservesActiveShells(t *testing.T) {
	a := testApp(t)
	active := addSession(t, a, "active")
	idle := addSession(t, a, "idle")
	legacy := func(s session) string {
		return filepath.Join(a.root, ".sessions", s.ID, "bin", "gh")
	}
	for _, s := range []session{active, idle} {
		mustWrite(t, legacy(s), a.wrapper("gh", quote(s.ID)))
	}
	custom := filepath.Join(filepath.Dir(legacy(idle)), "custom")
	mustWrite(t, custom, []byte("keep"))
	unlock, err := a.sessionLock(active.ID, true, false)
	if err != nil {
		t.Fatal(err)
	}
	a.pruneSessionWrappers()
	unlock()
	if _, err = os.Stat(legacy(active)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(legacy(idle)); !os.IsNotExist(err) {
		t.Fatal("obsolete wrapper remains", err)
	}
	if _, err = os.Stat(custom); err != nil {
		t.Fatal(err)
	}
	a.pruneSessionWrappers()
	if _, err = os.Stat(filepath.Dir(legacy(active))); !os.IsNotExist(err) {
		t.Fatal("obsolete wrapper directory remains", err)
	}
}
