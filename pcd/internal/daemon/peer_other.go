//go:build !linux

package daemon

import "net"

// Elsewhere the peer isn't looked up: TCP requests always need the token,
// and the Unix socket's file mode is all that guards it.
func connPeer(net.Conn) (Peer, bool) { return Peer{}, false }
