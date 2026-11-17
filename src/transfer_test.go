package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSelectiveArchiveAccountsSessionsAndMemory(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	work, err := source.newSession("work", members, []string{"company/*"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source.root, "MEMORY.md"), []byte("* Keep replies concise.\n"))
	payload, err := source.selectedArchive(members, []session{work}, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "selected.zip")
	if err = writeArchive(path, payload, "test password"); err != nil {
		t.Fatal(err)
	}
	payload, err = readArchive(path, "test password")
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	s, err := target.newSession("existing", []member{ghMember("other")}, []string{"private/*"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = target.readSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	selected := []member{members[0], members[3]}
	if err = target.importSelected(payload, importSelection{Accounts: selected, Sessions: []string{"work"}, Memory: "replace"}); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"github", "office"} {
		state, err := newStorage(target.root).state(service)
		if err != nil {
			t.Fatal(err)
		}
		var only []member
		for _, m := range selected {
			if m.Service == service {
				only = append(only, m)
			}
		}
		want, _ := selectLogins(service, payload.Logins[service], only)
		got, err := selectLogins(service, state, only)
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal("selected account changed", service, err)
		}
	}
	if state, _ := newStorage(target.root).state("hetzner"); len(state) != 0 {
		t.Fatal("imported unselected account")
	}
	after, err := target.readSession(s.ID)
	if err != nil || !reflect.DeepEqual(s, after) {
		t.Fatal("changed running session", err)
	}
	imported, err := target.findSession("work")
	if err != nil || imported.ID == work.ID || !reflect.DeepEqual(imported.Repositories, []string{"company/*"}) || len(imported.Accounts) != len(members) {
		t.Fatal("session was not imported with its repositories", imported, err)
	}
	if err = target.importSelected(payload, importSelection{Sessions: []string{"work"}}); err == nil {
		t.Fatal("replaced a session without confirmation")
	}
	if err = target.importSelected(payload, importSelection{Sessions: []string{"work"}, Replace: map[string]bool{"session:work": true}}); err != nil {
		t.Fatal(err)
	}
	if replaced, err := target.findSession("work"); err != nil || replaced.ID != imported.ID {
		t.Fatal("replacing a session changed its ID", err)
	}
	memory, _ := os.ReadFile(filepath.Join(target.root, "MEMORY.md"))
	if string(memory) != *payload.Memory {
		t.Fatal("memory was not imported")
	}
}

func TestArchiveMemoryOnly(t *testing.T) {
	source := testApp(t)
	fixtureLogins(t, source)
	mustWrite(t, filepath.Join(source.root, "MEMORY.md"), []byte("* Keep replies concise.\n"))
	payload, err := source.selectedArchive(nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Logins) != 0 || len(payload.Profile.Profiles) != 0 {
		t.Fatal("memory export contains accounts")
	}
	target := testApp(t)
	mustWrite(t, filepath.Join(target.root, "MEMORY.md"), []byte("* Keep replies concise.\n* Test meaningful changes.\n"))
	if err = target.importSelected(payload, importSelection{Memory: "merge"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(target.root, "MEMORY.md"))
	if string(got) != "* Keep replies concise.\n* Test meaningful changes.\n" {
		t.Fatal("memory duplicate", string(got))
	}
}

func TestSelectiveImportConflictsAndNativeDefaults(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	payload, err := source.selectedArchive(members, nil, false)
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
	if err = target.importSelected(payload, importSelection{Accounts: members}); err != nil {
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
	if err = target.importSelected(payload, importSelection{Accounts: members[:1], Replace: map[string]bool{members[0].id(): true}}); err != nil {
		t.Fatal(err)
	}
	got, _ := newStorage(target.root).state("github")
	entry := obj(got["github.com"])
	if entry["user"] != "other" || entry["oauth_token"] != "other-secret" {
		t.Fatal("replacement changed another account")
	}
}
