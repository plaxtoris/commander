package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGitHubHealthDistinguishesRejectedLogin(t *testing.T) {
	for _, tc := range []struct{ state, message, want string }{
		{"success", "", "Ready"},
		{"refresh_expired", "", "Sign in again"},
		{"error", "HTTP 401: Bad credentials", "Sign in again"},
		{"timeout", "private-fixture", "Check connection"},
		{"error", "HTTP 403: rate limit exceeded", "Could not check"},
		{"error", "HTTP 503: unavailable", "Could not check"},
		{"error", "dial tcp: private-fixture", "Could not check"},
		{"future-state", "", "Could not check"},
	} {
		t.Run(tc.state+tc.message, func(t *testing.T) {
			a := testApp(t)
			a.run = func(string, []string, bool, map[string]string) ([]byte, error) {
				return encode(object{"hosts": object{"github.com": []any{object{"login": "alice", "state": tc.state, "error": tc.message, "token": "private-fixture"}}}}), nil
			}
			rows := a.resolve([]member{ghMember("alice")})
			if len(rows) != 1 || rows[0].State != tc.want {
				t.Fatal(rows)
			}
			if a.health()[ghMember("alice").id()].State != tc.want {
				t.Fatal("status was not remembered")
			}
			data, err := os.ReadFile(filepath.Join(a.root, ".sessions", "health.json"))
			if err != nil || bytes.Contains(data, []byte("private-fixture")) {
				t.Fatal("health must contain no native error or token", err)
			}
		})
	}
}

func TestRememberedHealthSurvivesRestartAndOlderChecks(t *testing.T) {
	a := testApp(t)
	m := ghMember("alice")
	s := addSession(t, a, "work", m)
	a.rememberHealth([]account{{member: m, State: "Sign in again"}})
	b, err := newApp(a.root, os.Stdin, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil || len(healthIssues(s, b.health())) != 1 {
		t.Fatal("restart lost the warning", err)
	}
	old := time.Now().UTC()
	b.rememberHealth([]account{{member: m, State: "Ready"}})
	b.rememberHealthAt([]account{{member: m, State: "Sign in again"}}, old)
	if len(healthIssues(s, a.health())) != 0 {
		t.Fatal("an older check replaced a successful reconnect")
	}
	if len(healthIssues(session{Accounts: []member{ghMember("other")}}, a.health())) != 0 {
		t.Fatal("warning appeared in an unrelated session")
	}
}

func TestReconnectGitHubVerifiesIdentityAndPreservesOtherLogins(t *testing.T) {
	for _, outcome := range []string{"success", "wrong account", "cancel", "verify failure"} {
		t.Run(outcome, func(t *testing.T) {
			a := testApp(t)
			m := ghMember("alice")
			s := addSession(t, a, "work", m)
			s, _ = a.readSession(s.ID)
			before := []byte(`{"github.com":{"user":"bob","oauth_token":"bob-fixture","users":{"alice":{"oauth_token":"old-fixture"},"bob":{"oauth_token":"bob-fixture"}}}}`)
			path := filepath.Join(a.root, ".secrets", ".config", "gh", "hosts.yml")
			mustWrite(t, path, before)
			a.rememberHealth([]account{{member: m, State: "Sign in again"}})
			a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
				if tool != "gh" || extra["GH_CONFIG_DIR"] == filepath.Dir(path) {
					t.Fatal("login was not isolated")
				}
				if args[0] == "auth" && args[1] == "login" {
					if outcome == "cancel" {
						return nil, errCancelled
					}
					if !contains(args, "--expiring") || !contains(args, "--insecure-storage") || !contains(args, "github.com") {
						t.Fatal("login lost its host or storage settings", args)
					}
					mustWrite(t, filepath.Join(extra["GH_CONFIG_DIR"], "hosts.yml"), []byte(`{"github.com":{"user":"alice","oauth_token":"new-fixture","users":{"alice":{"oauth_token":"new-fixture"}}}}`))
					return nil, nil
				}
				if outcome == "wrong account" {
					return []byte("bob\n"), nil
				}
				if outcome == "verify failure" {
					return nil, errors.New("verification unavailable")
				}
				return []byte("alice\n"), nil
			}
			err := a.reconnectGitHub(m)
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if outcome != "success" {
				if err == nil || !bytes.Equal(before, after) || len(healthIssues(s, a.health())) != 1 {
					t.Fatal("failed login changed saved credentials or cleared the warning", err)
				}
			} else {
				state, readErr := newStorage(a.root).state("github")
				host := obj(state["github.com"])
				users := obj(host["users"])
				if err != nil || readErr != nil || host["user"] != "bob" || host["oauth_token"] != "bob-fixture" || obj(users["bob"])["oauth_token"] != "bob-fixture" || obj(users["alice"])["oauth_token"] != "new-fixture" {
					t.Fatal("reconnect changed the wrong credentials", err, readErr)
				}
				if len(healthIssues(s, a.health())) != 0 {
					t.Fatal("successful reconnect kept its warning")
				}
			}
			current, err := a.readSession(s.ID)
			if err != nil || !reflect.DeepEqual(current.Accounts, s.Accounts) {
				t.Fatal("reconnect changed the session", err)
			}
			staging, _ := filepath.Glob(filepath.Join(a.root, ".secrets", ".login-*"))
			if len(staging) != 0 {
				t.Fatal("temporary login was not removed")
			}
		})
	}
}

func TestInteractiveReconnectFromSessionWarning(t *testing.T) {
	a := testApp(t)
	m := ghMember("alice")
	addSession(t, a, "work", m)
	mustWrite(t, filepath.Join(a.root, "tools", ".requested", "gh"), nil)
	t.Setenv("COMMANDER_UI_GITHUB_EXPIRED", "1")
	s := openUI(t, a.root)
	s.wait(t, "1 account needs attention")
	s.send(t, "l")
	s.wait(t, "[l] sign in again")
	s.send(t, "l")
	s.wait(t, "alice connected")
	s.send(t, "\x1b")
	s.wait(t, "[q] quit")
	start := strings.LastIndex(s.output, "commander")
	if strings.Contains(s.output[start:], "needs attention") {
		t.Fatal("home retained the warning after reconnect", s.output[start:])
	}
	s.send(t, "q")
	if err := s.command.Wait(); err != nil {
		t.Fatal(err)
	}
	if !ready(a.health()[m.id()].State) {
		t.Fatal("reconnect was not remembered")
	}
}
