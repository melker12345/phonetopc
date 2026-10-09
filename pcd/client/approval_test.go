package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

type approvalVector struct {
	Name      string `json:"name"`
	PhoneID   string `json:"phone_id"`
	PCID      string `json:"pc_id"`
	Nonce     string `json:"nonce"`
	Reason    string `json:"reason"`
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

func loadApprovalVectors(t *testing.T) (valid approvalVector, invalid []approvalVector) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "conformance", "vectors", "approval.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Valid   approvalVector   `json:"valid"`
		Invalid []approvalVector `json:"invalid"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Valid, v.Invalid
}

// approvalPhone is a fake secure phone: its hello names phoneID, pcID and
// the approval key, and answer produces the request_approval result.
type approvalPhone struct {
	phoneID, pcID, key string
	caps               []string
	answer             func(p protocol.RequestApprovalParams) any
	requests           atomic.Int32
	lastParams         atomic.Value // protocol.RequestApprovalParams
}

func (ap *approvalPhone) serve(t *testing.T) string {
	return fakeServer(t, func(ctx context.Context, fc *fakeConn) {
		for {
			m, err := fc.read(ctx)
			if err != nil {
				return
			}
			switch m.Method {
			case protocol.MethodHello:
				h := helloResult()
				h.Device.ID = ap.phoneID
				h.PC = &protocol.PCInfo{ID: ap.pcID, Name: "pc"}
				h.Capabilities = append(h.Capabilities, protocol.CapabilityApproval)
				if ap.caps != nil {
					h.Capabilities = ap.caps
				}
				if ap.key != "" {
					pin, _ := security.ApprovalKeyPin(ap.key)
					h.Approval = &protocol.ApprovalInfo{Key: ap.key, Pin: pin}
				}
				fc.send(ctx, protocol.NewResult(m.ID, h))
			case protocol.MethodRequestApproval:
				ap.requests.Add(1)
				var p protocol.RequestApprovalParams
				if err := json.Unmarshal(m.Params, &p); err != nil {
					t.Errorf("params %s", m.Params)
				}
				ap.lastParams.Store(p)
				fc.send(ctx, protocol.NewResult(m.ID, ap.answer(p)))
			}
		}
	})
}

func pinOf(t *testing.T, key string) string {
	pin, err := security.ApprovalKeyPin(key)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// TestApprovalVectors replays vectors/approval.json through
// RequestApproval: the phone's hello carries the vector's ids and key, the
// client uses the vector's nonce and reason, and the phone answers with the
// vector's signature. Only the valid vector may be approved.
func TestApprovalVectors(t *testing.T) {
	valid, invalid := loadApprovalVectors(t)
	run := func(v approvalVector) (ApprovalResult, error) {
		ap := &approvalPhone{phoneID: v.PhoneID, pcID: v.PCID, key: v.Key,
			answer: func(protocol.RequestApprovalParams) any {
				return protocol.RequestApprovalResult{Approved: true, Signature: v.Signature}
			}}
		c, err := Dial(ctxT(t), ap.serve(t), WithApprovalKey(pinOf(t, v.Key)), func(o *options) { o.testNonce = v.Nonce })
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.RequestApproval(ctxT(t), v.Reason, 0)
	}
	res, err := run(valid)
	if err != nil || !res.Approved() || res.Signature != valid.Signature || res.Nonce != valid.Nonce {
		t.Fatalf("valid vector: %+v %v", res, err)
	}
	if len(invalid) < 6 {
		t.Fatalf("%d invalid vectors", len(invalid))
	}
	for _, v := range invalid {
		res, err := run(v)
		if res.Approved() || !errors.Is(err, ErrApprovalInvalid) {
			t.Errorf("%s: approved %v, err %v", v.Name, res.Approved(), err)
		}
	}
}

// TestApprovalUsesOwnValues: the client signs over its own reason and ids,
// sends a fresh 32-byte nonce, the requested timeout, and trims the reason.
func TestApprovalRequestAndOwnValues(t *testing.T) {
	key, err := security.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyB64, pin, _ := security.EncodeApprovalKey(&key.PublicKey)
	ap := &approvalPhone{phoneID: "phone-1", pcID: "pc-1", key: keyB64}
	ap.answer = func(p protocol.RequestApprovalParams) any {
		sig, _ := security.SignApproval(key, security.ApprovalMessage("phone-1", "pc-1", p.Nonce, p.Reason))
		return protocol.RequestApprovalResult{Approved: true, Signature: sig}
	}
	c, err := Dial(ctxT(t), ap.serve(t), WithApprovalKey(pin))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.RequestApproval(ctxT(t), "  Deploy?  ", 7*time.Second)
	if err != nil || !res.Approved() || res.Reason != "Deploy?" || res.KeyPin != pin {
		t.Fatalf("%+v %v", res, err)
	}
	p := ap.lastParams.Load().(protocol.RequestApprovalParams)
	if n, err := security.DecodeB64(p.Nonce); err != nil || len(n) != 32 || p.Nonce != res.Nonce {
		t.Errorf("nonce %q", p.Nonce)
	}
	if p.TimeoutMS == nil || *p.TimeoutMS != 7000 || p.Reason != "Deploy?" {
		t.Errorf("params %+v", p)
	}
	res2, _ := c.RequestApproval(ctxT(t), "Deploy?", 0)
	if p2 := ap.lastParams.Load().(protocol.RequestApprovalParams); p2.Nonce == p.Nonce || *p2.TimeoutMS != 30000 || !res2.Approved() {
		t.Errorf("second request reused the nonce or lacks the default timeout: %+v", p2)
	}

	// The phone signs a different reason (or echoes a different nonce in
	// its message): verification uses our values, so it fails.
	ap.answer = func(p protocol.RequestApprovalParams) any {
		sig, _ := security.SignApproval(key, security.ApprovalMessage("phone-1", "pc-1", p.Nonce, "something else"))
		return protocol.RequestApprovalResult{Approved: true, Signature: sig}
	}
	if res, err := c.RequestApproval(ctxT(t), "Deploy?", 0); res.Approved() || !errors.Is(err, ErrApprovalInvalid) {
		t.Errorf("signature over another reason: %v %v", res.Approved(), err)
	}
	// approved without a signature.
	ap.answer = func(protocol.RequestApprovalParams) any { return map[string]any{"approved": true} }
	if res, err := c.RequestApproval(ctxT(t), "Deploy?", 0); res.Approved() || !errors.Is(err, ErrApprovalInvalid) {
		t.Errorf("no signature: %v %v", res.Approved(), err)
	}
	// Every refusal is a result.
	for _, why := range protocol.Whys {
		ap.answer = func(protocol.RequestApprovalParams) any { return protocol.RequestApprovalResult{Why: why} }
		res, err := c.RequestApproval(ctxT(t), "Deploy?", 0)
		if err != nil || res.Approved() || res.Why != why || !strings.Contains(res.String(), string(why)) {
			t.Errorf("%s: %+v %v", why, res, err)
		}
	}
}

func TestApprovalKeyTrust(t *testing.T) {
	key, _ := security.GenerateKey()
	keyB64, pin, _ := security.EncodeApprovalKey(&key.PublicKey)
	ap := &approvalPhone{phoneID: "phone-1", pcID: "pc-1", key: keyB64,
		answer: func(protocol.RequestApprovalParams) any { return protocol.RequestApprovalResult{Why: WhyDenied} }}
	url := ap.serve(t)

	// A changed key: a typed error, and nothing is sent.
	c, err := Dial(ctxT(t), url, WithApprovalKey("sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.RequestApproval(ctxT(t), "Deploy?", 0)
	var kc *ApprovalKeyChangedError
	if !errors.As(err, &kc) || !errors.Is(err, ErrApprovalKeyChanged) || kc.Current != pin {
		t.Errorf("want key changed, got %v", err)
	}
	if offered, err := c.CheckApprovalKey(); !offered || !errors.Is(err, ErrApprovalKeyChanged) {
		t.Errorf("CheckApprovalKey: %v %v", offered, err)
	}
	// No trust configured at all.
	c2, _ := Dial(ctxT(t), url)
	defer c2.Close()
	if _, err := c2.RequestApproval(ctxT(t), "Deploy?", 0); !errors.Is(err, ErrNoApprovalTrust) {
		t.Errorf("no trust: %v", err)
	}
	if n := ap.requests.Load(); n != 0 {
		t.Errorf("%d requests sent despite an untrusted key", n)
	}
	// A hello whose pin doesn't belong to its key.
	url2 := fakeServer(t, func(ctx context.Context, fc *fakeConn) {
		m, _ := fc.read(ctx)
		h := helloResult()
		h.PC = &protocol.PCInfo{ID: "pc", Name: "pc"}
		h.Capabilities = append(h.Capabilities, protocol.CapabilityApproval)
		h.Approval = &protocol.ApprovalInfo{Key: keyB64, Pin: "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		fc.send(ctx, protocol.NewResult(m.ID, h))
		fc.drain(ctx)
	})
	c3, _ := Dial(ctxT(t), url2, WithApprovalKey("sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	defer c3.Close()
	if _, err := c3.RequestApproval(ctxT(t), "Deploy?", 0); !errors.Is(err, ErrApprovalInvalid) {
		t.Errorf("pin not matching key: %v", err)
	}
}

func TestApprovalUnavailableAndArguments(t *testing.T) {
	key, _ := security.GenerateKey()
	keyB64, pin, _ := security.EncodeApprovalKey(&key.PublicKey)
	ap := &approvalPhone{phoneID: "p", pcID: "pc", caps: []string{"events", "get"}, key: keyB64,
		answer: func(protocol.RequestApprovalParams) any {
			return protocol.RequestApprovalResult{Approved: true, Signature: "x"}
		}}
	c, err := Dial(ctxT(t), ap.serve(t), WithApprovalKey(pin))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// No capability: unavailable, without asking the phone.
	if res, err := c.RequestApproval(ctxT(t), "Deploy?", 0); err != nil || res.Approved() || res.Why != WhyUnavailable {
		t.Errorf("no capability: %+v %v", res, err)
	}
	if ap.requests.Load() != 0 {
		t.Error("request sent without capability")
	}
	for _, bad := range []struct {
		reason  string
		timeout time.Duration
	}{{"", 0}, {"   ", 0}, {strings.Repeat("x", 201), 0}, {"a\nb", 0}, {"ok", 4 * time.Second}, {"ok", 121 * time.Second}, {"ok", -time.Second}} {
		if _, err := c.RequestApproval(ctxT(t), bad.reason, bad.timeout); err == nil {
			t.Errorf("accepted %q / %v", bad.reason, bad.timeout)
		}
	}

	// Dev mode (no pc in hello): approvals don't exist.
	dev := fakeServer(t, func(ctx context.Context, fc *fakeConn) {
		fc.answerHello(ctx)
		fc.drain(ctx)
	})
	cd, err := Dial(ctxT(t), dev)
	if err != nil {
		t.Fatal(err)
	}
	defer cd.Close()
	if _, err := cd.RequestApproval(ctxT(t), "Deploy?", 0); !errors.Is(err, ErrApprovalNeedsSecureMode) {
		t.Errorf("dev mode: %v", err)
	}
}
