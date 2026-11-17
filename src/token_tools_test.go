package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redirectServiceFixture(target string) *http.Client {
	u, _ := url.Parse(target)
	client := *serviceHTTP
	client.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.Host = r.URL.Host
		req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(req)
	})
	return &client
}

func useServiceFixture(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	old := serviceHTTP
	serviceHTTP = redirectServiceFixture(server.URL)
	t.Cleanup(func() { serviceHTTP = old; server.Close() })
}

func tokenFixture(service, user string) tokenLogin {
	c := tokenLogin{User: user, Identity: user, Token: "fixture-secret-" + user}
	if service == "confluence" {
		c.User = user + "@example.test"
		c.Site = "https://team.atlassian.net"
		c.API = c.Site + "/wiki"
		c.Identity = "account-" + user
	}
	return c
}
func saveTokenFixture(t *testing.T, a *app, service string, records ...tokenLogin) {
	t.Helper()
	store := map[string]tokenLogin{}
	for _, c := range records {
		store[c.member(service).selector()] = c
	}
	if err := a.writeTokenLogins(service, store); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCLIScopeIdentityAndStreams(t *testing.T) {
	for _, service := range []string{"dockerhub", "confluence"} {
		t.Run(service, func(t *testing.T) {
			a := testApp(t)
			selected, other := tokenFixture(service, "alice"), tokenFixture(service, "other")
			saveTokenFixture(t, a, service, selected, other)
			useSession(t, a, "work", selected.member(service))
			calls := []string{}
			useServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.URL.Path {
				case "/v2/auth/token":
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["identifier"] != "alice" || body["secret"] != selected.Token {
						t.Error("wrong Hub login")
					}
					fmt.Fprint(w, `{"access_token":"fixture-bearer"}`)
				case "/wiki/rest/api/user/current":
					if r.Header.Get("Authorization") != selected.authorization() {
						t.Error("wrong Confluence login")
					}
					fmt.Fprint(w, `{"accountId":"account-alice","type":"known"}`)
				default:
					want := "Bearer fixture-bearer"
					if service == "confluence" {
						want = selected.authorization()
					}
					if r.Header.Get("Authorization") != want {
						t.Error("wrong API authorization")
					}
					body, _ := io.ReadAll(r.Body)
					if r.Method != "PATCH" || string(body) != "{\"name\":\"with spaces\"}\n" || r.URL.Query().Get("q") != "a b" {
						t.Error("changed request", r.Method, string(body), r.URL.RawQuery)
					}
					fmt.Fprint(w, `{"ok":true}`)
				}
			})
			if err := a.command([]string{"exec", "--", service, "whoami"}); err != nil {
				t.Fatal(err)
			}
			if got := a.out.(*bytes.Buffer).String(); strings.Contains(got, "secret") || strings.Contains(got, "other") || !strings.Contains(got, selected.User) {
				t.Fatal("unsafe identity output", got)
			}
			a.out = &bytes.Buffer{}
			if err := a.command([]string{"exec", "--", service, "whoami", "--account", other.member(service).selector()}); err == nil || len(calls) != 1 {
				t.Fatal("used an unselected account", err, calls)
			}
			input := filepath.Join(t.TempDir(), "body.json")
			mustWrite(t, input, []byte("{\"name\":\"with spaces\"}\n"))
			path := "/v2/namespaces/alice/repositories/repo?q=a%20b"
			if service == "confluence" {
				path = "/api/v2/pages/123?q=a%20b"
			}
			if err := a.command([]string{"exec", "--", service, "api", path, "-X", "PATCH", "--input", input}); err != nil {
				t.Fatal(err)
			}
			if got := a.out.(*bytes.Buffer).String(); got != `{"ok":true}` {
				t.Fatal("polluted stdout", got)
			}
			if len(calls) != 3 {
				t.Fatal(calls)
			}
			a.out = &bytes.Buffer{}
			if err := a.describe("", true); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(a.out.(*bytes.Buffer).String(), other.User) {
				t.Fatal("context exposed another account")
			}
		})
	}
}

func TestTokenAPIBoundariesAndFailures(t *testing.T) {
	for _, service := range []string{"dockerhub", "confluence"} {
		for _, path := range []string{"https://evil.test/api", "//evil.test/api", "/api/v2/../auth", "/api/v2/%2e%2e/auth", "/api/v2/%252e%252e/auth", "/api/v2/a#secret", "/v2/auth/token", "/v2/namespaces/a/../../auth/token"} {
			if _, err := apiEndpoint(service, path); err == nil {
				t.Errorf("accepted %s %s", service, path)
			}
		}
	}
	for _, site := range []string{"http://team.atlassian.net", "https://team.atlassian.net.evil.test", "https://alice:secret@team.atlassian.net", "https://team.atlassian.net/wiki?token=foo", "https://team.atlassian.net:443", "https://team.atlassian.net/redirect"} {
		if _, err := cloudSite(site); err == nil {
			t.Error("accepted unsafe site", site)
		}
	}
	for _, status := range []int{401, 403, 429, 500, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := testApp(t)
			c := tokenFixture("confluence", "alice")
			saveTokenFixture(t, a, "confluence", c)
			useSession(t, a, "work", c.member("confluence"))
			calls := 0
			useServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://evil.test/collect")
				w.WriteHeader(status)
				fmt.Fprint(w, c.Token)
			})
			err := a.command([]string{"exec", "--", "confluence", "api", "/api/v2/pages"})
			if err == nil || strings.Contains(err.Error(), c.Token) || a.out.(*bytes.Buffer).Len() != 0 || calls != 1 {
				t.Fatal("unsafe failure", err, calls)
			}
		})
	}
}

func TestConfluenceIdentityChangeBlocksAction(t *testing.T) {
	a := testApp(t)
	c := tokenFixture("confluence", "alice")
	saveTokenFixture(t, a, "confluence", c)
	useSession(t, a, "work", c.member("confluence"))
	calls := 0
	useServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/wiki/rest/api/user/current" {
			t.Error("acted before verifying identity")
		}
		fmt.Fprint(w, `{"accountId":"different-user"}`)
	})
	err := a.command([]string{"exec", "--", "confluence", "api", "/api/v2/pages/123", "-X", "DELETE"})
	if err == nil || !strings.Contains(err.Error(), "identity changed") || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestTokenArchiveRelocationAndDeletion(t *testing.T) {
	a := testApp(t)
	members := []member{}
	for _, service := range []string{"dockerhub", "confluence"} {
		c := tokenFixture(service, "alice")
		saveTokenFixture(t, a, service, c, tokenFixture(service, "other"))
		members = append(members, c.member(service))
	}
	payload, err := a.exportPayload("work", members)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(payload)), "other") {
		t.Fatal("export included unselected credentials")
	}
	archive := filepath.Join(t.TempDir(), "profile.zip")
	if err = writeArchive(archive, payload, "fixture-password", false); err != nil {
		t.Fatal(err)
	}
	payload, err = readArchive(archive, "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	b := testApp(t)
	name, err := b.importPayload(payload, "")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved Commander")
	if err = os.Rename(b.root, moved); err != nil {
		t.Fatal(err)
	}
	b.root = moved
	s, err := b.findSession(name)
	if err != nil {
		t.Fatal(err)
	}
	b.session = &s
	for _, service := range []string{"dockerhub", "confluence"} {
		got, err := b.selectedToken(service, "")
		if err != nil || !reflect.DeepEqual(got, tokenFixture(service, "alice")) {
			t.Fatal("relocation lost login", err)
		}
		info, err := os.Stat(filepath.Join(b.root, ".secrets", tokenFile(service)))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("unsafe file permissions", err)
		}
		if err = b.deleteLogin(got.member(service)); err != nil {
			t.Fatal(err)
		}
		records, err := b.tokenLogins(service)
		if err != nil || len(records) != 0 {
			t.Fatal("login not removed", err)
		}
	}
}

func TestDockerConfigScopeAndCleanup(t *testing.T) {
	a := testApp(t)
	c, other := tokenFixture("dockerhub", "alice"), tokenFixture("dockerhub", "other")
	saveTokenFixture(t, a, "dockerhub", c, other)
	useSession(t, a, "work", c.member("dockerhub"))
	checks := 0
	useServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { checks++; fmt.Fprint(w, `{"access_token":"fixture"}`) })
	path, cleanup, err := a.dockerConfig("")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil || !bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Token)))) || bytes.Contains(data, []byte(other.Token)) || checks != 1 {
		t.Fatal("incorrect Docker scope", err)
	}
	cleanup()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("retained temporary credentials")
	}
	useSession(t, a, "both", c.member("dockerhub"), other.member("dockerhub"))
	if _, cleanup, err = a.dockerConfig(""); err == nil {
		cleanup()
		t.Fatal("silently selected an account")
	}
	path, cleanup, err = a.dockerConfig("alice")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	useSession(t, a, "empty")
	path, cleanup, err = a.dockerConfig("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, _ = os.ReadFile(filepath.Join(path, "config.json"))
	if bytes.Contains(data, []byte("auth\"")) {
		t.Fatal("empty session inherited a login")
	}
}

func TestTokenStorageValidationAndSymlinks(t *testing.T) {
	a := testApp(t)
	path := filepath.Join(a.root, ".secrets", tokenFile("confluence"))
	c := tokenFixture("confluence", "alice")
	c.API = "https://evil.test/wiki"
	mustWrite(t, path, encode(map[string]tokenLogin{c.member("confluence").selector(): c}))
	if _, err := a.tokenLogins("confluence"); !errors.Is(err, errFormat) {
		t.Fatal("accepted hostile API host", err)
	}
	os.Remove(path)
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
		t.Fatal(err)
	}
	if err := a.writeTokenLogins("confluence", map[string]tokenLogin{}); err == nil {
		t.Fatal("followed secret symlink")
	}
}

func TestInteractiveTokenLogins(t *testing.T) {
	for _, service := range []string{"dockerhub", "confluence"} {
		modes := []string{"valid", "rejected"}
		if service == "confluence" {
			modes = append(modes, "scoped")
		}
		for _, mode := range modes {
			t.Run(service+"/"+mode, func(t *testing.T) {
				a := testApp(t)
				addSession(t, a, "work")
				token := "fixture-hidden-token-" + strings.Repeat("x", 1500)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "rejected" {
						w.WriteHeader(401)
						fmt.Fprint(w, token)
						return
					}
					switch r.Host + r.URL.Path {
					case "hub.docker.com/v2/auth/token":
						var body map[string]string
						if json.NewDecoder(r.Body).Decode(&body) != nil || body["identifier"] != "alice" || body["secret"] != token {
							t.Error("incorrect Hub input")
						}
						fmt.Fprint(w, `{"access_token":"fixture-bearer"}`)
					case "team.atlassian.net/wiki/rest/api/user/current":
						user, secret, ok := r.BasicAuth()
						if !ok || user != "alice@example.test" || secret != token {
							t.Error("incorrect Confluence input")
						}
						if mode == "scoped" {
							w.WriteHeader(401)
							return
						}
						fmt.Fprint(w, `{"accountId":"account-alice","type":"known"}`)
					case "team.atlassian.net/_edge/tenant_info":
						if r.Header.Get("Authorization") != "" {
							t.Error("discovery received a token")
						}
						fmt.Fprint(w, `{"cloudId":"fixture-cloud"}`)
					case "api.atlassian.com/ex/confluence/fixture-cloud/wiki/rest/api/user/current":
						_, secret, ok := r.BasicAuth()
						if !ok || secret != token {
							t.Error("scoped token missing")
						}
						fmt.Fprint(w, `{"accountId":"account-alice","type":"known"}`)
					default:
						t.Error("unexpected service request", r.Host, r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				t.Setenv("COMMANDER_UI_SERVICE_URL", server.URL)
				s := openUI(t, a.root)
				s.wait(t, "[q]uit")
				s.send(t, "e")
				s.wait(t, "+ add account")
				s.send(t, "a")
				s.wait(t, "google cloud")
				choice := strings.Repeat("\x1b[B", 5) + "\r"
				if service == "confluence" {
					choice = strings.Repeat("\x1b[B", 6) + "\r"
				}
				s.send(t, choice)
				if service == "confluence" {
					s.wait(t, "confluence url:")
					s.send(t, "https://team.atlassian.net/wiki\n")
					s.wait(t, "atlassian email:")
					s.send(t, "alice@example.test\n")
				} else {
					s.wait(t, "dockerhub username:")
					s.send(t, "alice\n")
				}
				s.wait(t, "api token:")
				s.send(t, token+"\n")
				if mode == "rejected" {
					s.wait(t, "no accounts yet")
				} else {
					s.wait(t, "[x] alice")
				}
				s.send(t, "\x1b")
				s.wait(t, "[q]uit")
				s.send(t, "q")
				if err := s.command.Wait(); err != nil {
					t.Fatal(err)
				}
				for chunk := range s.chunks {
					s.output += chunk
				}
				if strings.Contains(s.output, token) || strings.Contains(s.output, "fixture-bearer") {
					t.Fatal("terminal exposed token")
				}
				members, _ := a.logins()
				if mode == "rejected" {
					if len(members) != 0 {
						t.Fatal("saved rejected login")
					}
					if _, err := os.Stat(filepath.Join(a.root, ".secrets", tokenFile(service))); !os.IsNotExist(err) {
						t.Fatal("persisted rejected token")
					}
				} else {
					if len(members) != 1 || members[0].Service != service {
						t.Fatal("login not selected", members)
					}
					records, err := a.tokenLogins(service)
					if err != nil {
						t.Fatal(err)
					}
					c := records[members[0].selector()]
					if c.Token != token {
						t.Fatal("hidden token not preserved")
					}
					if mode == "scoped" && c.API != "https://api.atlassian.com/ex/confluence/fixture-cloud/wiki" {
						t.Fatal("wrong scoped endpoint")
					}
				}
			})
		}
	}
}

func TestDockerHubNativeCommandPreservesArgumentsAndExitCode(t *testing.T) {
	a := testApp(t)
	c := tokenFixture("dockerhub", "alice")
	saveTokenFixture(t, a, "dockerhub", c)
	useSession(t, a, "work", c.member("dockerhub"))
	preparedTools(t, a)
	executable := filepath.Join(a.root, "tools", "docker", "docker")
	mustWrite(t, executable, []byte("#!/bin/sh\ntest -f \"$DOCKER_CONFIG/config.json\" || exit 99\nprintf '%s\\n' \"$@\"\nprintf 'native error' >&2\nexit 37\n"))
	if err := os.Chmod(executable, 0755); err != nil {
		t.Fatal(err)
	}
	useServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"access_token":"fixture"}`) })
	err := a.command([]string{"exec", "--", "dockerhub", "docker", "--account", "alice", "--", "run", "image", "a b", "$(literal)"})
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 37 {
		t.Fatal("lost native exit code", err)
	}
	if a.out.(*bytes.Buffer).String() != "run\nimage\na b\n$(literal)\n" || a.errOut.(*bytes.Buffer).String() != "native error" {
		t.Fatal("changed native streams")
	}
	matches, _ := filepath.Glob(filepath.Join(a.root, ".secrets", ".docker-*"))
	if len(matches) != 0 {
		t.Fatal("retained temporary credentials after failure")
	}
}
