package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/melker12345/phonetopc/pcd/internal/pairing"
)

// openPTY returns the two ends of a new pseudo-terminal.
func openPTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlocking the pty: %v", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("pty number: %v", err)
	}
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the pty: %v", err)
	}
	t.Cleanup(func() { tty.Close() })
	return master, tty
}

// TestPrompterFlushesUnfinishedLine: a "y" typed before the question, but
// not yet ended with Enter, doesn't turn the user's Enter into a yes.
func TestPrompterFlushesUnfinishedLine(t *testing.T) {
	master, tty := openPTY(t)
	if !isTerminal(tty) {
		t.Fatal("the pty is not a terminal")
	}
	var out syncBuffer
	p := newTermPrompter(tty, &out, false, true)
	master.WriteString("y")
	time.Sleep(50 * time.Millisecond)

	got := make(chan bool, 1)
	go func() {
		ok, _ := p.ConfirmQR(context.Background(), pairing.PhoneInfo{ID: "635f3a01db1859bc", Name: "Pixel"}, nil)
		got <- ok
	}()
	waitFor(t, &out, "[y/N]")
	master.WriteString("\n")
	select {
	case ok := <-got:
		if ok {
			t.Error("the early y answered the question")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
}
