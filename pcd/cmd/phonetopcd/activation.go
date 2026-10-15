package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// activated returns the sockets systemd passed in when it started us on the
// first connection (sd_listen_fds(3)): LISTEN_FDS of them from fd 3, meant
// for us if LISTEN_PID is our pid. The variables are unset so that nothing
// we start inherits them.
func activated(getenv func(string) string) ([]net.Listener, error) {
	pid, _ := strconv.Atoi(getenv("LISTEN_PID"))
	n, _ := strconv.Atoi(getenv("LISTEN_FDS"))
	if pid != os.Getpid() || n <= 0 {
		return nil, nil
	}
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(k)
	}
	fds := make([]uintptr, n)
	for i := range fds {
		fds[i] = uintptr(3 + i)
	}
	return fileListeners(fds)
}

func fileListeners(fds []uintptr) ([]net.Listener, error) {
	var lns []net.Listener
	for _, fd := range fds {
		f := os.NewFile(fd, "listen-fd-"+strconv.Itoa(int(fd)))
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			for _, ln := range lns {
				ln.Close()
			}
			return nil, fmt.Errorf("socket passed in as fd %d: %w", fd, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}
