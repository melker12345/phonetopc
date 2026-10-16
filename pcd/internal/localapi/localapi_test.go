//go:build unix

package localapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ptc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSocket(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	missing := filepath.Join(dir, "missing")

	for _, tc := range []struct {
		mode, socket, want string
		err                bool
	}{
		{"", sock, sock, false},
		{"", missing, "", false},
		{"", file, "", false}, // not a socket
		{"user", sock, "", false},
		{"system", missing, missing, false},
		{"", "", "", false}, // SystemSocket, absent here unless installed
		{"both", sock, "", true},
	} {
		if tc.socket == "" {
			if _, err := os.Stat(SystemSocket); err == nil {
				continue
			}
		}
		env := map[string]string{"PHONETOPC_MODE": tc.mode, "PHONETOPC_SOCKET": tc.socket}
		got, err := Socket(func(k string) string { return env[k] })
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("mode %q socket %q: %q %v, want %q", tc.mode, tc.socket, got, err, tc.want)
		}
	}
}

func TestErrors(t *testing.T) {
	dir := shortDir(t)
	ctx := context.Background()
	if err := New(filepath.Join(dir, "missing")).Get(ctx, "/v1/status", nil); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("no daemon: %v", err)
	}

	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"forbidden","message":"only root may do this"}}`))
	}))
	err = New(sock).Do(ctx, "DELETE", "/v1/devices/x", nil, nil)
	if !IsCode(err, "forbidden") || err.Error() != "only root may do this" {
		t.Errorf("API error: %v", err)
	}
	if _, err := New(sock).WebSocket(ctx, "/v1/pairing"); !IsCode(err, "forbidden") {
		t.Errorf("WebSocket refused: %v", err)
	}

	ln2, err := net.Listen("unix", filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go http.Serve(ln2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(`{"match":42,"program":"phonetopc"}` + "\n"))
		if r.URL.Path == "/v1/approvals" {
			w.Write([]byte(`{"approved":true,"why":"approved","proof":{"match":42}}` + "\n"))
		} else {
			w.Write([]byte(`{"error":{"code":"no_answer","message":"no answer"}}` + "\n"))
		}
	}))
	var shown []string
	show := func(m int, prog string) { shown = append(shown, fmt.Sprintf("%d %s", m, prog)) }
	var res struct{ Approved bool }
	if err := New(ln2.Addr().String()).Ask(ctx, "/v1/approvals", struct{}{}, show, &res); err != nil || !res.Approved {
		t.Errorf("ask: %+v %v", res, err)
	}
	if err := New(ln2.Addr().String()).Ask(ctx, "/v1/decryptions", struct{}{}, show, &res); !IsCode(err, "no_answer") {
		t.Errorf("error after the match: %v", err)
	}
	if strings.Join(shown, ",") != "42 phonetopc,42 phonetopc" {
		t.Errorf("shown %q", shown)
	}

	if os.Getuid() != 0 {
		os.Chmod(sock, 0)
		if err := New(sock).Get(ctx, "/v1/status", nil); err == nil || !strings.Contains(err.Error(), "phonetopc-users") {
			t.Errorf("not allowed: %v", err)
		}
	}
}
