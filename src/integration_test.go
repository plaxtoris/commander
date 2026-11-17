//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func integrationApp(t *testing.T) *app {
	t.Helper()
	root := os.Getenv("COMMANDER_INTEGRATION_HOME")
	if root == "" {
		t.Skip("set COMMANDER_INTEGRATION_HOME to a disposable installation")
	}
	if _, err := os.Stat(filepath.Join(root, ".integration-fixture")); err != nil {
		t.Fatal("integration installation must have an explicit .integration-fixture marker")
	}
	a, err := newApp(root, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestIntegrationNativeFixtures(t *testing.T) {
	a := integrationApp(t)
	if err := os.RemoveAll(filepath.Join(a.root, ".secrets")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.root, ".secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	members := fixtureLogins(t, a)
	for _, service := range []string{"azure", "office", "hetzner", "gcloud"} {
		rows, err := a.accounts(service)
		if err != nil || len(rows) == 0 {
			t.Fatalf("%s native fixture: %v", service, err)
		}
	}
	payload, err := a.exportPayload("fixture", members)
	if err != nil {
		t.Fatal(err)
	}
	// Test imports through the real native tools using this disposable store only.
	store := filepath.Join(a.root, ".secrets")
	backup := store + ".fixture-backup"
	if err = os.Rename(store, backup); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(store, 0700)
	t.Cleanup(func() {
		os.RemoveAll(store)
		os.Rename(backup, store)
		os.Remove(filepath.Join(a.root, "profiles.json"))
	})
	if _, err = a.importPayload(payload, ""); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"azure", "office", "hetzner", "gcloud"} {
		rows, err := a.accounts(service)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s imported fixture: %d rows, %v", service, len(rows), err)
		}
	}
	if err = a.officeSettings(); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"copyDeviceCodeToClipboard", "false"}, {"autoOpenLinksInBrowser", "false"}, {"authType", "deviceCode"}} {
		data, err := a.run("m365", []string{"cli", "config", "get", "--key", pair[0], "--output", "text"}, true, nil)
		if err != nil || strings.TrimSpace(string(data)) != pair[1] {
			t.Fatal("Office terminal settings", pair, err)
		}
	}
}

func TestIntegrationProjectContainer(t *testing.T) {
	a := integrationApp(t)
	project := filepath.Join(a.root, "workspaces", "container fixture")
	mustWrite(t, filepath.Join(project, ".devcontainer", "devcontainer.json"), []byte(`{"name":"Commander fixture","build":{"dockerfile":"Dockerfile"},"workspaceMount":"source=${localWorkspaceFolder},target=/project,type=bind","workspaceFolder":"/project"}`))
	mustWrite(t, filepath.Join(project, ".devcontainer", "Dockerfile"), []byte("FROM alpine:3.22\nRUN apk add --no-cache git && printf built > /image-built\n"))
	mustWrite(t, filepath.Join(project, "message.txt"), []byte("initial"))
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"remote", "add", "origin", "https://github.com/company/fixture.git"}} {
		if _, err := a.gitAt(project, args...); err != nil {
			t.Fatal(err)
		}
	}
	s, err := a.newSession("container fixture", nil, []string{"company/fixture"})
	if err != nil {
		t.Fatal(err)
	}
	a.session = &s
	t.Cleanup(func() { a.sessionCommand([]string{"close", s.ID}) })
	t.Cleanup(func() {
		data, err := a.run("docker", []string{"ps", "-aq", "--filter", "label=commander.session=" + a.session.ID}, true, nil)
		if err == nil {
			for _, id := range strings.Fields(string(data)) {
				if len(id) >= 12 {
					a.run("docker", []string{"rm", "-f", id}, true, nil)
				}
			}
		}
		os.RemoveAll(project)
	})
	if err := a.up("container fixture"); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) error {
		_, err := a.run("devcontainer", append([]string{"exec", "--workspace-folder", project, "--id-label", "commander.session=" + a.session.ID}, args...), false, nil)
		return err
	}
	if err := command("sh", "-c", "test \"$(cat /image-built)\" = built && test \"$(cat message.txt)\" = initial"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(project, "message.txt"), []byte("edited"))
	if err := command("sh", "-c", "test \"$(cat message.txt)\" = edited"); err != nil {
		t.Fatal(err)
	}
	err = command("sh", "-c", "exit 37")
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 37 {
		t.Fatal("project exit status lost", err)
	}
}

func TestIntegrationOfficeDeviceCode(t *testing.T) {
	a := integrationApp(t)
	script := filepath.Join(t.TempDir(), "office.mjs")
	mustWrite(t, script, []byte(officeDeviceFixture))
	run := func(mode string) {
		cmd := exec.Command(filepath.Join(a.root, "tools", "node", "bin", "node"), script, mode)
		cmd.Env = a.environment("m365", map[string]string{"COMMANDER_M365_DIST": filepath.Join(a.packageDir("m365"), "node_modules", "@pnp", "cli-microsoft365", "dist")})
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("offline Office callback: %v\n%s", err, output)
		}
	}
	run("seed")
	if err := a.officeSettings(); err != nil {
		t.Fatal(err)
	}
	run("verify")
}

func TestIntegrationSelfUpdate(t *testing.T) {
	a := integrationApp(t)
	binary, err := os.ReadFile(filepath.Join(a.root, "commander"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%s  commander\n", hex.EncodeToString(sum[:]))
			return
		}
		if r.URL.Path == "/commander" {
			w.Write(binary)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	previous := releaseURL
	releaseURL = server.URL + "/"
	defer func() { releaseURL = previous }()
	if updated, err := a.update(); err != nil || updated {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(a.root, "commander"))
	if err != nil || !bytes.Equal(binary, after) {
		t.Fatal("update changed the verified executable", err)
	}
	if err = a.doctor(); err != nil {
		t.Fatal(err)
	}
}

const officeDeviceFixture = `
// Exercise the installed CLI callback offline, using only temporary settings.
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

const dist = process.env.COMMANDER_M365_DIST;
const load = relative => import(pathToFileURL(dist + '/' + relative).href);
const { cli } = await load('cli/cli.js');
const { default: auth } = await load('Auth.js');
const { browserUtil } = await load('utils/browserUtil.js');
const settings = cli.getConfig();
const response = {
  message: 'Open https://example.invalid/device and enter FIXTURE.',
  userCode: 'FIXTURE',
  verificationUri: 'https://example.invalid/device',
};
const messages = [];
const logger = { logToStderr: async message => messages.push(message) };
auth._clipboardy = { writeSync: () => { throw new Error('Fixture clipboard unavailable'); } };

if (process.argv[2] === 'seed') {
  const { interactivePreset } = await load('m365/commands/setupPresets.js');
  settings.set({ ...interactivePreset, disableTelemetry: true,
    clientId: 'fixture-app', tenantId: 'fixture-tenant' });
  browserUtil.open = async () => {};
  // The real interactive preset reproduces the failure reported by the user.
  await assert.rejects(auth.processDeviceCodeCallback(response, logger, false),
    /Fixture clipboard unavailable/);
} else {
  browserUtil.open = async () => { throw new Error('Unexpected desktop browser access'); };
  assert.equal(settings.get('copyDeviceCodeToClipboard'), false);
  assert.equal(settings.get('autoOpenLinksInBrowser'), false);
  assert.equal(settings.get('authType'), 'deviceCode');
  assert.equal(settings.get('clientId'), 'fixture-app');
  assert.equal(settings.get('tenantId'), 'fixture-tenant');
  // Use the real CLI logger. A stub logger would miss --output none hiding
  // the device code before the user ever gets a chance to sign in.
  const command = {
    getCommandName: () => 'login',
    action: nativeLogger => auth.processDeviceCodeCallback(response, nativeLogger, false),
  };
  if (process.argv[2] === 'login') {
    const output = process.argv[process.argv.indexOf('--output') + 1];
    await cli.executeCommand(command, { options: { output } });
  } else {
    cli.error = async message => messages.push(message);
    await cli.executeCommand(command, { options: { output: 'none' } });
    assert.equal(messages.length, 0);
    await cli.executeCommand(command, { options: { output: 'text' } });
    assert.ok(messages.some(message => message.includes('FIXTURE')));
  }
}
`
