//go:build unix

package main

import (
	"net"
	"syscall"
)

// listenUnixPrivate creates the socket under a 0177 umask so it is never
// reachable by other users, not even between bind and a later chmod.
func listenUnixPrivate(path string) (net.Listener, error) {
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
