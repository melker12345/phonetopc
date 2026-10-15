package daemon

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// peerIsSameUser finds the other end of a loopback TCP connection in
// /proc/net/tcp{,6}, where the kernel lists every socket with its owner.
func peerIsSameUser(c net.Conn) bool {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || !remote.IP.IsLoopback() {
		return false
	}
	// The peer's socket is the one whose own address is our remote one.
	peer, us := remote.AddrPort(), local.AddrPort()
	for _, name := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		uid, found := socketOwner(f, peer, us)
		f.Close()
		if found {
			return uid == os.Getuid()
		}
	}
	return false
}

// socketOwner returns the uid of the socket from local to remote in a
// /proc/net/tcp listing.
func socketOwner(r io.Reader, local, remote netip.AddrPort) (int, bool) {
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		l, ok1 := parseProcAddr(f[1])
		r, ok2 := parseProcAddr(f[2])
		if !ok1 || !ok2 || !sameAddrPort(l, local) || !sameAddrPort(r, remote) {
			continue
		}
		uid, err := strconv.Atoi(f[7])
		return uid, err == nil
	}
	return 0, false
}

func sameAddrPort(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}

// parseProcAddr parses "0100007F:1F47": the address as 32-bit words in host
// byte order, then the port.
func parseProcAddr(s string) (netip.AddrPort, bool) {
	addrHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr, uint16(port)), true
}
