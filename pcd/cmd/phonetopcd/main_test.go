package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shortDir returns a directory whose paths fit in a Unix socket address.
func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ptc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type daemonRun struct {
	socket, tcp string
	stderr      syncBuffer
	done        chan int
}

// startRun runs phonetopcd against a dev-mode mock.
func startRun(t *testing.T, rt string, args ...string) *daemonRun {
	t.Helper()
	srv := mockphone.New(mockphone.Options{TapEvery: -1, NFCEvery: -1, UserEvery: -1})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); hs.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	r := &daemonRun{done: make(chan int, 1)}
	ready := make(chan struct{})
	env := map[string]string{"XDG_RUNTIME_DIR": rt, "HOME": rt}
	args = append([]string{"--insecure-dev", "--addr", strings.TrimPrefix(hs.URL, "http://"), "--listen", "127.0.0.1:0"}, args...)
	go func() {
		r.done <- run(ctx, args, &r.stderr, func(k string) string { return env[k] }, func(socket, tcp string) {
			r.socket, r.tcp = socket, tcp
			close(ready)
		})
	}()
	t.Cleanup(func() { cancel(); <-r.done })
	select {
	case <-ready:
	case code := <-r.done:
		r.done <- code
		t.Fatalf("exited %d: %s", code, r.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	return r
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

func status(t *testing.T, c *http.Client, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(res.Body)
	json.Unmarshal(b, &m)
	return res.StatusCode, m
}

func TestServesSocketAndTCP(t *testing.T) {
	rt := shortDir(t)
	r := startRun(t, rt)
	if r.socket != filepath.Join(rt, "phonetopc.sock") {
		t.Fatalf("socket %q", r.socket)
	}
	fi, err := os.Stat(r.socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("socket mode %v", fi.Mode().Perm())
	}

	uc := unixClient(r.socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, m := status(t, uc, "http://phonetopc/v1/status", "")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		if m["connected"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not connected: %v", m)
		}
		time.Sleep(20 * time.Millisecond)
	}

	tokPath := filepath.Join(rt, "phonetopc.token")
	tb, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(tokPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v", fi.Mode().Perm())
	}
	token := strings.TrimSpace(string(tb))
	sameUser := 401
	if runtime.GOOS == "linux" {
		sameUser = 200
	}
	if code, _ := status(t, http.DefaultClient, "http://"+r.tcp+"/v1/status", ""); code != sameUser {
		t.Errorf("this user, no token: %d, want %d", code, sameUser)
	}
	req, _ := http.NewRequest("GET", "http://"+r.tcp+"/v1/status", nil)
	req.Header.Set("Origin", "https://example.com")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 401 {
		t.Errorf("web page, no token: %v %v", res.StatusCode, err)
	} else {
		res.Body.Close()
	}
	if code, m := status(t, http.DefaultClient, "http://"+r.tcp+"/v1/status", token); code != 200 || m["connected"] != true {
		t.Errorf("with token: %d %v", code, m)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	rt := shortDir(t)
	startRun(t, rt)
	var stderr bytes.Buffer
	env := map[string]string{"XDG_RUNTIME_DIR": rt}
	code := run(context.Background(), []string{"--insecure-dev", "--listen", "off"}, &stderr, func(k string) string { return env[k] }, nil)
	if code != 1 || !strings.Contains(stderr.String(), "already serving") {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
}

func TestStaleSocketReplaced(t *testing.T) {
	rt := shortDir(t)
	path := filepath.Join(rt, "phonetopc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the file stays, nobody answers: a daemon that died
	startRun(t, rt)
}

func TestTokenKeptAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	a, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadToken(path)
	if err != nil || a != b {
		t.Fatalf("%q then %q (%v)", a, b, err)
	}
	os.Chmod(path, 0o644)
	if _, err := loadToken(path); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
}

func TestUsageErrors(t *testing.T) {
	rt := shortDir(t)
	env := map[string]string{"XDG_RUNTIME_DIR": rt, "HOME": rt}
	getenv := func(k string) string { return env[k] }
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--nope"}, 2, "Usage"},
		{[]string{"extra"}, 2, "unexpected argument"},
		{[]string{"--socket", "off", "--listen", "off"}, 2, "nothing to serve on"},
		{[]string{"--insecure-dev", "--socket", "off", "--listen", "0.0.0.0:8767"}, 1, "only loopback"},
		{nil, 1, "no paired phone"},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), tc.args, &stderr, getenv, nil); code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: code %d: %s", tc.args, code, stderr.String())
		}
	}
}
