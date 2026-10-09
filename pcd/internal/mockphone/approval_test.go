package mockphone_test

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// approvalMock is a secure mock with an approval key.
type approvalMock struct {
	*secureMock
	key *ecdsa.PrivateKey
	pin string
}

func startApproval(t *testing.T, approve mockphone.Approver, tweak func(*mockphone.Options)) *approvalMock {
	t.Helper()
	key, err := security.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, pin, _ := security.EncodeApprovalKey(&key.PublicKey)
	m := startSecure(t, func(o *mockphone.Options) {
		o.ApprovalKey = func() *ecdsa.PrivateKey { return key }
		o.Approve = approve
		if tweak != nil {
			tweak(o)
		}
	})
	return &approvalMock{secureMock: m, key: key, pin: pin}
}

func (m *approvalMock) dialApproval(t *testing.T) *client.Client {
	t.Helper()
	c, err := m.dial(t, client.WithApprovalKey(m.pin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestApprovalApproved(t *testing.T) {
	var (
		mu sync.Mutex
		v  = newFrameValidator(t)
	)
	var seen mockphone.ApprovalRequest
	m := startApproval(t, func(_ context.Context, req mockphone.ApprovalRequest) protocol.Why {
		seen = req
		return ""
	}, func(o *mockphone.Options) {
		o.Tap = func(dir mockphone.Direction, frame []byte) {
			mu.Lock()
			defer mu.Unlock()
			v.check(dir, frame, false)
		}
	})
	c := m.dialApproval(t)
	h := c.Server()
	if !h.HasCapability(protocol.CapabilityApproval) || h.Approval == nil || h.Approval.Pin != m.pin {
		t.Fatalf("hello %+v approval %+v", h, h.Approval)
	}
	res, err := c.RequestApproval(testCtx(t), "Run deploy.sh?", 10*time.Second)
	if err != nil || !res.Approved() {
		t.Fatalf("%+v %v", res, err)
	}
	if seen.Reason != "Run deploy.sh?" || seen.PC.ID != "pc-1" || seen.Timeout != 10*time.Second {
		t.Errorf("the user saw %+v", seen)
	}
	// The signature is over exactly the message of §5.
	msg := security.ApprovalMessage(h.Device.ID, "pc-1", res.Nonce, "Run deploy.sh?")
	if err := security.VerifyApproval(&m.key.PublicKey, res.Signature, msg); err != nil {
		t.Error(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if v.checked < 4 {
		t.Errorf("only %d frames validated", v.checked)
	}
}

func TestApprovalOutcomes(t *testing.T) {
	for _, why := range []protocol.Why{protocol.WhyDenied, protocol.WhyCancelled, protocol.WhyLockout} {
		m := startApproval(t, mockphone.Answer(why), nil)
		res, err := m.dialApproval(t).RequestApproval(testCtx(t), "Deploy?", 0)
		if err != nil || res.Approved() || res.Why != why {
			t.Errorf("%s: %+v %v", why, res, err)
		}
	}
}

func TestApprovalTimeoutKeepsEventsFlowing(t *testing.T) {
	t.Parallel()
	m := startApproval(t, mockphone.NeverAnswer, nil)
	c := m.dialApproval(t)
	sub, err := c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 20})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan struct{})
	var (
		res  client.ApprovalResult
		rerr error
	)
	go func() {
		defer close(done)
		res, rerr = c.RequestApproval(testCtx(t), "Deploy?", client.MinApprovalTimeout)
	}()
	// Events and other calls continue while the approval is pending.
	nextEvent(t, sub)
	if _, err := c.Get(testCtx(t), "battery"); err != nil {
		t.Fatal(err)
	}
	<-done
	if rerr != nil || res.Approved() || res.Why != client.WhyTimeout {
		t.Errorf("%+v %v", res, rerr)
	}
	if d := time.Since(start); d < client.MinApprovalTimeout-100*time.Millisecond {
		t.Errorf("timed out after %v", d)
	}
}

func TestApprovalBusyAndCancelOnClose(t *testing.T) {
	asked := make(chan struct{}, 4)
	ended := make(chan error, 4)
	m := startApproval(t, func(ctx context.Context, _ mockphone.ApprovalRequest) protocol.Why {
		asked <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return protocol.WhyTimeout
	}, nil)
	first := m.dialApproval(t)
	go first.RequestApproval(context.Background(), "first", 60*time.Second)
	<-asked

	// One at a time per phone, across sessions.
	second := m.dialApproval(t)
	res, err := second.RequestApproval(testCtx(t), "second", 0)
	if err != nil || res.Why != client.WhyBusy {
		t.Fatalf("want busy, got %+v %v", res, err)
	}

	// Closing the first session cancels its prompt, and frees the phone.
	first.Close()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("prompt ended with %v, want cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt not cancelled when the session closed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		go second.RequestApproval(context.Background(), "third", 60*time.Second)
		select {
		case <-asked:
			return // shown to the user: no longer busy
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("still busy after the first session closed")
		}
	}
}

func TestApprovalTampered(t *testing.T) {
	m := startApproval(t, mockphone.ApproveAll, func(o *mockphone.Options) { o.TamperApprovals = true })
	res, err := m.dialApproval(t).RequestApproval(testCtx(t), "Deploy?", 0)
	if res.Approved() || !errors.Is(err, client.ErrApprovalInvalid) {
		t.Fatalf("tampered signature: %+v %v", res, err)
	}
}

func TestApprovalUnavailableAndDevMode(t *testing.T) {
	// Secure, but no approval key: no capability, and a raw request gets
	// unavailable after its params were checked.
	m := startSecure(t, nil)
	c, err := m.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if h := c.Server(); h.HasCapability(protocol.CapabilityApproval) || h.Approval != nil {
		t.Errorf("hello offers approvals: %+v", h)
	}
	r := dialRawSecure(t, m)
	r.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"` + m.pc.Token + `"}}}`)
	if resp := r.response(); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	nonce := security.B64(security.RandomBytes(32))
	resp := r.call(protocol.MethodRequestApproval, map[string]any{"reason": "Deploy?", "nonce": nonce})
	if resp.Error != nil || !strings.Contains(string(resp.Result), `"why":"unavailable"`) {
		t.Errorf("no key: %s %+v", resp.Result, resp.Error)
	}
	wantError(t, r.call(protocol.MethodRequestApproval, map[string]any{"reason": " Deploy?", "nonce": nonce}), protocol.CodeInvalidParams, "2")

	// Dev mode: the method doesn't exist.
	_, url := start(t, quietOptions())
	rd := dialRaw(t, url, nil)
	rd.hello()
	wantError(t, rd.call(protocol.MethodRequestApproval, map[string]any{"reason": "Deploy?", "nonce": nonce}), protocol.CodeMethodNotFound, "2")
}

func TestApprovalKeyFromState(t *testing.T) {
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k1, err := st.ApprovalKey()
	if err != nil {
		t.Fatal(err)
	}
	k1again, _ := st.ApprovalKey()
	k2, err := st.RotateApprovalKey()
	if err != nil {
		t.Fatal(err)
	}
	k2again, _ := st.ApprovalKey()
	if !k1.Equal(k1again) || k1.Equal(k2) || !k2.Equal(k2again) {
		t.Error("approval key not persisted or not rotated")
	}
}
