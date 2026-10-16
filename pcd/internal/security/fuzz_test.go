package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// vectorFields reads the string members of a vectors file.
func vectorFields(f *testing.F, name string) map[string]string {
	b, err := os.ReadFile(filepath.Join(vectorsDir, name))
	if err != nil {
		f.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		f.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func FuzzDecodeB64(f *testing.F) {
	for _, s := range []string{"", "-_8A", "AAECAwQFBgcICQoLDA0ODw", "AAE=", "AAF", "A", "+/8A"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := DecodeB64(s)
		if err != nil {
			return
		}
		if B64(b) != s {
			t.Fatalf("%q decodes to %x, which encodes as %q", s, b, B64(b))
		}
	})
}

func FuzzValidPinAndToken(f *testing.F) {
	f.Add("sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg")
	f.Add("sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgh")
	f.Add(NewToken())
	f.Fuzz(func(t *testing.T, s string) {
		if ValidPin(s) && len(s) != len(PinPrefix)+43 {
			t.Fatalf("pin %q has the wrong length", s)
		}
		if ValidToken(s) {
			if b, err := DecodeB64(s); err != nil || len(b) != TokenSize {
				t.Fatalf("token %q is not %d bytes", s, TokenSize)
			}
		}
	})
}

func FuzzParseApprovalKey(f *testing.F) {
	v := vectorFields(f, "approval.json")
	f.Add(v["key"])
	f.Add(vectorFields(f, "decrypt.json")["key"])
	f.Add("MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE")
	f.Fuzz(func(t *testing.T, key string) {
		pub, err := ParseApprovalKey(key)
		if err != nil {
			return
		}
		again, pin, err := EncodeApprovalKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		if again != key {
			t.Fatalf("key %q is also encoded as %q", key, again)
		}
		if p, err := ApprovalKeyPin(key); err != nil || p != pin {
			t.Fatalf("pin %q, want %q (%v)", p, pin, err)
		}
	})
}

func FuzzParseDecryptKey(f *testing.F) {
	f.Add(vectorFields(f, "decrypt.json")["key"])
	f.Add(vectorFields(f, "approval.json")["key"])
	f.Fuzz(func(t *testing.T, key string) {
		pub, der, err := ParseDecryptKey(key)
		if err != nil {
			return
		}
		if bits := pub.N.BitLen(); bits < 2048 || bits > 4096 {
			t.Fatalf("accepted a %d-bit key", bits)
		}
		again, pin, err := EncodeDecryptKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		if again != key || pin != PinSPKI(der) {
			t.Fatalf("key %q is also encoded as %q", key, again)
		}
	})
}

// A signature verifies only for the message that was signed.
func FuzzVerifyApproval(f *testing.F) {
	v := vectorFields(f, "approval.json")
	pub, err := ParseApprovalKey(v["key"])
	if err != nil {
		f.Fatal(err)
	}
	var valid struct {
		Valid struct {
			PhoneID   string `json:"phone_id"`
			PCID      string `json:"pc_id"`
			Nonce     string `json:"nonce"`
			Reason    string `json:"reason"`
			Signature string `json:"signature"`
		} `json:"valid"`
	}
	b, err := os.ReadFile(filepath.Join(vectorsDir, "approval.json"))
	if err != nil {
		f.Fatal(err)
	}
	if err := json.Unmarshal(b, &valid); err != nil {
		f.Fatal(err)
	}
	vv := valid.Valid
	signed := string(ApprovalMessage(vv.PhoneID, vv.PCID, vv.Nonce, vv.Reason))
	f.Add(vv.Signature, signed)
	f.Add(vv.Signature, signed+"\n")
	f.Add(vv.Signature+"A", signed)
	f.Fuzz(func(t *testing.T, sig, msg string) {
		if VerifyApproval(pub, sig, []byte(msg)) == nil && (msg != signed || sig != vv.Signature) {
			t.Fatalf("signature %q verifies for %q", sig, msg)
		}
	})
}

func FuzzParseKeyPEM(f *testing.F) {
	for _, name := range []string{"pc-key.pem", "phone-key.pem", "approval-key.pem", "decrypt-key.pem", "pc-cert.pem"} {
		b, err := os.ReadFile(filepath.Join(vectorsDir, name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if cert, err := LoadCertPEM(data); err == nil {
			_ = Pin(cert)
		}
		key, err := ParseKeyPEM(data)
		if err != nil {
			return
		}
		pemBytes, err := EncodeKeyPEM(key)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseKeyPEM(pemBytes)
		if err != nil || !again.Equal(key) {
			t.Fatalf("key doesn't survive encoding: %v", err)
		}
	})
}
