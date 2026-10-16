//go:build !linux

package daemon

import "net"

// Elsewhere the daemon doesn't look up who is at the other end.
func peerExecutable(net.Conn) string { return "" }
