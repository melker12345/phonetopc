package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

const fuzzPin = "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg"

func FuzzParseURI(f *testing.F) {
	u := URI{ID: "9d1c", Name: "Melker's PC & co/1", Hosts: []string{"192.168.1.20", "fd00::1"}, Port: 40123, Pin: fuzzPin, Secret: "AAECAwQFBgcICQoLDA0ODw"}
	f.Add(u.String())
	f.Add("phonetopc://pair?v=1&id=a&n=PC+1&h=%5Bfd00::1%5D%2C10.0.0.2&p=8766&pin=" + fuzzPin + "&s=AAAA")
	f.Add(" phonetopc://pair?h=[fe80::1%25eth0],,x&v=1&id=a&p=+1&pin=" + fuzzPin + "&s=AA&s=BB")
	f.Fuzz(func(t *testing.T, s string) {
		u, err := ParseURI(s)
		if err != nil {
			return
		}
		if u.Port < 1 || u.Port > 65535 || u.ID == "" || len(u.Hosts) == 0 || !security.ValidPin(u.Pin) {
			t.Fatalf("accepted %q as %+v", s, u)
		}
		for _, h := range u.Hosts {
			if h == "" {
				t.Fatalf("empty host in %+v", u)
			}
		}
		back, err := ParseURI(u.String())
		if err != nil {
			t.Fatalf("%q parsed as %+v, which renders as %q and is rejected: %v", s, u, u.String(), err)
		}
		if !reflect.DeepEqual(back, u) {
			t.Fatalf("%q parsed as %+v, which renders as %q and parses as %+v", s, u, u.String(), back)
		}
	})
}

// fuzzSecret is the QR secret of the listener FuzzSession talks to.
var fuzzSecret = security.B64(make([]byte, security.SecretSize))

// FuzzSession feeds a pairing listener's session a sequence of frames, one
// per line, as a phone would send them. Whatever the frames, a pairing is
// stored at most once, only with valid phone info and token, and only after
// the right steps.
func FuzzSession(f *testing.F) {
	key, err := security.GenerateKey()
	if err != nil {
		f.Fatal(err)
	}
	cert, err := security.SelfSigned(key, "pc")
	if err != nil {
		f.Fatal(err)
	}
	phone := `{"id":"635f3a01db1859bc","name":"Phone","pin":"` + fuzzPin + `","port":8765,"addresses":["192.168.1.153"]}`
	token := security.B64(make([]byte, security.TokenSize))
	nonce := security.B64(make([]byte, security.NonceSize))
	frame := func(id int, method, params string) string {
		return `{"jsonrpc":"2.0","id":` + string(protocol.IntID(int64(id))) + `,"method":"` + method + `","params":` + params + `}`
	}
	f.Add(strings.Join([]string{
		frame(1, MethodQR, `{"protocol":"`+Protocol+`","secret":"`+fuzzSecret+`","phone":`+phone+`}`),
		frame(2, MethodFinish, `{"token":"`+token+`"}`),
	}, "\n"))
	f.Add(strings.Join([]string{
		frame(1, MethodSASBegin, `{"protocol":"`+Protocol+`","phone":`+phone+`}`),
		frame(2, MethodSASNonce, `{"nonce":"`+nonce+`"}`),
		frame(3, MethodSASConfirm, `{"match":true}`),
		frame(4, MethodFinish, `{"token":"`+token+`"}`),
	}, "\n"))
	f.Add(strings.Join([]string{
		frame(1, MethodQR, `{"protocol":"`+Protocol+`","secret":"AAAAAAAAAAAAAAAAAAAAAB","phone":`+phone+`}`),
		frame(2, MethodSASBegin, `{"protocol":"phonetopc-pair/2","phone":`+phone+`}`),
		frame(3, MethodSASConfirm, `{"match":false}`),
		`{"jsonrpc":"2.0","method":"pair_finish"}`,
		`[1]`,
	}, "\n"))
	f.Fuzz(func(t *testing.T, input string) {
		var saved []Paired
		l, err := New(Options{
			PC: protocol.PCInfo{ID: "9d1c00000000aaaa", Name: "pc"}, Cert: cert, Secret: fuzzSecret,
			Prompt: yes(),
			Save:   func(p Paired) error { saved = append(saved, p); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l.ctx = ctx
		s := &session{l: l, remote: "192.168.1.153:40000", ctx: ctx}
		for _, fr := range strings.Split(input, "\n") {
			before := s.state
			resp, after, closeCode := s.handle([]byte(fr))
			if resp != nil {
				checkResponse(t, []byte(fr), resp)
			}
			if after != nil {
				after()
			}
			if len(saved) > 0 && (before != stConfirmed || s.state != stDone) {
				t.Fatalf("stored a pairing going from state %d to %d", before, s.state)
			}
			if closeCode != 0 {
				break
			}
		}
		if len(saved) > 1 {
			t.Fatalf("stored %d pairings", len(saved))
		}
		for _, p := range saved {
			if !ValidID(p.Phone.ID) || !security.ValidPin(p.Phone.Pin) || !security.ValidToken(p.Token) ||
				p.Phone.Port < 1 || p.Phone.Port > 65535 || protocol.CheckText(p.Phone.Name, 1, maxNameLen, false) != nil ||
				(p.Way != WayQR && p.Way != WaySAS) {
				t.Fatalf("stored invalid pairing %+v", p)
			}
		}
	})
}

// checkResponse checks that a response is well formed and answers the
// request's id, or null.
func checkResponse(t *testing.T, req []byte, resp *protocol.Response) {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("response doesn't encode: %v", err)
	}
	var m protocol.Message
	if err := json.Unmarshal(b, &m); err != nil || m.Kind() != protocol.KindResponse {
		t.Fatalf("response %s is not a JSON-RPC response", b)
	}
	if string(m.ID) == "null" {
		return
	}
	var r struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(req, &r) != nil || !bytes.Equal(compact(t, r.ID), compact(t, m.ID)) {
		t.Fatalf("response %s doesn't answer %s", b, req)
	}
}

func compact(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	return buf.Bytes()
}
