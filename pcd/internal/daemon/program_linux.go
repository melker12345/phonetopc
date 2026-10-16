package daemon

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// peerExecutable returns the path of the executable at the other end of c,
// or "" if it can't be found: the peer of a Unix socket by SO_PEERCRED, of
// a loopback TCP connection by its socket's inode in /proc/net/tcp{,6} and
// the process that has it open. Only processes whose /proc entries the
// daemon may read are found.
func peerExecutable(c net.Conn) string {
	pid := 0
	switch c := c.(type) {
	case *net.UnixConn:
		pid = unixPeerPID(c)
	case *net.TCPConn:
		pid = tcpPeerPID(c)
	}
	if pid <= 0 {
		return ""
	}
	if exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return exe
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(comm), "\n")
}

func unixPeerPID(c *net.UnixConn) int {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0
	}
	var cred *syscall.Ucred
	raw.Control(func(fd uintptr) {
		cred, err = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || cred == nil {
		return 0
	}
	return int(cred.Pid)
}

func tcpPeerPID(c *net.TCPConn) int {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || !remote.IP.IsLoopback() {
		return 0
	}
	for _, name := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		inode, found := socketInode(f, remote.AddrPort(), local.AddrPort())
		f.Close()
		if found {
			return socketPID(inode)
		}
	}
	return 0
}

// socketInode returns the inode of the socket from local to remote in a
// /proc/net/tcp listing.
func socketInode(r io.Reader, local, remote netip.AddrPort) (string, bool) {
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		l, ok1 := parseProcAddr(f[1])
		r, ok2 := parseProcAddr(f[2])
		if ok1 && ok2 && sameAddrPort(l, local) && sameAddrPort(r, remote) {
			return f[9], f[9] != "0"
		}
	}
	return "", false
}

// socketPID returns a process that has the socket with inode open.
func socketPID(inode string) int {
	want := "socket:[" + inode + "]"
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if l, err := os.Readlink(fd); err == nil && l == want {
			pid, _ := strconv.Atoi(strings.Split(fd, "/")[2])
			return pid
		}
	}
	return 0
}
