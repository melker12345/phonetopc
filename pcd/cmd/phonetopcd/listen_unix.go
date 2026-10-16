//go:build unix

package main

import (
	"net"
	"os"
	"syscall"
)

// listenUnixMode creates the socket under a umask that leaves it with mode
// perm, so it is never reachable by others, not even between bind and a
// later chmod.
func listenUnixMode(path string, perm os.FileMode) (net.Listener, error) {
	old := syscall.Umask(int(0o777 &^ perm))
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
