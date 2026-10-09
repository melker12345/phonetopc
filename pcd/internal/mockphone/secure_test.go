package mockphone_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

type secureMock struct {
	srv   *mockphone.Server
	state *mockphone.State
	id    mockphone.Identity
	addr  string // host:port
	url   string // wss://
	pc    mockphone.PairedPC
}

// startSecure runs a secure mock with a fresh state holding one paired PC.
func startSecure(t *testing.T, tweak func(*mockphone.Options)) *secureMock {
	t.Helper()
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("", "secure mock")
	if err != nil {
		t.Fatal(err)
	}
	pc := mockphone.PairedPC{ID: "pc-1", Name: "arch-desktop", Pin: id.Pin, Token: security.NewToken()}
	if err := st.PutPC(pc); err != nil {
		t.Fatal(err)
	}
	opts := quietOptions()
	opts.Device.ID = id.ID
	opts.Authenticate = st.Authenticate
	if tweak != nil {
		tweak(&opts)
	}
	m := &secureMock{state: st, id: id, pc: pc}
	m.srv, m.addr = serveTLS(t, opts, id.Cert)
	m.url = client.SecureURL(m.addr)
	return m
}

func serveTLS(t *testing.T, opts mockphone.Options, cert tls.Certificate) (*mockphone.Server, string) {
	t.Helper()
	srv := mockphone.New(opts)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = security.ServerTLSConfig(cert)
	hs.StartTLS()
	t.Cleanup(func() { srv.Close(); hs.Close() })
	return srv, strings.TrimPrefix(hs.URL, "https://")
}

func (m *secureMock) dial(t *testing.T, opts ...client.Option) (*client.Client, error) {
	t.Helper()
	return client.Dial(testCtx(t), m.url, append([]client.Option{client.WithPin(m.id.Pin), client.WithToken(m.pc.Token)}, opts...)...)
}

func TestSecureSession(t *testing.T) {
	m := startSecure(t, nil)
	c, err := m.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := c.Server()
	if h.PC == nil || h.PC.ID != "pc-1" || h.PC.Name != "arch-desktop" || h.Device.ID != m.id.ID {
		t.Errorf("hello %+v pc %+v", h, h.PC)
	}
	sub, err := c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 50})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, sub); ev.Source != "gyro" || ev.Seq != 0 {
		t.Errorf("event %+v", ev)
	}
}

func TestSecurePinMismatch(t *testing.T) {
	var frames atomic.Int32
	m := startSecure(t, func(o *mockphone.Options) {
		o.Tap = func(mockphone.Direction, []byte) { frames.Add(1) }
	})
	other := "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg"
	_, err := client.Dial(testCtx(t), m.url, client.WithPin(other), client.WithToken(m.pc.Token))
	var pm *client.PinMismatchError
	if !errors.Is(err, client.ErrPinMismatch) || !errors.As(err, &pm) || pm.Got != m.id.Pin {
		t.Fatalf("want pin mismatch, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := frames.Load(); n != 0 {
		t.Errorf("the server received %d frames despite the pin mismatch", n)
	}
	// No pin, or a pin with plaintext: refused before connecting.
	if _, err := client.Dial(testCtx(t), m.url, client.WithToken(m.pc.Token)); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Errorf("no pin: %v", err)
	}
	if _, err := client.Dial(testCtx(t), "ws://"+m.addr+"/v1/ws", client.WithPin(m.id.Pin)); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("plaintext with pin: %v", err)
	}
}

func TestSecureUnauthorized(t *testing.T) {
	m := startSecure(t, func(o *mockphone.Options) { o.MaxAuthFailures = -1 })
	for _, opts := range [][]client.Option{
		{client.WithPin(m.id.Pin)}, // no token
		{client.WithPin(m.id.Pin), client.WithToken(security.NewToken())},
	} {
		_, err := client.Dial(testCtx(t), m.url, opts...)
		if !client.IsUnauthorized(err) {
			t.Fatalf("want -32010, got %v", err)
		}
		wantCode(t, err, protocol.CodeUnauthorized)
	}
	// On the wire: the error, then close 1008.
	r := dialRawSecure(t, m)
	r.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"nope"}}}`)
	wantError(t, r.mustRead(), protocol.CodeUnauthorized, "1")
	if _, err := r.read(testCtx(t)); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("want close 1008, got %v", err)
	}
}

func dialRawSecure(t *testing.T, m *secureMock) *rawConn {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: security.PinnedClientTLSConfig(m.id.Pin)}}
	return dialRaw(t, m.url, &websocket.DialOptions{HTTPClient: hc})
}

func TestSecureRevokeAndRepair(t *testing.T) {
	m := startSecure(t, nil)
	c, err := m.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Re-pairing the same PC replaces its token: sessions with the old one
	// are closed, the old token no longer works and the new one does.
	newTok := security.NewToken()
	repaired := m.pc
	repaired.Token = newTok
	if err := m.state.PutPC(repaired); err != nil {
		t.Fatal(err)
	}
	if n := m.srv.Recheck(); n != 1 {
		t.Errorf("Recheck closed %d sessions", n)
	}
	select {
	case <-c.Done():
		if websocket.CloseStatus(c.Err()) != websocket.StatusPolicyViolation {
			t.Errorf("closed with %v", c.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session with the old token stayed open")
	}
	if _, err := m.dial(t); !client.IsUnauthorized(err) {
		t.Fatalf("old token: %v", err)
	}
	m.pc = repaired
	c2, err := m.dial(t)
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	defer c2.Close()

	// Revoking closes the session at once with 1008.
	if _, err := m.state.RemovePC("arch-desktop"); err != nil {
		t.Fatal(err)
	}
	m.srv.Recheck()
	select {
	case <-c2.Done():
		if websocket.CloseStatus(c2.Err()) != websocket.StatusPolicyViolation {
			t.Errorf("closed with %v", c2.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked session stayed open")
	}
	if _, err := m.dial(t); !client.IsUnauthorized(err) {
		t.Fatalf("revoked token: %v", err)
	}
}

func TestSecureHelloTimeout(t *testing.T) {
	m := startSecure(t, func(o *mockphone.Options) { o.HelloTimeout = 200 * time.Millisecond })
	r := dialRawSecure(t, m)
	start := time.Now()
	if _, err := r.read(testCtx(t)); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("want close 1008, got %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("closed after %v", d)
	}
	// A successful hello stops the timer.
	c, err := m.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(400 * time.Millisecond)
	if _, err := c.ListSensors(testCtx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestSecureAuthRateLimit(t *testing.T) {
	m := startSecure(t, func(o *mockphone.Options) { o.MaxAuthFailures = 2 })
	for range 2 {
		if _, err := client.Dial(testCtx(t), m.url, client.WithPin(m.id.Pin), client.WithToken("bad")); !client.IsUnauthorized(err) {
			t.Fatalf("want -32010, got %v", err)
		}
	}
	_, err := m.dial(t) // even the right token: this address is blocked for now
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want HTTP 429, got %v", err)
	}
}

// Secure server cases: spec/conformance/secure-server-cases.json.

type secureCases struct {
	Fixture struct {
		Key string `json:"key"`
		Pin string `json:"pin"`
		PCs []struct {
			ID, Name, Token string
		} `json:"pcs"`
		RevokedTokens []string `json:"revoked_tokens"`
		HelloTimeoutS float64  `json:"hello_timeout_s"`
	} `json:"fixture"`
	Cases []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Hello       bool            `json:"hello"`
		Send        json.RawMessage `json:"send"`
		Expect      struct {
			caseExpect
			ResultHas  map[string]json.RawMessage `json:"result_has"`
			CloseAfter *int                       `json:"close_after"`
		} `json:"expect"`
	} `json:"cases"`
}

func TestSecureServerCases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(specDir, "conformance", "secure-server-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc secureCases
	if err := json.Unmarshal(b, &sc); err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(specDir, "conformance", sc.Fixture.Key))
	if err != nil {
		t.Fatal(err)
	}
	key, err := security.ParseKeyPEM(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := security.SelfSigned(key, "conformance")
	if err != nil {
		t.Fatal(err)
	}
	if pin, _ := security.CertPin(cert); pin != sc.Fixture.Pin {
		t.Fatalf("fixture pin %s, key gives %s", sc.Fixture.Pin, pin)
	}
	opts := quietOptions()
	opts.MaxAuthFailures = -1 // the cases fail many hellos from one address
	// The fixture's timeout is 10 s; the runner may shorten it.
	opts.HelloTimeout = time.Second
	opts.Authenticate = func(token string) (protocol.PCInfo, bool) {
		for _, pc := range sc.Fixture.PCs {
			if security.Equal(pc.Token, token) {
				return protocol.PCInfo{ID: pc.ID, Name: pc.Name}, true
			}
		}
		return protocol.PCInfo{}, false // revoked tokens are simply unknown
	}
	_, addr := serveTLS(t, opts, cert)
	url := client.SecureURL(addr)
	hello := `{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"},"auth":{"token":"` + sc.Fixture.PCs[0].Token + `"}}}`

	seen := map[string]bool{}
	for _, c := range sc.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			hc := &http.Client{Transport: &http.Transport{TLSClientConfig: security.PinnedClientTLSConfig(sc.Fixture.Pin)}}
			r := dialRaw(t, url, &websocket.DialOptions{HTTPClient: hc})
			if c.Hello {
				r.sendRaw(hello)
				if m := r.response(); m.Error != nil || string(m.ID) != `"conformance-hello"` {
					t.Fatalf("hello failed: %+v", m.Error)
				}
			}
			ex := c.Expect
			if string(c.Send) == `{"wait": true}` || string(c.Send) == `{"wait":true}` {
				// Send nothing; expect the close.
				ctx, cancel := context.WithTimeout(context.Background(), opts.HelloTimeout+5*time.Second)
				defer cancel()
				_, _, err := r.conn.Read(ctx)
				if ex.Close == nil || int(websocket.CloseStatus(err)) != *ex.Close {
					t.Fatalf("want close %v, got %v", ex.Close, err)
				}
				return
			}
			m := runCaseStep(t, r, "send", caseStep{Send: c.Send, Expect: ex.caseExpect})
			if ex.ResultHas != nil {
				var got map[string]json.RawMessage
				if err := json.Unmarshal(m.Result, &got); err != nil {
					t.Fatal(err)
				}
				for k, want := range ex.ResultHas {
					wantSameJSON(t, "result."+k, got[k], want)
				}
			}
			if ex.CloseAfter != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, data, err := r.conn.Read(ctx)
				if err == nil {
					t.Fatalf("want close %d, got frame %s", *ex.CloseAfter, data)
				}
				if got := int(websocket.CloseStatus(err)); got != *ex.CloseAfter {
					t.Fatalf("want close %d, got %v", *ex.CloseAfter, err)
				}
			}
		})
	}
}
