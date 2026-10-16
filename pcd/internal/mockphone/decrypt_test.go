package mockphone_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

func TestDecryptRules(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, pin, _ := security.EncodeDecryptKey(&key.PublicKey)
	var (
		mu      sync.Mutex
		v       = newFrameValidator(t)
		invalid bool // the frames sent now are meant to break the rules
	)
	release := make(chan struct{})
	asked := make(chan mockphone.ApprovalRequest, 4)
	m := startApproval(t, func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
		asked <- req
		select {
		case <-release:
			return ""
		case <-ctx.Done():
			return protocol.WhyTimeout
		}
	}, func(o *mockphone.Options) {
		o.DecryptKey = func() *rsa.PrivateKey { return key }
		o.Tap = func(dir mockphone.Direction, frame []byte) {
			mu.Lock()
			defer mu.Unlock()
			v.check(dir, frame, invalid)
		}
	})
	c := m.dialApproval(t)
	if h := c.Server(); !h.HasCapability(protocol.CapabilityDecrypt) || h.Decrypt == nil || h.Decrypt.Pin != pin {
		t.Fatalf("hello %+v", h)
	}

	ct, _ := security.EncryptToPhone(&key.PublicKey, []byte("file key 16 byte"))
	r := dialRawSecure(t, m.secureMock)
	r.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","auth":{"token":"` + m.pc.Token + `"}}}`)
	if resp := r.response(); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	ok := map[string]any{"reason": "Decrypt a.age", "key_pin": pin, "ciphertext": security.B64(ct)}
	with := func(k string, val any) map[string]any {
		p := map[string]any{}
		for k, v := range ok {
			p[k] = v
		}
		if val == nil {
			delete(p, k)
		} else {
			p[k] = val
		}
		return p
	}
	mu.Lock()
	invalid = true
	mu.Unlock()
	for name, p := range map[string]map[string]any{
		"no reason":      with("reason", nil),
		"padded reason":  with("reason", " Decrypt"),
		"no key_pin":     with("key_pin", nil),
		"no ciphertext":  with("ciphertext", nil),
		"not base64url":  with("ciphertext", "a+b/"),
		"short":          with("ciphertext", security.B64(ct[:200])),
		"timeout":        with("timeout_ms", 10),
		"timeout string": with("timeout_ms", "30000"),
	} {
		resp := r.call(protocol.MethodDecrypt, p)
		if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
			t.Errorf("%s: %s %+v", name, resp.Result, resp.Error)
		}
	}
	mu.Lock()
	invalid = false
	mu.Unlock()
	if resp := r.call(protocol.MethodDecrypt, with("key_pin", "sha256/other")); resp.Error != nil || !strings.Contains(string(resp.Result), `"why":"unavailable"`) {
		t.Errorf("other pin: %s %+v", resp.Result, resp.Error)
	}
	select {
	case req := <-asked:
		t.Fatalf("the user was asked about %+v", req)
	default:
	}

	// Approvals and decryptions share one pending slot.
	done := make(chan []byte, 1)
	go func() {
		res, err := c.Decrypt(testCtx(t), "Decrypt a.age", pin, ct, 30*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- res.Plaintext
	}()
	if req := <-asked; !req.Decrypt || req.Reason != "Decrypt a.age" {
		t.Errorf("the user saw %+v", req)
	}
	if res, err := c.RequestApproval(testCtx(t), "Deploy?", 0); err != nil || res.Why != protocol.WhyBusy {
		t.Errorf("approval while decrypting: %+v %v", res, err)
	}
	close(release)
	if pt := <-done; string(pt) != "file key 16 byte" {
		t.Errorf("plaintext %q", pt)
	}

	// Dev mode: the method doesn't exist.
	_, url := start(t, quietOptions())
	rd := dialRaw(t, url, nil)
	rd.hello()
	wantError(t, rd.call(protocol.MethodDecrypt, ok), protocol.CodeMethodNotFound, "2")
}

func TestDecryptKeyFromState(t *testing.T) {
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k1, err := st.DecryptKey()
	if err != nil {
		t.Fatal(err)
	}
	k1again, _ := st.DecryptKey()
	k2, err := st.RotateDecryptKey()
	if err != nil {
		t.Fatal(err)
	}
	k2again, _ := st.DecryptKey()
	if !k1.Equal(k1again) || k1.Equal(k2) || !k2.Equal(k2again) {
		t.Error("decryption key not persisted or not rotated")
	}
}
