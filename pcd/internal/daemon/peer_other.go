//go:build !linux

package daemon

import "net"

// Elsewhere there is no cheap way to find a TCP peer's user, so TCP
// requests always need the token.
func peerIsSameUser(net.Conn) bool { return false }
