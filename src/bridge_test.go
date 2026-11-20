package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBridgeCredentialsFollowLiveSessionSelection(t *testing.T) {
	a := testApp(t)
	fixtureLogins(t, a)
	preparedTools(t, a)
	s, err := a.newSession("container", []member{ghMember("alice")}, []string{"company/app"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	a.run = func(tool string, args []string, capture bool, extra map[string]string) ([]byte, error) {
		if tool != "gh" || len(args) < 2 || args[0] != "auth" || args[1] != "token" {
			t.Fatal("unexpected native call", tool, args)
		}
		if !strings.Contains(extra["GH_CONFIG_DIR"], ".run-") {
			t.Fatal("shared login directory exposed")
		}
		return []byte("fixture-token"), nil
	}
	request := func(repo string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/git", strings.NewReader("protocol=https\nhost=github.com\npath="+repo+".git\n\n"))
		w := httptest.NewRecorder()
		a.bridgeRequest(w, r)
		return w
	}
	w := request("company/app")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "username=alice\npassword=fixture-token") {
		t.Fatal("credential connection failed", w.Code)
	}
	w = request("private/app")
	if w.Code != 403 || strings.Contains(w.Body.String(), "fixture-token") {
		t.Fatal("credential scope escaped")
	}
	s.Repositories = nil
	if err = a.saveSession(s); err != nil {
		t.Fatal(err)
	}
	if w = request("company/app"); w.Code != 403 {
		t.Fatal("container used stale selection")
	}
	leftovers, _ := filepath.Glob(filepath.Join(a.root, ".secrets", ".runtime", ".run-*"))
	if len(leftovers) != 0 {
		t.Fatal("temporary credentials left")
	}
}

func TestBridgeAllowsOnlySelectedProductTargetsAndPreservesStatus(t *testing.T) {
	a, project := taskFixture(t)
	a = taskSession(t, a, "container")
	mustWrite(t, filepath.Join(project, "Makefile"), []byte("dev:\n\t@printf 'dev output\\n'\nprd:\n\t@printf 'prd error\\n' >&2; exit 7\n"))
	id := strings.Repeat("a", 24)
	path, err := a.containerFile(id)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	mustWrite(t, path, encode(containerProject{ID: id, Project: project, Container: strings.Repeat("b", 64), Workspace: "/workspace"}))
	for _, target := range []string{"dev", "prd", "shell"} {
		r := httptest.NewRequest("POST", "/make/"+id+"/"+target, nil)
		w := httptest.NewRecorder()
		a.bridgeRequest(w, r)
		if target == "shell" {
			if w.Code != 404 {
				t.Fatal("arbitrary target accepted")
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal("product target rejected", w.Code)
		}
		decoder := json.NewDecoder(w.Body)
		output := ""
		code := -1
		for {
			var event bridgeEvent
			err := decoder.Decode(&event)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			output += string(event.Data)
			if event.Exit != nil {
				code = *event.Exit
			}
		}
		if target == "dev" && (code != 0 || !strings.Contains(output, "dev output")) {
			t.Fatal("lost stdout or exit status", output, code)
		}
		if target == "prd" && (code != 2 || !strings.Contains(output, "prd error")) {
			t.Fatal("lost stderr or exit status", output, code)
		}
	}
}

func TestCredentialRefreshAcrossAccountsDoesNotRace(t *testing.T) {
	a := testApp(t)
	fixtureLogins(t, a)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, m := range []member{ghMember("alice"), ghMember("other")} {
		wg.Add(1)
		go func(m member) {
			defer wg.Done()
			root, _, finish, err := a.accountRuntime(m)
			if err != nil {
				results <- err
				return
			}
			defer os.RemoveAll(root)
			results <- finish()
		}(m)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := newStorage(a.root).state("github")
	if err != nil {
		t.Fatal(err)
	}
	if len(obj(obj(state["github.com"])["users"])) != 2 || obj(state["github.com"])["user"] != "other" {
		t.Fatal("refresh lost another account or switched the default")
	}
}
