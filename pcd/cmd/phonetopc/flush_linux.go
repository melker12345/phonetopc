package main

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// flushInput discards what was typed on terminal r but not read yet.
func flushInput(r io.Reader) {
	f, ok := r.(*os.File)
	if !ok {
		return
	}
	if rc, err := f.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH) })
	}
}
