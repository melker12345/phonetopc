//go:build !unix

package main

import (
	"net"
	"os"
)

// listenUnixMode has no umask to lean on here, so it sets the mode after
// binding, best effort: Windows ignores Unix permission bits, and there the
// parent directory's ACL is what protects the socket.
func listenUnixMode(path string, perm os.FileMode) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, perm)
	return ln, nil
}
