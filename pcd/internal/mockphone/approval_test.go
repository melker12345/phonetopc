package mockphone_test

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
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

	// One at a time per PC, across sessions.
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

// queueMock is an approval mock whose user reports the PC of each
// approval shown and answers it with what release gets.
type queueMock struct {
	*approvalMock
	shown   chan string
	release chan protocol.Why
	arrived chan struct{} // a request_approval or decrypt reached the phone
}

func startQueue(t *testing.T, tweak func(*mockphone.Options)) *queueMock {
	q := &queueMock{
		shown:   make(chan string, 16),
		release: make(chan protocol.Why),
		arrived: make(chan struct{}, 64),
	}
	q.approvalMock = startApproval(t, func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
		q.shown <- req.PC.ID
		select {
		case why := <-q.release:
			return why
		case <-ctx.Done():
			return protocol.WhyTimeout
		}
	}, func(o *mockphone.Options) {
		o.Tap = func(dir mockphone.Direction, frame []byte) {
			f := string(frame)
			if dir == mockphone.ClientToServer &&
				(strings.Contains(f, `"method":"request_approval"`) || strings.Contains(f, `"method":"decrypt"`)) {
				q.arrived <- struct{}{}
			}
		}
		if tweak != nil {
			tweak(o)
		}
	})
	return q
}

// dialAs pairs another PC and dials as it.
func (q *queueMock) dialAs(t *testing.T, id string) *client.Client {
	t.Helper()
	pc := mockphone.PairedPC{ID: id, Name: id, Pin: q.id.Pin, Token: security.NewToken()}
	if err := q.state.PutPC(pc); err != nil {
		t.Fatal(err)
	}
	c, err := q.dial(t, client.WithApprovalKey(q.pin), client.WithToken(pc.Token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type approvalAnswer struct {
	res client.ApprovalResult
	err error
}

// ask returns once the request reached the phone; its answer follows on
// the channel.
func (q *queueMock) ask(t *testing.T, c *client.Client, timeout time.Duration) <-chan approvalAnswer {
	t.Helper()
	ch := make(chan approvalAnswer, 1)
	go func() {
		res, err := c.RequestApproval(context.Background(), "Deploy?", timeout)
		ch <- approvalAnswer{res, err}
	}()
	select {
	case <-q.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the phone")
	}
	return ch
}

// queued asks and checks that the request is pending (shown or waiting).
func (q *queueMock) queued(t *testing.T, c *client.Client) <-chan approvalAnswer {
	t.Helper()
	ch := q.ask(t, c, 60*time.Second)
	q.wantBusy(t, c)
	return ch
}

func (q *queueMock) wantBusy(t *testing.T, c *client.Client) {
	t.Helper()
	wantAnswer(t, q.ask(t, c, 0), client.WhyBusy)
}

func (q *queueMock) wantShown(t *testing.T, pc string) {
	t.Helper()
	select {
	case got := <-q.shown:
		if got != pc {
			t.Fatalf("shown %s, want %s", got, pc)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s not shown", pc)
	}
}

func (q *queueMock) wantNothingShown(t *testing.T) {
	t.Helper()
	select {
	case got := <-q.shown:
		t.Fatalf("shown %s meanwhile", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func wantAnswer(t *testing.T, ch <-chan approvalAnswer, why client.Why) {
	t.Helper()
	select {
	case a := <-ch:
		if a.err != nil || a.res.Why != why || a.res.Approved() != (why == "") {
			t.Fatalf("got %+v %v, want %q", a.res, a.err, why)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer")
	}
}

func TestApprovalOtherPCsWaitTheirTurn(t *testing.T) {
	q := startQueue(t, nil)
	a, b, c := q.dialApproval(t), q.dialAs(t, "pc-2"), q.dialAs(t, "pc-3")
	ra := q.ask(t, a, 60*time.Second)
	q.wantShown(t, "pc-1")
	rb := q.queued(t, b)
	rc := q.queued(t, c)
	q.wantNothingShown(t)

	q.release <- protocol.WhyDenied
	wantAnswer(t, ra, client.WhyDenied)
	q.wantShown(t, "pc-2")
	q.release <- ""
	wantAnswer(t, rb, "")
	q.wantShown(t, "pc-3")
	q.release <- protocol.WhyCancelled
	wantAnswer(t, rc, client.WhyCancelled)
}

func TestApprovalWaitingTimesOutUnseen(t *testing.T) {
	t.Parallel()
	q := startQueue(t, nil)
	a, b := q.dialApproval(t), q.dialAs(t, "pc-2")
	ra := q.ask(t, a, 60*time.Second)
	q.wantShown(t, "pc-1")
	wantAnswer(t, q.ask(t, b, client.MinApprovalTimeout), client.WhyTimeout)
	q.release <- protocol.WhyDenied
	wantAnswer(t, ra, client.WhyDenied)
	q.wantNothingShown(t)
}

func TestApprovalWaitingOfClosedSessionVanishes(t *testing.T) {
	q := startQueue(t, nil)
	a, b, c := q.dialApproval(t), q.dialAs(t, "pc-2"), q.dialAs(t, "pc-3")
	ra := q.ask(t, a, 60*time.Second)
	q.wantShown(t, "pc-1")
	q.queued(t, b)
	q.queued(t, c)
	b.Close()

	// pc-2 can ask again once its old request left the queue, and now
	// waits behind pc-3.
	b2 := q.dialAs(t, "pc-2")
	deadline := time.Now().Add(5 * time.Second)
	for queued := false; !queued; {
		select {
		case got := <-q.ask(t, b2, 60*time.Second):
			if got.res.Why != client.WhyBusy || time.Now().After(deadline) {
				t.Fatalf("pc-2 asking again: %+v %v", got.res, got.err)
			}
			time.Sleep(20 * time.Millisecond)
		case <-time.After(200 * time.Millisecond):
			queued = true
		}
	}
	q.release <- protocol.WhyDenied
	wantAnswer(t, ra, client.WhyDenied)
	q.wantShown(t, "pc-3")
	q.release <- protocol.WhyDenied
	q.wantShown(t, "pc-2")
}

func TestApprovalQueueBound(t *testing.T) {
	q := startQueue(t, nil)
	ra := q.ask(t, q.dialApproval(t), 60*time.Second)
	q.wantShown(t, "pc-1")
	for i := range mockphone.MaxWaitingApprovals {
		q.queued(t, q.dialAs(t, fmt.Sprintf("pc-%d", i+2)))
	}
	last := q.dialAs(t, "pc-last")
	q.wantBusy(t, last)
	q.release <- protocol.WhyDenied
	wantAnswer(t, ra, client.WhyDenied)
	q.wantShown(t, "pc-2")
	q.queued(t, last)
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

// TestApprovalAttestation: the mock attests its keys like a phone, bound to
// its TLS key, so a client over the pinned connection verifies them; a
// mock without attestation, or with a broken one, is refused before
// anything is sent.
func TestApprovalAttestation(t *testing.T) {
	m := startApproval(t, mockphone.ApproveAll, nil)
	res, err := m.dialApproval(t).RequestApproval(testCtx(t), "Deploy?", 0)
	if err != nil || !res.Approved() || res.Attestation != client.AttestedTEE {
		t.Fatalf("%+v %v", res, err)
	}
	cases := []struct {
		check string
		pin   string // the TLS pin the chain is bound to; "": the mock's
		tweak func(*attesttest.Description)
	}{
		{attest.CheckBoot, "", func(d *attesttest.Description) { d.DeviceLocked = false }},
		{attest.CheckSecurityLevel, "", func(d *attesttest.Description) { d.KeyMintLevel = attest.Software }},
		{attest.CheckChallenge, security.PinSPKI([]byte("another phone")), nil},
		{client.CheckMissing, "", nil},
	}
	for _, c := range cases {
		var (
			asked bool
			pin   string
		)
		m := startApproval(t, func(context.Context, mockphone.ApprovalRequest) protocol.Why { asked = true; return "" }, func(o *mockphone.Options) {
			if c.check == client.CheckMissing {
				o.Attest = nil
				return
			}
			o.Attest = func(spki []byte, p attest.Purpose) []string {
				return mockphone.TestAttestation(testCA, pin, c.tweak)(spki, p)
			}
		})
		pin = cmp.Or(c.pin, m.id.Pin)
		_, err := m.dialApproval(t).RequestApproval(testCtx(t), "Deploy?", 0)
		var kae *client.KeyAttestationError
		if !errors.As(err, &kae) || kae.Check != c.check || asked {
			t.Errorf("%s: %v, asked %v", c.check, err, asked)
		}
	}
}
