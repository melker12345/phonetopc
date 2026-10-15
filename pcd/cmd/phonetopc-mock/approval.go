package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

const approvalModes = "auto, deny, cancel, timeout, lockout, ask, tamper or off"

// st holds the approval key; in is read only for "ask".
func approvalOptions(opts *mockphone.Options, mode string, delay time.Duration, st *mockphone.State, in io.Reader, out io.Writer) error {
	if mode == "off" {
		return nil
	}
	if _, err := st.ApprovalKey(); err != nil { // create it now, so errors show at start
		return err
	}
	opts.ApprovalKey = func() *ecdsa.PrivateKey {
		k, err := st.ApprovalKey() // reread: `approval-key --rotate` applies to new sessions
		if err != nil {
			log.Printf("mock: approval key: %v", err)
			return nil
		}
		return k
	}
	after := func(why protocol.Why) mockphone.Approver {
		return func(ctx context.Context, _ mockphone.ApprovalRequest) protocol.Why {
			select {
			case <-time.After(delay):
				return why
			case <-ctx.Done():
				return protocol.WhyTimeout
			}
		}
	}
	switch mode {
	case "auto":
		opts.Approve = after("")
	case "tamper":
		opts.Approve = after("")
		opts.TamperApprovals = true
	case "deny":
		opts.Approve = after(protocol.WhyDenied)
	case "cancel":
		opts.Approve = after(protocol.WhyCancelled)
	case "lockout":
		opts.Approve = after(protocol.WhyLockout)
	case "timeout":
		opts.Approve = mockphone.NeverAnswer
	case "ask":
		opts.Approve = newAsker(in, out).ask
	default:
		return fmt.Errorf("--approval: %q is not one of %s", mode, approvalModes)
	}
	return nil
}

// asker plays the user on the mock's terminal. Lines are read by one
// goroutine for the whole run.
type asker struct {
	out   io.Writer
	lines chan string
	mu    sync.Mutex
}

func newAsker(in io.Reader, out io.Writer) *asker {
	a := &asker{out: out, lines: make(chan string)}
	go func() {
		defer close(a.lines)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			a.lines <- sc.Text()
		}
	}()
	return a
}

func (a *asker) ask(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.out, "\n%q asks for approval (%v to answer):\n    %s\nApprove with your fingerprint? [y = approve, n = deny, c = cancel, l = lockout] ", req.PC.Name, req.Timeout, req.Reason)
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fmt.Fprintln(a.out, "\n(timed out)")
			} else {
				fmt.Fprintln(a.out, "\n(cancelled: the PC went away)")
			}
			return protocol.WhyTimeout
		case line, ok := <-a.lines:
			if !ok {
				fmt.Fprintln(a.out, "\n(no terminal input; cancelled)")
				return protocol.WhyCancelled
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return ""
			case "n", "no":
				return protocol.WhyDenied
			case "c":
				return protocol.WhyCancelled
			case "l":
				return protocol.WhyLockout
			}
			fmt.Fprint(a.out, "[y/n/c/l] ")
		}
	}
}

// approvalKeyCmd shows, or with --rotate replaces, the mock's approval key.
func approvalKeyCmd(args []string, out io.Writer) error {
	fs := newFlags("approval-key")
	stateDir := fs.String("state", "", "state directory (required)")
	rotate := fs.Bool("rotate", false, "replace the key, as a phone does when fingerprints were added or removed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openState(*stateDir)
	if err != nil {
		return err
	}
	var key *ecdsa.PrivateKey
	if *rotate {
		key, err = st.RotateApprovalKey()
	} else {
		key, err = st.ApprovalKey()
	}
	if err != nil {
		return err
	}
	_, pin, err := security.EncodeApprovalKey(&key.PublicKey)
	if err != nil {
		return err
	}
	if *rotate {
		fmt.Fprintf(out, "New approval key %s.\nNew sessions announce it; PCs refuse approvals until their user accepts it (phonetopc approval-key --accept).\n", pin)
		return nil
	}
	fmt.Fprintf(out, "Approval key %s\n", pin)
	return nil
}
