package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The bridge exposes Git credentials and two product Make targets to trusted
// containers. It never mounts account stores or installs an agent in an image.
type bridgeEvent struct {
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
}

type bridgeWriter struct {
	mu      *sync.Mutex
	encoder *json.Encoder
	flush   http.Flusher
	stream  string
}

func (w bridgeWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.encoder.Encode(bridgeEvent{Stream: w.stream, Data: data})
	if err != nil {
		return 0, err
	}
	w.flush.Flush()
	return len(data), nil
}

func socketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func bridgeClient(args []string) int {
	if len(args) == 0 {
		return 2
	}
	endpoint := ""
	var body io.Reader
	switch args[0] {
	case "git":
		if len(args) != 2 && len(args) != 3 {
			return 2
		}
		if args[len(args)-1] != "get" {
			return 0
		}
		endpoint = "/git"
		if len(args) == 3 {
			if !containerID.MatchString(args[1]) {
				return 2
			}
			endpoint += "/" + args[1]
		}
		body = io.LimitReader(os.Stdin, 32768)
	case "make":
		if len(args) != 3 || !containerID.MatchString(args[1]) || !contains([]string{"dev", "prd"}, args[2]) {
			return 2
		}
		endpoint = "/make/" + args[1] + "/" + args[2]
	default:
		return 2
	}
	response, err := socketClient("/run/commander/socket").Post("http://commander"+endpoint, "application/octet-stream", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Commander connection is unavailable; reopen this container with commander up")
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "Commander rejected this request; check the session selection")
		return 1
	}
	if args[0] == "git" {
		if _, err = io.Copy(os.Stdout, response.Body); err != nil {
			return 1
		}
		return 0
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event bridgeEvent
		if err = decoder.Decode(&event); err != nil {
			fmt.Fprintln(os.Stderr, "Commander deployment connection ended before completion")
			return 1
		}
		if event.Exit != nil {
			return *event.Exit
		}
		writer := io.Writer(os.Stdout)
		if event.Stream == "err" {
			writer = os.Stderr
		}
		if _, err = writer.Write(event.Data); err != nil {
			return 1
		}
	}
}

func (a *app) bridgeDirectory() (string, error) {
	if a.session == nil {
		return "", errNoSession
	}
	path := filepath.Join(a.root, ".sessions", a.session.ID, "bridge")
	return path, noSymlinks(a.root, path)
}

func bridgeHealthy(path string) bool {
	client := socketClient(filepath.Join(path, "socket"))
	client.Timeout = time.Second
	response, err := client.Get("http://commander/health")
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == 200
}

func (a *app) startBridge() (string, error) {
	path, err := a.bridgeDirectory()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(path, 0755); err != nil {
		return "", err
	}
	unlock, err := fileLock(filepath.Join(path, ".start.lock"), false, true)
	if err != nil {
		return "", err
	}
	defer unlock()
	if bridgeHealthy(path) {
		return path, nil
	}
	binary := filepath.Join(a.root, "commander")
	// One linked executable provides the small client without adding curl,
	// language runtimes or agent packages to any project image.
	target := filepath.Join(path, "commander")
	os.Remove(target)
	if err = os.Link(binary, target); err != nil {
		data, readErr := os.ReadFile(binary)
		if readErr != nil {
			return "", readErr
		}
		if err = atomicWrite(target, data, 0755); err != nil {
			return "", err
		}
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer null.Close()
	cmd := exec.Command(binary, "--session", a.session.ID, "bridge-serve")
	cmd.Env = a.environment("shell", nil)
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return "", err
	}
	cmd.Process.Release()
	for i := 0; i < 50; i++ {
		if bridgeHealthy(path) {
			return path, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("Commander container connection did not start")
}

func (a *app) serveBridge() error {
	path, err := a.bridgeDirectory()
	if err != nil {
		return err
	}
	unlock, err := fileLock(filepath.Join(path, ".server.lock"), false, false)
	if err != nil {
		return err
	}
	defer unlock()
	socket := filepath.Join(path, "socket")
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	// The parent session directory is private on the host. Inside a trusted
	// container its selected session is available to the configured remote user.
	if err = os.Chmod(socket, 0666); err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/stop" {
			w.WriteHeader(200)
			go server.Close()
			return
		}
		a.bridgeRequest(w, r)
	})
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if _, err := a.readSession(a.session.ID); err != nil {
					server.Close()
					return
				}
			}
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *app) bridgeRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	current, err := a.readSession(a.session.ID)
	if err != nil {
		http.Error(w, "session unavailable", 403)
		return
	}
	scoped := *a
	scoped.session = &current
	if r.URL.Path == "/git" || strings.HasPrefix(r.URL.Path, "/git/") {
		if r.URL.Path != "/git" {
			project, err := scoped.readContainerProject(strings.TrimPrefix(r.URL.Path, "/git/"))
			if err != nil {
				http.Error(w, "project unavailable", 403)
				return
			}
			scoped.account = project.Account
		}
		var response bytes.Buffer
		err = scoped.writeGitCredential(http.MaxBytesReader(w, r.Body, 32768), &response)
		if err != nil {
			http.Error(w, "Git selection unavailable", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Write(response.Bytes())
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "make" || !containerID.MatchString(parts[1]) || !contains([]string{"dev", "prd"}, parts[2]) {
		http.NotFound(w, r)
		return
	}
	project, err := scoped.readContainerProject(parts[1])
	if err != nil {
		http.Error(w, "project unavailable", 403)
		return
	}
	origin, err := scoped.gitAt(project.Project, "config", "remote.origin.url")
	if err != nil || !current.allows(repositoryName(origin)) {
		http.Error(w, "project outside selection", 403)
		return
	}
	unlock, err := scoped.sessionLock(current.ID, true, true)
	if err != nil {
		http.Error(w, "session unavailable", 403)
		return
	}
	defer unlock()
	if err = scoped.sessionWrappers(); err != nil {
		http.Error(w, "session unavailable", 500)
		return
	}
	command, err := scoped.lookupTool("make")
	if err != nil {
		http.Error(w, "make unavailable on host", 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	mu := &sync.Mutex{}
	flush := w.(http.Flusher)
	cmd := exec.CommandContext(r.Context(), command, "--no-print-directory", "-C", project.Project, parts[2])
	cmd.Env = scoped.environment("shell", nil)
	cmd.Stdout = bridgeWriter{mu, encoder, flush, "out"}
	cmd.Stderr = bridgeWriter{mu, encoder, flush, "err"}
	code := 0
	if err = cmd.Run(); err != nil {
		code = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
			if code < 0 {
				code = 1
			}
		}
	}
	encoder.Encode(bridgeEvent{Exit: &code})
	flush.Flush()
}

func (a *app) stopBridge() error {
	path, err := a.bridgeDirectory()
	if err != nil {
		return err
	}
	if !bridgeHealthy(path) {
		return nil
	}
	client := socketClient(filepath.Join(path, "socket"))
	client.Timeout = 2 * time.Second
	response, err := client.Post("http://commander/stop", "text/plain", nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}
