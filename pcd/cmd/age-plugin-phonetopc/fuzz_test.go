package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// testSPKI is the decryption key of spec/conformance/vectors/decrypt.json.
func testSPKI(f *testing.F) []byte {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "spec", "conformance", "vectors", "decrypt.json"))
	if err != nil {
		f.Fatal(err)
	}
	var v struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		f.Fatal(err)
	}
	spki, err := security.DecodeB64(v.Key)
	if err != nil {
		f.Fatal(err)
	}
	return spki
}

func FuzzParseIdentity(f *testing.F) {
	spki := testSPKI(f)
	for _, id := range []string{"635f3a01db1859bc", "x", strings.Repeat("a", 255)} {
		data, err := encodeIdentity(id, spki)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add(append([]byte{3, 0xff, 0xfe, 0xfd}, spki...))
	f.Add([]byte{0})
	f.Add(spki)
	f.Fuzz(func(t *testing.T, data []byte) {
		if r, err := parseRecipient(data); err == nil && !bytes.Equal(r.spki, data) {
			t.Fatal("recipient doesn't keep its key")
		}
		id, r, err := parseIdentity(data)
		if err != nil {
			return
		}
		again, err := encodeIdentity(id, r.spki)
		if err != nil {
			t.Fatalf("identity %q doesn't encode: %v", id, err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("identity %x encodes as %x", data, again)
		}
	})
}

// FuzzUnwrap gives an identity stanzas and a phone that answers with any
// bytes: Unwrap asks the phone only about a stanza for its key, and returns
// a 16-byte file key or an error.
func FuzzUnwrap(f *testing.F) {
	spki := testSPKI(f)
	r, err := parseRecipient(spki)
	if err != nil {
		f.Fatal(err)
	}
	stanzas, err := r.Wrap(make([]byte, fileKeyLen))
	if err != nil {
		f.Fatal(err)
	}
	s := stanzas[0]
	f.Add(s.Type, s.Args[0], s.Body, make([]byte, fileKeyLen))
	f.Add(s.Type, s.Args[0], s.Body[1:], make([]byte, fileKeyLen))
	f.Add(s.Type, s.Args[0], s.Body, []byte("short"))
	f.Add("X25519", "abc", []byte{1, 2, 3}, []byte(nil))
	f.Fuzz(func(t *testing.T, typ, arg string, body, answer []byte) {
		asked := false
		id := &identity{deviceID: "635f3a01db1859bc", recipient: r, ask: func(_ context.Context, deviceID, keyPin string, ct []byte) ([]byte, error) {
			asked = true
			if deviceID != "635f3a01db1859bc" || keyPin != security.PinSPKI(spki) || len(ct) != r.pub.Size() {
				t.Fatalf("asked %q with pin %q about %d bytes", deviceID, keyPin, len(ct))
			}
			return answer, nil
		}}
		key, err := id.Unwrap([]*age.Stanza{{Type: typ, Args: []string{arg}, Body: body}})
		if asked && (typ != stanzaType || arg != r.tag()) {
			t.Fatalf("asked the phone about a %q stanza for %q", typ, arg)
		}
		if err == nil && len(key) != fileKeyLen {
			t.Fatalf("file key of %d bytes", len(key))
		}
	})
}

func FuzzReason(f *testing.F) {
	for _, s := range []string{"", "age -d -i id.txt secrets.age", "sops -d x\x00y", strings.Repeat("ä ", 100), "a\u202Eb", "\xff\xfe", " \t "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		if err := protocol.CheckReason(reason(cmd)); err != nil {
			t.Fatalf("reason(%q) = %q: %v", cmd, reason(cmd), err)
		}
	})
}
