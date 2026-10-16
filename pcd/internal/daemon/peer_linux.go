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
	"unsafe"

	"golang.org/x/sys/unix"
)

// connPeer finds who is at the other end of a local connection: for a Unix
// socket the kernel says (SO_PEERCRED, SO_PEERGROUPS); for loopback TCP,
// /proc/net/tcp{,6} lists every socket with its owner.
func connPeer(c net.Conn) (Peer, bool) {
	if uc, ok := c.(*net.UnixConn); ok {
		return unixPeer(uc)
	}
	uid, ok := tcpPeerUID(c)
	return Peer{UID: uid}, ok
}

func unixPeer(c *net.UnixConn) (Peer, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, false
	}
	var (
		cred          *unix.Ucred
		credErr, gErr error
		groups        []int
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		groups, gErr = peerGroups(int(fd))
	}); err != nil || credErr != nil {
		return Peer{}, false
	}
	p := Peer{UID: int(cred.Uid)}
	if gErr == nil {
		p.Groups = append(groups, int(cred.Gid))
	}
	return p, true
}

// peerGroups returns the supplementary groups the peer had when it
// connected (SO_PEERGROUPS, Linux 4.13).
func peerGroups(fd int) ([]int, error) {
	buf := make([]uint32, 32)
	for {
		size := uint32(len(buf) * 4)
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_SOCKET, unix.SO_PEERGROUPS,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
		if errno == unix.ERANGE && int(size/4) > len(buf) {
			buf = make([]uint32, size/4)
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		groups := make([]int, 0, size/4)
		for _, g := range buf[:size/4] {
			groups = append(groups, int(g))
		}
		return groups, nil
	}
}

func tcpPeerUID(c net.Conn) (int, bool) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || !remote.IP.IsLoopback() {
		return 0, false
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
			return uid, true
		}
	}
	return 0, false
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
