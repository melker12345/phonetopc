package mockphone

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/conformance"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

const (
	fuzzToken   = "fuzz-token"
	fuzzHello   = `{"jsonrpc":"2.0","id":0,"method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"fuzz"},"auth":{"token":"` + fuzzToken + `"}}}`
	fuzzRequest = `{"jsonrpc":"2.0","id":"r","method":"request_approval","params":{"reason":"Deploy?","nonce":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`
)

// FuzzSession feeds one session of the mock phone a sequence of frames, one
// per line, in dev mode or in secure mode (with approval and decryption
// keys). Every response must be well formed and answer its request, and
// nothing but hello may succeed before hello.
func FuzzSession(f *testing.F) {
	frames, err := conformance.Frames("../../../spec/conformance")
	if err != nil {
		f.Fatal(err)
	}
	for _, fr := range frames {
		if !bytes.ContainsRune(fr, '\n') {
			f.Add(fuzzHello+"\n"+string(fr), true)
			f.Add(string(fr), false)
		}
	}
	f.Add(fuzzHello+"\n"+fuzzRequest+"\n"+fuzzRequest, true)

	approvalKey, err := security.GenerateKey()
	if err != nil {
		f.Fatal(err)
	}
	decryptKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, input string, secure bool) {
		opts := Options{
			TapEvery: -1, LightEvery: -1, ProximityEvery: -1, BatteryEvery: -1, LocationEvery: -1, NFCEvery: -1,
			UserEvery: -1, PanelExpiry: time.Millisecond, Seed: 1,
		}
		if secure {
			opts.Authenticate = func(token string) (protocol.PCInfo, bool) {
				return protocol.PCInfo{ID: "9d1c00000000aaaa", Name: "pc"}, token == fuzzToken
			}
			opts.ApprovalKey = func() *ecdsa.PrivateKey { return approvalKey }
			opts.DecryptKey = func() *rsa.PrivateKey { return decryptKey }
		}
		srv := New(opts)
		defer srv.Close()
		s := newSession(srv, nil, "192.168.1.20:40000")
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for {
				select {
				case <-s.out:
				case <-s.ctx.Done():
					return
				}
			}
		}()
		defer func() {
			s.cancel()
			<-drained
			s.stopAll()
			srv.panels.sessionClosed(s)
		}()

		for _, fr := range strings.Split(input, "\n") {
			helloed := s.helloed
			resp, after := s.handle([]byte(fr))
			if resp != nil {
				checkResponse(t, []byte(fr), resp, helloed)
			}
			if after != nil {
				after()
			}
			if s.rejected {
				break
			}
		}
	})
}

func checkResponse(t *testing.T, req []byte, resp *protocol.Response, helloed bool) {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("response doesn't encode: %v", err)
	}
	var m protocol.Message
	if err := json.Unmarshal(b, &m); err != nil || m.Kind() != protocol.KindResponse {
		t.Fatalf("response %s is not a JSON-RPC response", b)
	}
	id, method, _, errResp := protocol.ParseRequest(req)
	if errResp != nil {
		if string(b) != string(mustJSON(errResp)) {
			t.Fatalf("framing error %s, want %s", b, mustJSON(errResp))
		}
		return
	}
	if !sameJSON(id, m.ID) {
		t.Fatalf("response %s doesn't answer %s", b, req)
	}
	if !helloed && method != protocol.MethodHello && (m.Error == nil || m.Error.Code != protocol.CodeHelloRequired) {
		t.Fatalf("%s before hello answered with %s", req, b)
	}
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && string(mustJSON(x)) == string(mustJSON(y))
}
