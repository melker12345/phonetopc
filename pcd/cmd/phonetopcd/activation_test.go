//go:build unix

package main

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestActivationIgnoresOtherProcessesSockets(t *testing.T) {
	env := map[string]string{"LISTEN_PID": "1", "LISTEN_FDS": "2"}
	lns, err := activated(func(k string) string { return env[k] })
	if err != nil || lns != nil {
		t.Fatalf("%v %v", lns, err)
	}
}

func TestFileListenersKeepsTheSockets(t *testing.T) {
	ul, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	lns, err := fileListeners([]uintptr{dupFd(t, ul.(*net.UnixListener)), dupFd(t, tl.(*net.TCPListener))})
	if err != nil {
		t.Fatal(err)
	}
	defer lns[0].Close()
	defer lns[1].Close()
	if lns[0].Addr().Network() != "unix" || lns[1].Addr().String() != tl.Addr().String() {
		t.Fatalf("got %v %v", lns[0].Addr(), lns[1].Addr())
	}

	f, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileListeners([]uintptr{uintptr(fd)}); err == nil {
		t.Fatal("a file that isn't a socket was accepted")
	}
}

// dupFd returns a descriptor of its own for what f has open, for
// fileListeners to take over.
func dupFd(t *testing.T, f interface{ File() (*os.File, error) }) uintptr {
	t.Helper()
	file, err := f.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return uintptr(fd)
}
