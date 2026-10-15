//go:build !unix

package main

import (
	"net"
	"os"
)

// listenUnixPrivate has no umask to lean on here, so it tightens the mode
// after binding, best effort: Windows ignores Unix permission bits, and
// there the parent directory's ACL is what protects the socket.
func listenUnixPrivate(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, nil
}
