package mockphone_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Number matching, program names, expiry and the cool-down (APPROVAL.md
// §4, §4.2, §4.3).

func ask(reason string, timeout time.Duration) client.ApprovalRequest {
	return client.ApprovalRequest{Reason: reason, Program: "mock test", Timeout: timeout, ShowMatch: func(int) {}}
}

func askDecrypt(reason, pin string, ct []byte, timeout time.Duration) client.DecryptRequest {
	return client.DecryptRequest{Reason: reason, Program: "mock test", KeyPin: pin, Ciphertext: ct, Timeout: timeout, ShowMatch: func(int) {}}
}

// approvalParams are valid request_approval params with nonce and an
// expiry by the real clock, changed by with (nil removes a member).
func approvalParams(nonce string, with map[string]any) map[string]any {
	p := map[string]any{
		"reason": "Deploy?", "program": "deploy.sh", "nonce": nonce, "match": 42,
		"expiry": time.Now().Add(40 * time.Second).UnixMilli(),
	}
	for k, v := range with {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	return p
}

// clock is a settable phone clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// rawHello opens a raw secure session as the PC with token.
func rawHello(t *testing.T, m *secureMock, token string) *rawConn {
	t.Helper()
	r := dialRawSecure(t, m)
	r.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"` + token + `"}}}`)
	if resp := r.response(); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	return r
}

func TestApprovalChoices(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []mockphone.ApprovalRequest
	)
	m := startApproval(t, func(_ context.Context, req mockphone.ApprovalRequest) protocol.Why {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, req)
		return ""
	}, nil)
	c := m.dialApproval(t)
	positions := map[int]bool{}
	for range 40 {
		res, err := c.RequestApproval(testCtx(t), ask("Deploy?", 0))
		if err != nil || !res.Approved() {
			t.Fatalf("%+v %v", res, err)
		}
		mu.Lock()
		req := seen[len(seen)-1]
		mu.Unlock()
		ch := req.Choices
		if req.Match != res.Match || ch[0] == ch[1] || ch[1] == ch[2] || ch[0] == ch[2] {
			t.Fatalf("choices %v for %d", ch, res.Match)
		}
		for i, n := range ch {
			if n < protocol.MinMatch || n > protocol.MaxMatch {
				t.Fatalf("choice %d out of range", n)
			}
			if n == res.Match {
				positions[i] = true
			}
		}
	}
	if len(positions) != 3 {
		t.Errorf("the right number was only ever at %v", positions)
	}
}

func TestApprovalParamsMatchProgramExpiry(t *testing.T) {
	now := time.Now()
	asked := 0
	m := startApproval(t, func(context.Context, mockphone.ApprovalRequest) protocol.Why { asked++; return "" }, func(o *mockphone.Options) {
		o.Now = func() time.Time { return now }
	})
	r := rawHello(t, m.secureMock, m.pc.Token)
	nonce := security.B64(security.RandomBytes(32))
	ms := now.UnixMilli()
	minute := int64(60000)
	for name, with := range map[string]map[string]any{
		"no program":             {"program": nil},
		"empty program":          {"program": ""},
		"program 61":             {"program": strings.Repeat("p", 61)},
		"program padded":         {"program": "deploy.sh "},
		"program with newline":   {"program": "deploy\nsh"},
		"program bidi":           {"program": "deploy\u202esh"},
		"no match":               {"match": nil},
		"match 9":                {"match": 9},
		"match 100":              {"match": 100},
		"match string":           {"match": "42"},
		"match fraction":         {"match": 42.5},
		"no expiry":              {"expiry": nil},
		"expiry string":          {"expiry": "1760000040000"},
		"expiry too old":         {"expiry": ms - 5*minute - 1},
		"expiry too late":        {"expiry": ms + 30000 + minute + 5*minute + 1},
		"expiry too late, 5 s":   {"expiry": ms + 5000 + minute + 5*minute + 1, "timeout_ms": 5000},
		"expiry far in the past": {"expiry": 0},
	} {
		resp := r.call(protocol.MethodRequestApproval, approvalParams(nonce, with))
		if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
			t.Errorf("%s: %s %+v", name, resp.Result, resp.Error)
		}
	}
	if asked != 0 {
		t.Fatalf("the user was asked %d times about invalid requests", asked)
	}
	for name, with := range map[string]map[string]any{
		"program 60":           {"program": strings.Repeat("p", 60)},
		"match 10":             {"match": 10},
		"match 99":             {"match": 99},
		"oldest expiry":        {"expiry": ms - 5*minute},
		"latest expiry":        {"expiry": ms + 30000 + minute + 5*minute},
		"latest expiry, 2 min": {"expiry": ms + 120000 + minute + 5*minute, "timeout_ms": 120000},
	} {
		resp := r.call(protocol.MethodRequestApproval, approvalParams(nonce, with))
		if resp.Error != nil || !strings.Contains(string(resp.Result), `"approved":true`) {
			t.Errorf("%s: %s %+v", name, resp.Result, resp.Error)
		}
	}
}

func TestApprovalCooldown(t *testing.T) {
	dkey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, dpin, _ := security.EncodeDecryptKey(&dkey.PublicKey)
	ct, _ := security.EncryptToPhone(&dkey.PublicKey, []byte("file key 16 byte"))
	clk := &clock{now: time.Now()}
	t0 := clk.Now()
	var (
		mu    sync.Mutex
		next  protocol.Why
		asked []string
	)
	m := startApproval(t, func(_ context.Context, req mockphone.ApprovalRequest) protocol.Why {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, req.PC.ID)
		return next
	}, func(o *mockphone.Options) {
		o.Now = clk.Now
		o.DecryptKey = func() *rsa.PrivateKey { return dkey }
	})
	pc2 := mockphone.PairedPC{ID: "pc-2", Name: "pc-2", Pin: m.id.Pin, Token: security.NewToken()}
	if err := m.state.PutPC(pc2); err != nil {
		t.Fatal(err)
	}
	r, r2 := rawHello(t, m.secureMock, m.pc.Token), rawHello(t, m.secureMock, pc2.Token)

	// try asks at t0 + at, the user answering why, and checks the outcome.
	try := func(r *rawConn, decrypt bool, at time.Duration, why protocol.Why, want string) {
		t.Helper()
		clk.set(t0.Add(at))
		mu.Lock()
		next = why
		before := len(asked)
		mu.Unlock()
		var resp *protocol.Message
		if decrypt {
			resp = r.call(protocol.MethodDecrypt, map[string]any{
				"reason": "Decrypt a.age", "program": "age", "match": 42, "key_pin": dpin, "ciphertext": security.B64(ct)})
		} else {
			resp = r.call(protocol.MethodRequestApproval, approvalParams(security.B64(security.RandomBytes(32)),
				map[string]any{"expiry": t0.Add(at + 40*time.Second).UnixMilli()}))
		}
		mu.Lock()
		shown := len(asked) > before
		mu.Unlock()
		if resp.Error != nil || !strings.Contains(string(resp.Result), want) {
			t.Fatalf("at %v: %s %+v, want %s", at, resp.Result, resp.Error, want)
		}
		if shown == strings.Contains(want, "cooldown") {
			t.Fatalf("at %v: shown %v for %s", at, shown, resp.Result)
		}
	}
	denied, mismatch, cancelled := `"why":"denied"`, `"why":"mismatch"`, `"why":"cancelled"`
	cooldown, approved := `"why":"cooldown"`, `"approved":true`

	try(r, false, 0, protocol.WhyDenied, denied)
	try(r, false, time.Minute, protocol.WhyMismatch, mismatch)
	// Approvals and other outcomes neither count nor reset the count.
	try(r, false, 2*time.Minute, "", approved)
	try(r, false, 2*time.Minute, protocol.WhyLockout, `"why":"lockout"`)
	try(r, false, 2*time.Minute, protocol.WhyTimeout, `"why":"timeout"`)
	// A decryption's refusal counts too, and is the third.
	try(r, true, 3*time.Minute, protocol.WhyCancelled, cancelled)
	try(r, false, 3*time.Minute, "", cooldown)
	try(r, true, 3*time.Minute, "", cooldown)
	// Other PCs are not affected.
	try(r2, false, 3*time.Minute, "", approved)
	// The cool-down lasts 5 minutes, and isn't extended by the requests
	// it refuses.
	try(r, false, 8*time.Minute-time.Millisecond, "", cooldown)
	try(r, false, 8*time.Minute, "", approved)

	// The refusals before the cool-down were forgotten; refusals count
	// within 10 minutes only.
	try(r, false, 9*time.Minute, protocol.WhyDenied, denied)
	try(r, false, 13*time.Minute, protocol.WhyDenied, denied)
	try(r, false, 19*time.Minute, protocol.WhyDenied, denied)
	try(r, false, 19*time.Minute, "", approved)
	try(r, false, 20*time.Minute, protocol.WhyDenied, denied)
	try(r, false, 20*time.Minute, "", cooldown)
}

func TestApprovalCooldownThroughClient(t *testing.T) {
	m := startApproval(t, mockphone.Answer(protocol.WhyMismatch), nil)
	c := m.dialApproval(t)
	for i := range protocol.CooldownRefusals {
		if res, err := c.RequestApproval(testCtx(t), ask("Deploy?", 0)); err != nil || res.Why != client.WhyMismatch {
			t.Fatalf("request %d: %+v %v", i, res, err)
		}
	}
	res, err := c.RequestApproval(testCtx(t), ask("Deploy?", 0))
	if err != nil || res.Why != client.WhyCooldown || !strings.Contains(res.String(), "5 minutes") {
		t.Errorf("%+v %v", res, err)
	}
	if !slices.Contains(protocol.Whys, protocol.WhyCooldown) || !slices.Contains(protocol.Whys, protocol.WhyMismatch) {
		t.Error("Whys lacks the new outcomes")
	}
}
