package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func TestApprovalModes(t *testing.T) {
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := mockphone.ApprovalRequest{PC: protocol.PCInfo{Name: "desk"}, Program: "deploy.sh", Reason: "Deploy?",
		Choices: [3]int{12, 42, 77}, Match: 42, Timeout: time.Second}
	decide := func(mode, input string) (mockphone.Options, protocol.Why, string) {
		var o mockphone.Options
		var out bytes.Buffer
		if err := approvalOptions(&o, mode, 0, st, strings.NewReader(input), &out); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return o, o.Approve(ctx, req), out.String()
	}
	for mode, want := range map[string]protocol.Why{
		"auto": "", "tamper": "", "deny": protocol.WhyDenied, "cancel": protocol.WhyCancelled, "mismatch": protocol.WhyMismatch,
		"lockout": protocol.WhyLockout, "timeout": protocol.WhyTimeout,
	} {
		o, why, _ := decide(mode, "")
		if why != want || o.ApprovalKey == nil || o.ApprovalKey() == nil || o.TamperApprovals != (mode == "tamper") {
			t.Errorf("%s: why %q, options %+v", mode, why, o)
		}
	}
	if _, why, out := decide("ask", "maybe\n13\n42\n"); why != "" || !strings.Contains(out, `deploy.sh on "desk" asks for approval`) ||
		!strings.Contains(out, "Deploy?") || !strings.Contains(out, "[12 / 42 / 77]") {
		t.Errorf("ask 42: %q\n%s", why, out)
	}
	if _, why, _ := decide("ask", "77\n"); why != protocol.WhyMismatch {
		t.Errorf("ask 77: %q", why)
	}
	if _, why, _ := decide("ask", "n\n"); why != protocol.WhyDenied {
		t.Errorf("ask n: %q", why)
	}
	// No answer before the timeout.
	var o mockphone.Options
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	approvalOptions(&o, "ask", 0, st, pr, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if why := o.Approve(ctx, req); why != protocol.WhyTimeout || !strings.Contains(out.String(), "(timed out)") {
		t.Errorf("ask without answer: %q %s", why, out.String())
	}

	var off mockphone.Options
	if err := approvalOptions(&off, "off", 0, st, nil, io.Discard); err != nil || off.ApprovalKey != nil {
		t.Errorf("off: %v %+v", err, off)
	}
	if err := approvalOptions(&off, "sometimes", 0, st, nil, io.Discard); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestApprovalKeyCommand(t *testing.T) {
	dir := t.TempDir()
	var a, b, c bytes.Buffer
	if err := approvalKeyCmd([]string{"--state", dir}, &a); err != nil {
		t.Fatal(err)
	}
	if err := approvalKeyCmd([]string{"--state", dir}, &b); err != nil || a.String() != b.String() {
		t.Errorf("key changed between runs: %s / %s %v", a.String(), b.String(), err)
	}
	if err := approvalKeyCmd([]string{"--state", dir, "--rotate"}, &c); err != nil || !strings.Contains(c.String(), "New approval key sha256/") {
		t.Fatalf("rotate: %s %v", c.String(), err)
	}
	pin := strings.TrimSpace(strings.TrimPrefix(a.String(), "Approval key "))
	if strings.Contains(c.String(), pin) {
		t.Error("rotation kept the key")
	}
}
