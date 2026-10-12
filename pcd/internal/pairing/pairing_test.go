package pairing

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// prompter answers prompts from functions and records the SAS code.
type prompter struct {
	qr  func(PhoneInfo) bool
	sas func(code string) bool

	mu   sync.Mutex
	code string
}

func (p *prompter) ConfirmQR(ctx context.Context, phone PhoneInfo) (bool, error) {
	if p.qr == nil {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return p.qr(phone), nil
}

func (p *prompter) ConfirmSAS(ctx context.Context, phone PhoneInfo, code string) (bool, error) {
	p.mu.Lock()
	p.code = code
	p.mu.Unlock()
	if p.sas == nil {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return p.sas(code), nil
}

func (p *prompter) lastCode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func yes() *prompter {
	return &prompter{qr: func(PhoneInfo) bool { return true }, sas: func(string) bool { return true }}
}

type harness struct {
	l      *Listener
	addr   string
	pc     protocol.PCInfo
	saved  chan Paired
	done   chan struct{}
	res    Paired
	err    error
	cancel context.CancelFunc
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := security.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := security.SelfSigned(key, "pc")
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func startListener(t *testing.T, p Prompter, tweak func(*Options)) *harness {
	t.Helper()
	cert := testCert(t)
	h := &harness{pc: protocol.PCInfo{ID: "9d1c00000000aaaa", Name: "arch-desktop"}, saved: make(chan Paired, 4), done: make(chan struct{})}
	opts := Options{
		PC: h.pc, Cert: cert, Prompt: p,
		Save: func(pp Paired) error { h.saved <- pp; return nil },
	}
	if tweak != nil {
		tweak(&opts)
	}
	var err error
	h.l, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.res, h.err = h.l.Serve(ctx, ln)
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

func (h *harness) uri() URI {
	host, port, _ := net.SplitHostPort(h.addr)
	p, _ := strconv.Atoi(port)
	return URI{ID: h.pc.ID, Name: h.pc.Name, Hosts: []string{host}, Port: p, Pin: h.l.Pin(), Secret: h.l.Secret()}
}

func (h *harness) wait(t *testing.T) (Paired, error) {
	t.Helper()
	select {
	case <-h.done:
		return h.res, h.err
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not stop")
	}
	panic("unreachable")
}

func testPhone(t *testing.T) PhoneInfo {
	key, _ := security.GenerateKey()
	pin, _ := security.PublicKeyPin(key.Public())
	return PhoneInfo{ID: "635f3a01db1859bc", Name: "Melker's S25 Ultra", Pin: pin, Port: 8765, Addresses: []string{"192.168.1.153"}}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return c
}

func wantCode(t *testing.T, err error, code protocol.ErrorCode) {
	t.Helper()
	got, ok := protocol.ErrorCodeOf(err)
	if !ok || got != code {
		t.Fatalf("want error %v, got %v", code, err)
	}
	var e *protocol.Error
	errors.As(err, &e)
	if e.Data == nil || e.Data.Name != code.Name() {
		t.Errorf("data %+v, want name %q", e.Data, code.Name())
	}
}

func TestURIRoundTrip(t *testing.T) {
	u := URI{ID: "9d1c", Name: "Melker's PC & co/1", Hosts: []string{"192.168.1.20", "fd00::1"}, Port: 40123,
		Pin: "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg", Secret: security.NewSecret()}
	s := u.String()
	if !strings.HasPrefix(s, "phonetopc://pair?v=1&id=9d1c&n=Melker%27s%20PC%20%26%20co%2F1&h=192.168.1.20,fd00::1&p=40123&pin=sha256/") {
		t.Errorf("uri %s", s)
	}
	back, err := ParseURI(s)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != u.Name || back.ID != u.ID || back.Port != u.Port || back.Pin != u.Pin || back.Secret != u.Secret ||
		strings.Join(back.Hosts, ",") != strings.Join(u.Hosts, ",") {
		t.Errorf("round trip %+v", back)
	}
	for _, bad := range []string{
		"http://pair?v=1", "phonetopc://pair?v=2&id=a&h=x&p=1&pin=" + u.Pin + "&s=AAAA",
		"phonetopc://pair?v=1&id=a&h=x&p=0&pin=" + u.Pin + "&s=AAAA",
		"phonetopc://pair?v=1&id=a&h=&p=1&pin=" + u.Pin + "&s=AAAA",
		"phonetopc://pair?v=1&id=a&h=x&p=1&pin=sha256/x&s=AAAA",
		"phonetopc://pair?v=1&id=a&h=x&p=1&pin=" + u.Pin + "&s=",
	} {
		if _, err := ParseURI(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// "+" in the name decodes as a space too (form encoding).
	if u2, err := ParseURI(strings.Replace(s, "%20", "+", 1)); err != nil || u2.Name != u.Name {
		t.Errorf("plus: %v %q", err, u2.Name)
	}
}

func TestPairQR(t *testing.T) {
	var asked PhoneInfo
	p := &prompter{qr: func(ph PhoneInfo) bool { asked = ph; return true }}
	h := startListener(t, p, nil)
	phone := testPhone(t)
	token := security.NewToken()
	pc, err := PairQR(ctx(t), h.uri(), phone, token)
	if err != nil {
		t.Fatal(err)
	}
	if pc.ID != h.pc.ID || pc.Name != h.pc.Name || pc.Pin != h.l.Pin() || pc.Token != token {
		t.Errorf("phone stored %+v", pc)
	}
	if asked.Name != phone.Name {
		t.Errorf("prompt for %+v", asked)
	}
	res, err := h.wait(t)
	if err != nil || res.Token != token || res.Phone.Pin != phone.Pin || res.Way != WayQR || res.Phone.Addresses[0] != "192.168.1.153" {
		t.Errorf("listener: %+v %v", res, err)
	}
	if s := <-h.saved; s.Token != token {
		t.Errorf("saved %+v", s)
	}
}

func TestPairQRWrongPin(t *testing.T) {
	h := startListener(t, yes(), nil)
	u := h.uri()
	u.Pin = "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg"
	_, err := PairQR(ctx(t), u, testPhone(t), security.NewToken())
	if !errors.Is(err, security.ErrPinMismatch) {
		t.Fatalf("want pin mismatch, got %v", err)
	}
}

func TestPairQRBadSecretAndRejected(t *testing.T) {
	reject := true
	p := &prompter{qr: func(PhoneInfo) bool { return !reject }}
	h := startListener(t, p, nil)
	u := h.uri()
	good := u.Secret
	u.Secret = security.NewSecret()
	_, err := PairQR(ctx(t), u, testPhone(t), security.NewToken())
	wantCode(t, err, protocol.CodeBadSecret)

	u.Secret = good
	_, err = PairQR(ctx(t), u, testPhone(t), security.NewToken())
	wantCode(t, err, protocol.CodeRejected)

	// The listener stays open after both failures.
	reject = false
	if _, err := PairQR(ctx(t), u, testPhone(t), security.NewToken()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimit(t *testing.T) {
	h := startListener(t, yes(), func(o *Options) { o.MaxFailures = 3 })
	u := h.uri()
	good := u.Secret
	u.Secret = security.NewSecret()
	for range 3 {
		_, err := PairQR(ctx(t), u, testPhone(t), security.NewToken())
		wantCode(t, err, protocol.CodeBadSecret)
	}
	// Now even the right secret is refused: the connection is closed with
	// 1013 before anything is answered.
	u.Secret = good
	_, err := PairQR(ctx(t), u, testPhone(t), security.NewToken())
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("want close 1013, got %v", err)
	}
}

func TestRateLimitIsPerAddress(t *testing.T) {
	l, err := New(Options{Cert: testCert(t), Prompt: yes(), Save: func(Paired) error { return nil }, MaxFailures: 2})
	if err != nil {
		t.Fatal(err)
	}
	l.fail("192.0.2.1")
	l.fail("192.0.2.1")
	if !l.blocked("192.0.2.1") || l.blocked("192.0.2.2") {
		t.Error("the limit must apply per source address")
	}
}

func TestPairSAS(t *testing.T) {
	p := yes()
	h := startListener(t, p, nil)
	phone := testPhone(t)
	token := security.NewToken()
	var phoneCode string
	pc, err := PairSAS(ctx(t), h.addr, phone, token, func(_ context.Context, pc protocol.PCInfo, code string) (bool, error) {
		if pc != h.pc {
			t.Errorf("pc %+v", pc)
		}
		phoneCode = code
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if phoneCode == "" || phoneCode != p.lastCode() || len(phoneCode) != 6 {
		t.Errorf("codes: phone %q, PC %q", phoneCode, p.lastCode())
	}
	if pc.Pin != h.l.Pin() || pc.Token != token {
		t.Errorf("phone stored %+v", pc)
	}
	res, err := h.wait(t)
	if err != nil || res.Way != WaySAS || res.Token != token {
		t.Errorf("listener: %+v %v", res, err)
	}
}

func TestPairSASMismatch(t *testing.T) {
	// The PC user says no.
	pcSays := false
	p := &prompter{qr: func(PhoneInfo) bool { return true }, sas: func(string) bool { return pcSays }}
	h := startListener(t, p, nil)
	ok := func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil }
	_, err := PairSAS(ctx(t), h.addr, testPhone(t), security.NewToken(), ok)
	if !errors.Is(err, ErrCodesDiffer) {
		t.Fatalf("PC says no: %v", err)
	}
	// The phone user says no.
	pcSays = true
	no := func(context.Context, protocol.PCInfo, string) (bool, error) { return false, nil }
	if _, err := PairSAS(ctx(t), h.addr, testPhone(t), security.NewToken(), no); !errors.Is(err, ErrCodesDiffer) {
		t.Fatalf("phone says no: %v", err)
	}
	select {
	case s := <-h.saved:
		t.Fatalf("saved %+v", s)
	default:
	}
	// Still open; a correct pairing works.
	if _, err := PairSAS(ctx(t), h.addr, testPhone(t), security.NewToken(), ok); err != nil {
		t.Fatal(err)
	}
}

// TestPairSASManInTheMiddle relays a SAS pairing through an attacker with
// its own certificate: the codes the two sides compute differ.
func TestPairSASManInTheMiddle(t *testing.T) {
	p := &prompter{qr: func(PhoneInfo) bool { return true }, sas: func(string) bool { return false }}
	h := startListener(t, p, nil)

	key, _ := security.GenerateKey()
	cert, _ := security.SelfSigned(key, "mitm")
	mitm := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		up, err := dial(r.Context(), h.addr, "")
		if err != nil {
			return
		}
		defer up.ws.CloseNow()
		pipe := func(from, to *websocket.Conn) {
			for {
				typ, data, err := from.Read(context.Background())
				if err != nil {
					to.CloseNow()
					return
				}
				to.Write(context.Background(), typ, data)
			}
		}
		go pipe(up.ws, down)
		pipe(down, up.ws)
	}))
	mitm.TLS = security.ServerTLSConfig(cert)
	mitm.StartTLS()
	defer mitm.Close()

	var phoneCode string
	_, err := PairSAS(ctx(t), strings.TrimPrefix(mitm.URL, "https://"), testPhone(t), security.NewToken(),
		func(_ context.Context, _ protocol.PCInfo, code string) (bool, error) {
			phoneCode = code
			return phoneCode == p.lastCode(), nil // the user compares
		})
	if !errors.Is(err, ErrCodesDiffer) {
		t.Fatalf("want codes to differ, got %v", err)
	}
	if phoneCode == p.lastCode() {
		t.Fatalf("codes match through a MITM: %s", phoneCode)
	}
}

// raw drives the listener by hand.
type raw struct {
	t  *testing.T
	ws *websocket.Conn
	id int
}

func dialRaw(t *testing.T, h *harness) *raw {
	c, err := dial(ctx(t), h.addr, h.l.Pin())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.ws.CloseNow() })
	return &raw{t: t, ws: c.ws}
}

func (r *raw) call(method string, params any) *protocol.Message {
	r.t.Helper()
	r.id++
	b, _ := json.Marshal(protocol.NewRequest(protocol.IntID(int64(r.id)), method, params))
	return r.send(string(b))
}

func (r *raw) send(frame string) *protocol.Message {
	r.t.Helper()
	if err := r.ws.Write(ctx(r.t), websocket.MessageText, []byte(frame)); err != nil {
		r.t.Fatal(err)
	}
	_, data, err := r.ws.Read(ctx(r.t))
	if err != nil {
		r.t.Fatal(err)
	}
	var m protocol.Message
	if err := json.Unmarshal(data, &m); err != nil {
		r.t.Fatal(err)
	}
	return &m
}

func wantErr(t *testing.T, m *protocol.Message, code protocol.ErrorCode) {
	t.Helper()
	if m.Error == nil {
		t.Fatalf("want %v, got result %s", code, m.Result)
	}
	wantCode(t, m.Error, code)
}

func TestOutOfOrderAndInvalid(t *testing.T) {
	h := startListener(t, yes(), nil)
	r := dialRaw(t, h)
	phone := testPhone(t)
	wantErr(t, r.call(MethodFinish, FinishParams{Token: security.NewToken()}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodSASNonce, NonceMessage{Nonce: security.B64(security.RandomBytes(32))}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodSASConfirm, map[string]bool{"match": true}), protocol.CodeOutOfOrder)
	wantErr(t, r.call("hello", nil), protocol.CodeMethodNotFound)
	wantErr(t, r.send("nope"), protocol.CodeParseError)
	wantErr(t, r.call(MethodQR, QRParams{Protocol: "phonetopc-pair/9", Secret: security.NewSecret(), Phone: phone}), protocol.CodeUnsupportedProtocol)
	// Step 7 before step 8: a wrongly sized secret is -32602 even with an
	// unknown protocol, and it is not a failed attempt.
	wantErr(t, r.call(MethodQR, QRParams{Protocol: "phonetopc-pair/9", Secret: "x", Phone: phone}), protocol.CodeInvalidParams)
	wantErr(t, r.call(MethodQR, QRParams{Protocol: Protocol, Secret: security.B64(security.RandomBytes(15)), Phone: phone}), protocol.CodeInvalidParams)
	m := r.call(MethodSASBegin, SASBeginParams{Protocol: "phonetopc-pair/9", Phone: phone})
	wantErr(t, m, protocol.CodeUnsupportedProtocol)
	if m.Error.Data == nil || len(m.Error.Data.Supported) != 1 || m.Error.Data.Supported[0] != Protocol {
		t.Errorf("supported %+v", m.Error.Data)
	}
	bad := phone
	bad.Pin = "sha256/short"
	wantErr(t, r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: bad}), protocol.CodeInvalidParams)
	wantErr(t, r.call(MethodQR, []int{1}), protocol.CodeInvalidParams)

	// Begin SAS, then the QR way and a second begin are out of order.
	m = r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: phone})
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	wantErr(t, r.call(MethodQR, QRParams{Protocol: Protocol, Secret: h.l.Secret(), Phone: phone}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: phone}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodFinish, FinishParams{Token: security.NewToken()}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodSASNonce, NonceMessage{Nonce: "short"}), protocol.CodeInvalidParams)
	if m := r.call(MethodSASNonce, NonceMessage{Nonce: security.B64(security.RandomBytes(32))}); m.Error != nil {
		t.Fatal(m.Error)
	}
	wantErr(t, r.call(MethodFinish, FinishParams{Token: security.NewToken()}), protocol.CodeOutOfOrder)
	wantErr(t, r.call(MethodSASConfirm, map[string]any{}), protocol.CodeInvalidParams)
	if m := r.call(MethodSASConfirm, map[string]bool{"match": true}); m.Error != nil {
		t.Fatal(m.Error)
	}
	wantErr(t, r.call(MethodFinish, FinishParams{Token: "too-short"}), protocol.CodeInvalidParams)
	if m := r.call(MethodFinish, FinishParams{Token: security.NewToken()}); m.Error != nil {
		t.Fatal(m.Error)
	}
	// The listener closes normally after pair_finish.
	if _, _, err := r.ws.Read(ctx(t)); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("want close 1000, got %v", err)
	}
	if _, err := h.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestExpired(t *testing.T) {
	// The PC user never answers; the listener expires meanwhile.
	h := startListener(t, &prompter{}, func(o *Options) { o.Expiry = 500 * time.Millisecond })
	_, err := PairQR(ctx(t), h.uri(), testPhone(t), security.NewToken())
	wantCode(t, err, protocol.CodeExpired)
	if _, err := h.wait(t); !errors.Is(err, ErrExpired) {
		t.Fatalf("Serve: %v", err)
	}
}

func TestOnePairingAtATime(t *testing.T) {
	h := startListener(t, &prompter{}, nil) // prompts never answer
	first := dialRaw(t, h)
	b, _ := json.Marshal(protocol.NewRequest(protocol.IntID(1), MethodQR, QRParams{Protocol: Protocol, Secret: h.l.Secret(), Phone: testPhone(t)}))
	if err := first.ws.Write(ctx(t), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	_, err := PairQR(ctx(t), h.uri(), testPhone(t), security.NewToken())
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("second phone: %v", err)
	}
}

func TestPhoneIDFormat(t *testing.T) {
	h := startListener(t, yes(), nil)
	r := dialRaw(t, h)
	phone := testPhone(t)
	for _, id := range []string{"", "635f3a01db1859b", "635F3A01DB1859BC", "635f3a01db1859bg", "../../etc/passwd", "635f3a01db1859bc\n", strings.Repeat("a", 65)} {
		bad := phone
		bad.ID = id
		wantErr(t, r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: bad}), protocol.CodeInvalidParams)
	}
	for _, id := range []string{"635f3a01db1859bc", strings.Repeat("0", 64), security.NewID()} {
		if !ValidID(id) {
			t.Errorf("%q refused", id)
		}
	}
}

// idle waits until no session holds the pairing slot and n failed SAS runs
// have been counted.
func (h *harness) idle(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		h.l.mu.Lock()
		busy, runs := h.l.busy, h.l.sasRuns
		h.l.mu.Unlock()
		if !busy && runs == n {
			return
		}
	}
	t.Fatalf("listener not idle with %d failed SAS runs", n)
}

// TestSASRunLimit: every SAS run that revealed Npc and didn't pair counts,
// from any address and however it ended; the third stops the listener.
func TestSASRunLimit(t *testing.T) {
	h := startListener(t, yes(), nil)
	phone := testPhone(t)
	nonce := NonceMessage{Nonce: security.B64(security.RandomBytes(32))}

	// The phone disconnects after seeing Npc.
	r := dialRaw(t, h)
	if m := r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: phone}); m.Error != nil {
		t.Fatal(m.Error)
	}
	if m := r.call(MethodSASNonce, nonce); m.Error != nil {
		t.Fatal(m.Error)
	}
	r.ws.Close(websocket.StatusNormalClosure, "")
	h.idle(t, 1)

	// A run abandoned before Npc doesn't count.
	r = dialRaw(t, h)
	if m := r.call(MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: phone}); m.Error != nil {
		t.Fatal(m.Error)
	}
	r.ws.Close(websocket.StatusNormalClosure, "")
	h.idle(t, 1)

	// Two mismatches; the listener stops after the second.
	no := func(context.Context, protocol.PCInfo, string) (bool, error) { return false, nil }
	if _, err := PairSAS(ctx(t), h.addr, phone, security.NewToken(), no); !errors.Is(err, ErrCodesDiffer) {
		t.Fatalf("second run: %v", err)
	}
	h.idle(t, 2)
	if _, err := PairSAS(ctx(t), h.addr, phone, security.NewToken(), no); !errors.Is(err, ErrCodesDiffer) {
		t.Fatalf("third run: %v", err)
	}
	if _, err := h.wait(t); !errors.Is(err, ErrSASLimit) {
		t.Fatalf("Serve: %v", err)
	}
}

func TestNoSAS(t *testing.T) {
	h := startListener(t, yes(), func(o *Options) { o.NoSAS = true })
	ok := func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil }
	_, err := PairSAS(ctx(t), h.addr, testPhone(t), security.NewToken(), ok)
	wantCode(t, err, protocol.CodeRejected)
	// The QR code still works.
	if _, err := PairQR(ctx(t), h.uri(), testPhone(t), security.NewToken()); err != nil {
		t.Fatal(err)
	}
	if res, err := h.wait(t); err != nil || res.Way != WayQR {
		t.Fatalf("listener: %+v %v", res, err)
	}
}
