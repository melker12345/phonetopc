package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// End-to-end tests of `phonetopc approve` and `approval-key` against the
// secure mock phone with approvals.

type approvalPhone struct {
	*phone
	asked atomic.Int32 // how often the "user" was asked
}

func startApprovalPhone(t *testing.T, approve mockphone.Approver, tamper bool) *approvalPhone {
	t.Helper()
	ap := &approvalPhone{}
	ap.phone = startPhoneOn(t, "", func(o *mockphone.Options, st *mockphone.State) {
		o.ApprovalKey = func() *ecdsa.PrivateKey {
			k, err := st.ApprovalKey()
			if err != nil {
				t.Error(err)
			}
			return k
		}
		o.Approve = func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
			ap.asked.Add(1)
			return approve(ctx, req)
		}
		o.TamperApprovals = tamper
	})
	return ap
}

func storedDevice(t *testing.T, env map[string]string) store.Device {
	t.Helper()
	d, err := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")).Find("")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApproveApproved(t *testing.T) {
	env := testEnv(t, nil)
	ph := startApprovalPhone(t, mockphone.ApproveAll, false)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	if storedDevice(t, env).ApprovalKey != "" {
		t.Fatal("approval key stored before first sight")
	}
	code, out, errs := runCLI(t, tctx(t), env, "approve", "Deploy?")
	if code != 0 || !strings.Contains(errs, `approved on "Test Phone"`) || out != "" {
		t.Fatalf("approve: %d %q %s", code, out, errs)
	}
	// First sight stored the key; info shows it as trusted.
	pin := storedDevice(t, env).ApprovalKey
	if !strings.HasPrefix(pin, "sha256/") {
		t.Errorf("stored approval key %q", pin)
	}
	code, out, _ = runCLI(t, tctx(t), env, "info")
	if code != 0 || !strings.Contains(out, "approval") || !strings.Contains(out, pin+" (trusted)") {
		t.Errorf("info:\n%s", out)
	}
	code, out, _ = runCLI(t, tctx(t), env, "approve", "Deploy?", "--json", "--timeout", "10s")
	var res map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res["approved"] != true {
		t.Errorf("--json: %d %s", code, out)
	}
}

func TestApproveNotApproved(t *testing.T) {
	for _, why := range []protocol.Why{protocol.WhyDenied, protocol.WhyCancelled, protocol.WhyLockout} {
		env := testEnv(t, nil)
		ph := startApprovalPhone(t, mockphone.Answer(why), false)
		pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
		code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?")
		if code != 1 || !strings.Contains(errs, "not approved: "+string(why)) {
			t.Errorf("%s: %d %s", why, code, errs)
		}
		code, out, _ := runCLI(t, tctx(t), env, "approve", "Deploy?", "--json")
		if code != 1 || !strings.Contains(out, `"why":"`+string(why)+`"`) {
			t.Errorf("%s --json: %d %s", why, code, out)
		}
	}
}

func TestApproveTimeout(t *testing.T) {
	t.Parallel()
	env := testEnv(t, nil)
	ph := startApprovalPhone(t, mockphone.NeverAnswer, false)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?", "--timeout", "5s")
	if code != 1 || !strings.Contains(errs, "not approved: timeout") {
		t.Errorf("%d %s", code, errs)
	}
}

func TestApproveBusy(t *testing.T) {
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	env := testEnv(t, nil)
	ph := startApprovalPhone(t, func(ctx context.Context, _ mockphone.ApprovalRequest) protocol.Why {
		asked <- struct{}{}
		select {
		case <-release:
			return ""
		case <-ctx.Done():
			return protocol.WhyTimeout
		}
	}, false)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	first := make(chan int, 1)
	go func() {
		code, _, _ := runCLI(t, tctx(t), env, "approve", "first")
		first <- code
	}()
	<-asked
	code, _, errs := runCLI(t, tctx(t), env, "approve", "second")
	if code != 1 || !strings.Contains(errs, "not approved: busy") {
		t.Errorf("second: %d %s", code, errs)
	}
	close(release)
	if code := <-first; code != 0 {
		t.Errorf("first: %d", code)
	}
}

func TestApprovalKeyChange(t *testing.T) {
	env := testEnv(t, nil)
	ph := startApprovalPhone(t, mockphone.ApproveAll, false)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	if code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?"); code != 0 {
		t.Fatalf("first approve: %d %s", code, errs)
	}
	old := storedDevice(t, env).ApprovalKey

	// Fingerprints changed on the phone: a new approval key.
	if _, err := ph.state.RotateApprovalKey(); err != nil {
		t.Fatal(err)
	}
	asked := ph.asked.Load()
	code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?")
	if code != 2 || !strings.Contains(errs, "approval key of \"phone\" changed") || !strings.Contains(errs, "phonetopc approval-key --accept") {
		t.Errorf("changed key: %d %s", code, errs)
	}
	if ph.asked.Load() != asked {
		t.Error("the phone was asked despite the changed key")
	}
	if storedDevice(t, env).ApprovalKey != old {
		t.Error("the changed key was stored without acceptance")
	}
	code, out, errs := runCLI(t, tctx(t), env, "info")
	if code != 0 || !strings.Contains(out, "CHANGED") || !strings.Contains(errs, "warning: the approval key") {
		t.Errorf("info: %d\n%s\n%s", code, out, errs)
	}
	code, out, _ = runCLI(t, tctx(t), env, "approval-key")
	if code != 1 || !strings.Contains(out, "accepted  "+old) || !strings.Contains(out, "CHANGED") {
		t.Errorf("approval-key: %d\n%s", code, out)
	}

	code, out, errs = runCLI(t, tctx(t), env, "approval-key", "--accept")
	if code != 0 || !strings.Contains(out, "accepted now") {
		t.Fatalf("--accept: %d\n%s\n%s", code, out, errs)
	}
	now := storedDevice(t, env).ApprovalKey
	if now == old || !strings.Contains(out, "current   "+now) {
		t.Errorf("accepted %q (old %q)\n%s", now, old, out)
	}
	if code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?"); code != 0 {
		t.Errorf("approve after accepting: %d %s", code, errs)
	}
	if code, out, _ := runCLI(t, tctx(t), env, "approval-key"); code != 0 || !strings.Contains(out, "status    trusted") {
		t.Errorf("approval-key after accepting: %d\n%s", code, out)
	}
}

func TestApproveTamperedSignature(t *testing.T) {
	env := testEnv(t, nil)
	ph := startApprovalPhone(t, mockphone.ApproveAll, true)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	code, out, errs := runCLI(t, tctx(t), env, "approve", "Deploy?", "--json")
	if code != 2 || out != "" || !strings.Contains(errs, "signature is invalid") || !strings.Contains(errs, "NOT an approval") {
		t.Errorf("%d %q %s", code, out, errs)
	}
}

func TestApproveErrors(t *testing.T) {
	env := testEnv(t, nil)
	// Usage errors.
	for _, args := range [][]string{{"approve"}, {"approve", "a", "b"}, {"approve", "x", "--timeout", "1s"}} {
		if code, _, _ := runCLI(t, tctx(t), env, args...); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
	// No paired phone: an error, exit 2.
	if code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?"); code != 2 || !strings.Contains(errs, "no paired phone") {
		t.Errorf("unpaired: %d %s", code, errs)
	}
	ph := startApprovalPhone(t, mockphone.ApproveAll, false)
	pairDirect(t, env, ph.phone, "phone", []string{"127.0.0.1"})
	// A reason the spec forbids is refused before asking.
	if code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy? "); code != 2 || !strings.Contains(errs, "whitespace") || ph.asked.Load() != 0 {
		t.Errorf("trailing space: %d %s", code, errs)
	}
	// Dev mode has no approvals.
	_, addr := startMock(t)
	if code, _, errs := runCLI(t, tctx(t), env, "--insecure-dev", "--addr", addr, "approve", "Deploy?"); code != 2 || !strings.Contains(errs, "secure mode") {
		t.Errorf("dev mode: %d %s", code, errs)
	}
	// A phone without approvals: unavailable, exit 1.
	env2 := testEnv(t, nil)
	plain := startPhone(t)
	pairDirect(t, env2, plain, "plain", []string{"127.0.0.1"})
	if code, _, errs := runCLI(t, tctx(t), env2, "approve", "Deploy?"); code != 1 || !strings.Contains(errs, "unavailable") {
		t.Errorf("no approvals: %d %s", code, errs)
	}
	if code, out, _ := runCLI(t, tctx(t), env2, "info"); code != 0 || !strings.Contains(out, "approval      no") {
		t.Errorf("info without approvals:\n%s", out)
	}
}
