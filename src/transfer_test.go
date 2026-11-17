package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionArchiveHoldsOnlyItsSessionAndLogins(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	work, err := source.newSession("work", []member{members[0], members[3]}, []string{"company/*"})
	if err != nil {
		t.Fatal(err)
	}
	addSession(t, source, "private", members...)
	mustWrite(t, filepath.Join(source.root, "MEMORY.md"), []byte("* Keep replies concise.\n"))
	payload, err := source.sessionArchive(work)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Memory != nil || len(payload.Profile.Profiles) != 1 || len(payload.Logins) != 2 || !reflect.DeepEqual(payload.Accounts, work.Accounts) {
		t.Fatal("archive holds more than its session")
	}
	stale := session{Name: "stale", Accounts: []member{members[0], ghMember("gone")}, Repositories: []string{}}
	if _, err = source.sessionArchive(stale); err == nil || !strings.Contains(err.Error(), "gone @ github.com has no saved login") {
		t.Fatal("export did not name the missing login", err)
	}
	target := testApp(t)
	existing, err := target.readSession(addSession(t, target, "existing", ghMember("other")).ID)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := target.importChoices(payload, func(q string) bool { t.Fatal("asked without a conflict:", q); return false })
	if err != nil {
		t.Fatal(err)
	}
	if err = target.importSelected(payload, selection); err != nil {
		t.Fatal(err)
	}
	for _, m := range work.Accounts {
		state, err := newStorage(target.root).state(m.Service)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := selectLogins(m.Service, payload.Logins[m.Service], []member{m})
		got, err := selectLogins(m.Service, state, []member{m})
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal("login was not imported", m.Service, err)
		}
	}
	if state, _ := newStorage(target.root).state("hetzner"); len(state) != 0 {
		t.Fatal("imported a login outside the session")
	}
	if after, err := target.readSession(existing.ID); err != nil || !reflect.DeepEqual(existing, after) {
		t.Fatal("changed another session", err)
	}
	imported, err := target.findSession("work")
	if err != nil || !reflect.DeepEqual(imported.Repositories, []string{"company/*"}) || len(imported.Accounts) != 2 || imported.Accounts[1].id() != work.Accounts[1].id() {
		t.Fatal("session was not imported with its repositories", imported, err)
	}
	asked := []string{}
	if _, err = target.importChoices(payload, func(q string) bool { asked = append(asked, q); return false }); err != errCancelled || !reflect.DeepEqual(asked, []string{"replace session work?"}) {
		t.Fatal("replaced a session without confirmation", asked, err)
	}
	if selection, err = target.importChoices(payload, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err = target.importSelected(payload, selection); err != nil {
		t.Fatal(err)
	}
	if replaced, err := target.findSession("work"); err != nil || replaced.ID != imported.ID {
		t.Fatal("replacing a session changed its ID", err)
	}
}

func TestOlderArchiveImportsOnlySessionsWithTheirLogins(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	payload, err := source.sessionArchive(session{Name: "work", Accounts: members, Repositories: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	// Older archives could hold loose accounts and memory beside sessions.
	memory := "* Old preference.\n"
	payload.Profile.Profiles["work"], payload.Memory = members[:1], &memory
	target := testApp(t)
	selection, err := target.importChoices(payload, func(string) bool { return true })
	if err != nil || !reflect.DeepEqual(selection.Accounts, members[:1]) || !reflect.DeepEqual(selection.Sessions, []string{"work"}) {
		t.Fatal("selected more than the session uses", selection, err)
	}
	if err = target.importSelected(payload, selection); err != nil {
		t.Fatal(err)
	}
	if state, _ := newStorage(target.root).state("hetzner"); len(state) != 0 {
		t.Fatal("imported a loose account")
	}
	if _, err = os.Stat(filepath.Join(target.root, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatal("imported memory", err)
	}
	delete(payload.Profile.Profiles, "work")
	if _, err = target.importChoices(payload, func(string) bool { return true }); err == nil {
		t.Fatal("accepted an archive without a session")
	}
}

func TestImportConflictsKeepNativeDefaults(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	payload, err := source.sessionArchive(session{Name: "work", Accounts: members, Repositories: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	fixtureLogins(t, target)
	before := map[string]object{}
	for _, m := range members {
		before[m.Service], err = newStorage(target.root).state(m.Service)
		if err != nil {
			t.Fatal(err)
		}
	}
	selection, err := target.importChoices(payload, func(q string) bool { t.Fatal("asked about identical logins:", q); return false })
	if err != nil || len(selection.Accounts) != len(members) {
		t.Fatal("did not select the session logins", err)
	}
	if err = target.importSelected(payload, selection); err != nil {
		t.Fatal(err)
	}
	for service, want := range before {
		got, err := newStorage(target.root).state(service)
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal("changed native default or unrelated account", service, err)
		}
	}
	host := obj(payload.Logins["github"]["github.com"])
	host["oauth_token"] = "replacement"
	obj(obj(host["users"])["alice"])["oauth_token"] = "replacement"
	if target.importSelected(payload, importSelection{Accounts: members[:1]}) == nil {
		t.Fatal("silently replaced credentials")
	}
	asked := []string{}
	selection, err = target.importChoices(payload, func(q string) bool { asked = append(asked, q); return q != "replace session work?" })
	if err != errCancelled || len(asked) != 1 {
		t.Fatal("a declined session still imported its logins", asked, err)
	}
	asked = nil
	selection, err = target.importChoices(payload, func(q string) bool { asked = append(asked, q); return true })
	if err != nil || len(asked) != 2 || !selection.Replace[members[0].id()] {
		t.Fatal("login replacement was not confirmed", asked, err)
	}
	if err = target.importSelected(payload, selection); err != nil {
		t.Fatal(err)
	}
	got, _ := newStorage(target.root).state("github")
	entry := obj(got["github.com"])
	if entry["user"] != "other" || entry["oauth_token"] != "other-secret" {
		t.Fatal("replacement changed another account")
	}
}
