package main

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// exitError makes run exit with code after printing err (if not nil).
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

// approve asks the phone's user for a fingerprint approval (APPROVAL.md).
// Exit codes: 0 approved; 1 not approved (denied, cancelled, mismatch,
// timeout, busy, cooldown, lockout, unavailable); 2 errors, including an approval that failed
// verification, a changed approval key, and connection problems.
func (c *cli) approve(ctx context.Context, args []string) error {
	fs := c.flagSet("approve")
	timeout := fs.Duration("timeout", client.DefaultApprovalTimeout, "")
	pos, err := parse(fs, args, 1, "reason")
	if err != nil {
		return err
	}
	if *timeout < client.MinApprovalTimeout || *timeout > client.MaxApprovalTimeout {
		return usageErr{fmt.Errorf("--timeout must be between %v and %v", client.MinApprovalTimeout, client.MaxApprovalTimeout)}
	}
	// The reason is sent exactly as given; refuse what the phone would.
	if err := protocol.CheckReason(pos[0]); err != nil {
		return exitError{2, err}
	}
	fail := func(err error) error {
		if ctx.Err() != nil {
			err = errors.New("interrupted; not approved")
		}
		return exitError{2, err}
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return fail(err)
	}
	defer cl.Close()
	name := cl.Server().Device.Name
	show := func(match int) {
		fmt.Fprintf(c.stderr, "phonetopc: pick %d on %q, then confirm with your fingerprint (up to %v)…\n", match, name, *timeout)
	}
	res, err := cl.RequestApproval(ctx, client.ApprovalRequest{Reason: pos[0], Program: "phonetopc", Timeout: *timeout, ShowMatch: show})
	if err != nil {
		return fail(err)
	}
	if c.json {
		out := map[string]any{"approved": res.Approved(), "device": cl.Server().Device.ID, "match": res.Match}
		if !res.Approved() {
			out["why"] = res.Why
		}
		if err := c.printJSON(out); err != nil {
			return exitError{2, err}
		}
	} else if res.Approved() {
		fmt.Fprintf(c.stderr, "phonetopc: approved on %q\n", name)
	} else {
		fmt.Fprintf(c.stderr, "phonetopc: not approved: %s (%s)\n", res.Why, res.Why.Describe())
	}
	if !res.Approved() {
		return exitError{1, nil}
	}
	return nil
}

// APPROVAL.md §3.
func (c *cli) approvalKey(ctx context.Context, args []string) error {
	fs := c.flagSet("approval-key")
	accept := fs.Bool("accept", false, "")
	unattested := fs.Bool("unattested", false, "")
	if _, err := parse(fs, args, 0, ""); err != nil {
		return err
	}
	if *unattested && !*accept {
		return usageErr{errors.New("--unattested only goes with --accept")}
	}
	if c.dev {
		return errors.New("approval keys belong to paired phones; approvals don't exist in dev mode")
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	before, err := st.Find(c.device)
	if err != nil {
		return err
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	h := cl.Server()

	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintf(tw, "device\t%s (id %s)\n", before.Name, before.ID)
	accepted := before.ApprovalKey
	if accepted == "" {
		accepted = "none yet"
	}
	fmt.Fprintf(tw, "accepted\t%s\n", accepted)
	offered, err := cl.CheckApprovalKey()
	if !offered {
		fmt.Fprintf(tw, "current\tnone: the phone offers no approvals (no strong biometric enrolled?)\n")
		return nil
	}
	current, perr := security.ApprovalKeyPin(h.Approval.Key)
	fmt.Fprintf(tw, "current\t%s\n", h.Approval.Pin)
	var (
		changed *client.ApprovalKeyChangedError
		kae     *client.KeyAttestationError
	)
	switch {
	case perr != nil || errors.Is(err, client.ErrApprovalInvalid):
		return exitError{2, fmt.Errorf("the phone's approval key is malformed: %v", errors.Join(perr, err))}
	case errors.As(err, &kae) && kae.Check == client.CheckStrongBox:
		fmt.Fprintf(tw, "status\tREFUSED: %v\n", kae.Err)
		return exitError{1, nil}
	case errors.As(err, &changed) || errors.As(err, &kae):
		if !*accept {
			if changed != nil {
				fmt.Fprintf(tw, "status\tCHANGED: approvals are refused until you accept the new key\n")
				tw.Flush()
				fmt.Fprintln(c.stderr, "phonetopc: the approval key changed. That is expected after fingerprints were added or removed on the phone;\n"+
					"  if you did that, accept it with `phonetopc approval-key --accept`. Otherwise someone may be tampering with the phone app.")
			} else {
				fmt.Fprintf(tw, "status\tNOT ATTESTED: approvals are refused\n")
				tw.Flush()
				fmt.Fprintf(c.stderr, "phonetopc: %v.\n  %s\n", kae, unattestedHint)
			}
			return exitError{1, nil}
		}
		level, aerr := cl.AttestApprovalKey(ctx)
		if aerr != nil {
			if !*unattested {
				return exitError{2, fmt.Errorf("%w; the key was not accepted. %s", aerr, unattestedHint)}
			}
			level = client.Unattested
		}
		if err := st.AcceptApprovalKey(before.ID, current, level); err != nil {
			return err
		}
		fmt.Fprintf(tw, "attestation\t%s\n", describeAttestation(level))
		fmt.Fprintf(tw, "status\taccepted now\n")
	case err != nil:
		return err
	default:
		after, err := st.Find(before.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "attestation\t%s\n", describeAttestation(after.ApprovalAttestation))
		if before.ApprovalKey == "" {
			fmt.Fprintf(tw, "status\ttrusted (first seen now, over the pinned connection)\n")
		} else {
			fmt.Fprintf(tw, "status\ttrusted\n")
		}
	}
	return nil
}

const unattestedHint = "The phone's hardware didn't prove that this key needs your fingerprint for every use. If you trust this phone and app anyway\n" +
	"  (a custom ROM, an unlocked bootloader, an emulator), accept it with `phonetopc approval-key --accept --unattested`."

func describeAttestation(level string) string {
	switch level {
	case client.AttestedStrongBox:
		return "StrongBox (proved by the phone's hardware)"
	case client.AttestedTEE:
		return "TEE (proved by the phone's hardware)"
	case client.Unattested:
		return "none: accepted without attestation"
	}
	return "not checked"
}

func approvalRow(cl *client.Client) string {
	h := cl.Server()
	if h.PC == nil {
		return ""
	}
	offered, err := cl.CheckApprovalKey()
	switch {
	case !offered:
		return "no (the phone offers no approvals)"
	case errors.Is(err, client.ErrApprovalKeyChanged):
		return fmt.Sprintf("yes, key %s — CHANGED, refused until accepted (see `phonetopc approval-key`)", h.Approval.Pin)
	case errors.Is(err, client.ErrKeyAttestation):
		return fmt.Sprintf("yes, key %s — refused: %v (see `phonetopc approval-key`)", h.Approval.Pin, err)
	case err != nil:
		return fmt.Sprintf("yes, key %s — unusable: %v", h.Approval.Pin, err)
	}
	level, _ := cl.ApprovalKeyAttestation(context.Background())
	return fmt.Sprintf("yes, key %s (trusted, attestation: %s)", h.Approval.Pin, describeAttestation(level))
}
