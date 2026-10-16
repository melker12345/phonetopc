//go:build linux

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSystemMode: the system service starts before any phone is paired
// (pairing goes through it), shares its socket with a group, and has no
// token: only allowed users get in, and no web page.
func TestSystemMode(t *testing.T) {
	rt := shortDir(t)
	sock := filepath.Join(rt, "run", "phonetopc.sock")
	stateDir := filepath.Join(t.TempDir(), "state")
	env := map[string]string{"XDG_RUNTIME_DIR": rt, "HOME": rt, "STATE_DIRECTORY": stateDir}
	gid := os.Getgid()

	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuffer
	ready := make(chan [2]string, 1)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--system", "--socket", sock, "--group", strconv.Itoa(gid), "--listen", "127.0.0.1:0", "--no-mdns"},
			&stderr, func(k string) string { return env[k] }, func(s, tcp string) { ready <- [2]string{s, tcp} })
	}()
	t.Cleanup(func() { cancel(); <-done })
	var at [2]string
	select {
	case at = <-ready:
	case code := <-done:
		t.Fatalf("exited %d: %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o660 || int(fi.Sys().(*syscall.Stat_t).Gid) != gid {
		t.Errorf("socket mode %v group %d", fi.Mode().Perm(), fi.Sys().(*syscall.Stat_t).Gid)
	}
	code, m := status(t, unixClient(sock), "http://phonetopc/v1/status", "")
	if code != 200 || m["connected"] != false || !strings.Contains(m["error"].(string), "sudo phonetopc pair") {
		t.Errorf("status %d %v", code, m)
	}
	if code, _ := status(t, http.DefaultClient, "http://"+at[1]+"/v1/status", ""); code != 200 {
		t.Errorf("this user over TCP: %d", code)
	}
	req, _ := http.NewRequest("GET", "http://"+at[1]+"/v1/status", nil)
	req.Header.Set("Origin", "https://example.com")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 401 {
		t.Errorf("web page: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	if _, err := os.Stat(filepath.Join(rt, "phonetopc.token")); !os.IsNotExist(err) {
		t.Errorf("a token file: %v", err)
	}
	if !strings.Contains(stderr.String(), "serving on http://"+at[1]+"\n") {
		t.Errorf("log: %s", stderr.String())
	}
}

func TestSystemDefaults(t *testing.T) {
	c := config{system: true}
	c.defaults(func(k string) string { return map[string]string{"STATE_DIRECTORY": "/var/lib/phonetopc:/x"}[k] })
	if c.storeDir != SystemStore || c.socket != SystemSocket || c.group != SystemGroup || c.tokenFile != "" {
		t.Errorf("%+v", c)
	}
}
