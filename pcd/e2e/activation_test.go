//go:build unix

package e2e

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The way systemd starts phonetopcd on the first connection: it already
// holds the sockets, a request is waiting on one, and phonetopcd gets them
// as fds 3 and 4 with LISTEN_PID set to its own pid.
func TestSocketActivationServesTheWaitingRequest(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	mockAddr := freeAddr(t)
	startMock(t, env, mockAddr)

	unixLn, err := net.Listen("unix", env.socket())
	if err != nil {
		t.Fatal(err)
	}
	unixLn.(*net.UnixListener).SetUnlinkOnClose(false)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	for _, ln := range []interface{ File() (*os.File, error) }{unixLn.(*net.UnixListener), tcpLn.(*net.TCPListener)} {
		f, err := ln.File()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}

	waiting := make(chan response, 1)
	go func() {
		r, err := tryGet(unixClient(env.socket()), "http://phonetopc/v1/sensors/battery", "")
		if err != nil {
			t.Error(err)
		}
		waiting <- r
	}()

	p := &proc{name: "phonetopcd", done: make(chan struct{})}
	p.cmd = exec.Command("/bin/sh", "-c", `LISTEN_PID=$$ exec "$0" --insecure-dev --addr "$1"`, filepath.Join(bin, "phonetopcd"), mockAddr)
	p.cmd.Env = append(env.vars, "LISTEN_FDS=2")
	p.cmd.ExtraFiles = files
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		f.Close()
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		p.kill()
		if t.Failed() {
			t.Logf("phonetopcd:\n%s", p.out.String())
		}
	})
	unixLn.Close()
	tcpLn.Close()

	if r := <-waiting; r.status != 200 || r.body["source"] != "battery" {
		t.Fatalf("the request that started it: %d %s", r.status, r.raw)
	}
	if r := do(t, http.DefaultClient, "GET", "http://"+tcpLn.Addr().String()+"/v1/status", "", ""); r.status != sameUserStatus() {
		t.Errorf("tcp: %d %s", r.status, r.raw)
	}
	p.cmd.Process.Signal(os.Interrupt)
	if code := p.wait(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(env.socket()); err != nil {
		t.Errorf("the socket belongs to systemd and must stay: %v", err)
	}
}
