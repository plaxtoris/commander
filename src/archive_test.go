package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yeka/zip"
)

func msalFixture(user, home, realm, client string) object {
	return object{
		"Account":      object{user: object{"username": user, "home_account_id": home, "local_account_id": user + "-local", "realm": realm, "environment": "login.microsoftonline.com"}},
		"RefreshToken": object{user + client: object{"home_account_id": home, "client_id": client, "secret": user + "-refresh"}},
		"AccessToken":  object{user + client: object{"home_account_id": home, "client_id": client, "realm": realm, "secret": user + "-access"}},
		"IdToken":      object{}, "AppMetadata": object{client: object{"client_id": client}},
	}
}

func fixtureLogins(t *testing.T, a *app) []member {
	t.Helper()
	store := filepath.Join(a.root, ".secrets")
	mustWrite(t, filepath.Join(store, ".config/gh/hosts.yml"), []byte("github.com:\n  user: other\n  oauth_token: other-secret\n  git_protocol: https\n  users:\n    alice:\n      oauth_token: alice-secret\n    other:\n      oauth_token: other-secret\n"))
	mustWrite(t, filepath.Join(store, ".config/hcloud/cli.toml"), []byte("active_context = 'other'\n[[contexts]]\nname = 'project'\ntoken = 'project-secret'\n[[contexts]]\nname = 'other'\ntoken = 'other-secret'\n"))
	azure := msalFixture("alice", "alice-home", "tenant", azureClient)
	other := msalFixture("other", "other-home", "tenant", azureClient)
	azure, _ = mergeCache(azure, other)
	mustWrite(t, filepath.Join(store, ".azure/azureProfile.json"), encode(object{"subscriptions": []any{object{"id": "sub", "name": "Production", "tenantId": "tenant", "user": object{"name": "alice", "type": "user"}, "isDefault": false}, object{"id": "other-sub", "tenantId": "tenant", "user": object{"name": "other", "type": "user"}, "isDefault": true}}}))
	mustWrite(t, filepath.Join(store, ".azure/msal_token_cache.json"), encode(azure))
	connection := object{"name": "office", "identityName": "alice", "identityId": "alice-local", "authType": "deviceCode", "appId": "office-app", "tenant": "tenant", "active": true}
	mustWrite(t, filepath.Join(store, ".cli-m365-all-connections.json"), encode([]any{connection}))
	mustWrite(t, filepath.Join(store, ".cli-m365-connection.json"), encode(connection))
	mustWrite(t, filepath.Join(store, ".cli-m365-msal-cache.json"), encode(msalFixture("alice", "alice-home", "tenant", "office-app")))
	path := filepath.Join(store, ".config/gcloud/credentials.db")
	os.MkdirAll(filepath.Dir(path), 0700)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE credentials (account_id TEXT PRIMARY KEY, value BLOB)"); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "other"} {
		if _, err = db.Exec("INSERT INTO credentials VALUES (?,?)", user, encode(object{"type": "authorized_user", "refresh_token": user + "-refresh", "client_id": "gcloud", "client_secret": "fixture"})); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return []member{ghMember("alice"), hMember("project"), {Service: "azure", Key: jsonKey("sub"), Account: "alice", Context: "Production"}, {Service: "office", Key: jsonKey("office"), Account: "alice", Context: "tenant"}, {Service: "gcloud", Key: jsonKey("alice"), Account: "alice", Context: ""}}
}

func TestProfileArchiveRoundTripAllServices(t *testing.T) {
	source := testApp(t)
	members := fixtureLogins(t, source)
	saveProfile(t, source, "work", members...)
	payload, err := source.exportPayload("work", members)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(payload)), "other-secret") || strings.Contains(string(encode(payload)), "other-refresh") {
		t.Fatal("export included unselected login")
	}
	path := filepath.Join(t.TempDir(), "profile.zip")
	if err = writeArchive(path, payload, "fixture password"); err != nil {
		t.Fatal(err)
	}
	encrypted, _ := os.ReadFile(path)
	if bytes.Contains(encrypted, []byte("alice-secret")) {
		t.Fatal("unencrypted export")
	}
	if _, err = readArchive(path, "wrong password"); err == nil {
		t.Fatal("accepted wrong password")
	}
	restored, err := readArchive(path, "fixture password")
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	name, err := target.importPayload(restored, "")
	if err != nil || name != "work" {
		t.Fatal(name, err)
	}
	again, err := target.exportPayload("work", members)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload, again) {
		t.Fatal("round trip changed selected native records")
	}
	if err = writeArchive(path, payload, "fixture password"); err == nil {
		t.Fatal("overwrote archive")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("archive permissions", info.Mode())
	}
	if _, err = target.importPayload(restored, ""); err == nil {
		t.Fatal("overwrote existing profile")
	}
}

func TestArchiveMergePreservesDefaultsAndRejectsConflicts(t *testing.T) {
	source := testApp(t)
	fixtureLogins(t, source)
	payload, err := source.exportPayload("incoming", []member{ghMember("alice"), hMember("project")})
	if err != nil {
		t.Fatal(err)
	}
	target := testApp(t)
	saveProfile(t, target, "existing", ghMember("other"))
	mustWrite(t, filepath.Join(target.root, ".secrets/.config/gh/hosts.yml"), encode(object{"github.com": object{"user": "other", "oauth_token": "other-secret", "users": object{"other": object{"oauth_token": "other-secret"}}}}))
	if _, err = target.importPayload(payload, ""); err != nil {
		t.Fatal(err)
	}
	p, _ := target.readProfiles()
	if *p.Active != "existing" {
		t.Fatal("switched profile")
	}
	state, err := newStorage(target.root).state("github")
	if err != nil || obj(state["github.com"])["user"] != "other" {
		t.Fatal("switched native default", err)
	}
	if _, err = target.importPayload(payload, "shared"); err != nil {
		t.Fatal("identical logins should be reusable", err)
	}
	changed := payload
	json.Unmarshal(encode(payload), &changed)
	obj(obj(obj(changed.Logins["github"]["github.com"])["users"])["alice"])["oauth_token"] = "different-secret"
	// Keep the selected native representation internally consistent.
	obj(changed.Logins["github"]["github.com"])["oauth_token"] = "different-secret"
	before, _ := os.ReadFile(filepath.Join(target.root, "profiles.json"))
	if _, err = target.importPayload(changed, "conflict"); err == nil {
		t.Fatal("accepted conflicting credential")
	}
	after, _ := os.ReadFile(filepath.Join(target.root, "profiles.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed import changed profiles")
	}
}

func TestArchiveRejectsExtraAccountsAndAgentPaths(t *testing.T) {
	a := testApp(t)
	fixtureLogins(t, a)
	payload, err := a.exportPayload("work", []member{ghMember("alice")})
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(payload.Logins["github"]["github.com"])["users"])["intruder"] = object{"oauth_token": "hidden"}
	if _, err = testApp(t).importPayload(payload, ""); err == nil {
		t.Fatal("accepted extra account")
	}
	store := newStorage(a.root)
	for _, path := range []string{"../.codex/auth.json", ".codex/auth.json", ".claude.json", "/tmp/secret"} {
		if _, err = store.read(path); err == nil {
			t.Fatal("accepted nonservice path")
		}
	}
	if err = store.publish(map[string][]byte{".codex/auth.json": []byte("no")}); err == nil {
		t.Fatal("published agent data")
	}
}

func TestNativeStorageRejectsSymlinkAndConcurrentChange(t *testing.T) {
	a := testApp(t)
	fixtureLogins(t, a)
	store := newStorage(a.root)
	if _, err := store.read(".config/gh/hosts.yml"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.root, ".secrets/.config/gh/hosts.yml")
	mustWrite(t, path, []byte("changed"))
	if err := store.publish(map[string][]byte{".secrets/.config/gh/hosts.yml": []byte("replacement")}); err == nil {
		t.Fatal("overwrote concurrent native change")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(t.TempDir(), "secret"), path)
	if _, err := newStorage(a.root).read(".config/gh/hosts.yml"); err == nil {
		t.Fatal("followed login symlink")
	}
}

func TestMSALAccountTenantAndAppFiltering(t *testing.T) {
	cache := msalFixture("alice", "home", "tenant-a", azureClient)
	obj(cache["Account"])["alice-b"] = object{"username": "alice", "home_account_id": "home", "local_account_id": "local-b", "realm": "tenant-b", "environment": "login.microsoftonline.com"}
	obj(cache["AccessToken"])["b"] = object{"home_account_id": "home", "client_id": azureClient, "realm": "tenant-b", "secret": "wrong-tenant"}
	obj(cache["RefreshToken"])["wrong-app"] = object{"home_account_id": "home", "client_id": "wrong-app", "secret": "wrong-app"}
	selected, err := selectMSAL(cache, []string{"alice"}, []string{azureClient}, nil, []string{"tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(selected)), "wrong-tenant") || strings.Contains(string(encode(selected)), "wrong-app") || len(obj(selected["Account"])) != 1 {
		t.Fatal("leaked another tenant or app")
	}
	obj(cache["Account"])["ambiguous"] = object{"username": "alice", "home_account_id": "different-home", "realm": "tenant-a", "environment": "login.microsoftonline.com"}
	if _, err = selectMSAL(cache, []string{"alice"}, []string{azureClient}, nil, []string{"tenant-a"}); err == nil {
		t.Fatal("accepted ambiguous identity")
	}
}

func TestArchiveIntegrityAndEncryption(t *testing.T) {
	a := testApp(t)
	fixtureLogins(t, a)
	payload, err := a.exportPayload("work", []member{hMember("project")})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{archiveEntry, "../outside", ".codex/auth.json"} {
		path := filepath.Join(t.TempDir(), "bad.zip")
		f, _ := os.Create(path)
		z := zip.NewWriter(f)
		w, _ := z.Create(entry)
		w.Write(encode(payload))
		z.Close()
		f.Close()
		if _, err := readArchive(path, "password"); err == nil {
			t.Fatal("accepted unencrypted or invalid archive")
		}
	}
	path := filepath.Join(t.TempDir(), "corrupt.zip")
	if err = writeArchive(path, payload, "password"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	data[len(data)/3] ^= 0x40
	mustWrite(t, path, data)
	if _, err = readArchive(path, "password"); err == nil {
		t.Fatal("accepted tampered archive")
	}
}

func TestZIPCompatibilityWith7Zip(t *testing.T) {
	seven, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z optional interoperability check")
	}
	a := testApp(t)
	fixtureLogins(t, a)
	payload, err := a.exportPayload("work", []member{ghMember("alice"), hMember("project")})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, archiveEntry), encode(payload))
	path := filepath.Join(dir, "legacy.zip")
	cmd := exec.Command(seven, "a", "-tzip", "-mem=AES256", "-p", path, archiveEntry)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("fixture-password\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	got, err := readArchive(path, "fixture-password")
	if err != nil || !bytes.Equal(encode(got), encode(payload)) {
		t.Fatal("cannot import legacy ZIP", err)
	}
	path = filepath.Join(dir, "new.zip")
	if err = writeArchive(path, payload, "fixture-password"); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(seven, "x", "-so", path, archiveEntry)
	cmd.Stdin = strings.NewReader("fixture-password\n")
	out, err := cmd.Output()
	if err != nil || !bytes.Equal(out, encode(payload)) {
		t.Fatal("7z cannot read new ZIP", err)
	}
}

func TestRefreshableGitHubLogin(t *testing.T) {
	a := testApp(t)
	m := ghMember("alice")
	native := object{"github.com": object{"user": "alice", "users": object{"alice": object{"refreshable_oauth_token": object{"access_token": "access", "refresh_token": "refresh", "expires_at": "2026-12-01T00:00:00Z", "refresh_token_expires_at": "2027-01-01T00:00:00Z"}}}}}
	mustWrite(t, filepath.Join(a.root, ".secrets/.config/gh/hosts.yml"), encode(native))
	payload, err := a.exportPayload("work", []member{m})
	if err != nil {
		t.Fatal(err)
	}
	b := testApp(t)
	if _, err = b.importPayload(payload, ""); err != nil {
		t.Fatal(err)
	}
	again, err := b.exportPayload("work", []member{m})
	if err != nil || !reflect.DeepEqual(payload, again) {
		t.Fatal("lost refreshable GitHub fields", err)
	}
}
