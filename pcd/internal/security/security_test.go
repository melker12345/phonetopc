package security

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var vectorsDir = filepath.Join("..", "..", "..", "spec", "conformance", "vectors")

type vectors struct {
	Pin []struct {
		Cert string `json:"cert"`
		Pin  string `json:"pin"`
	} `json:"pin"`
	SAS struct {
		PCPin        string `json:"pc_pin"`
		PhonePin     string `json:"phone_pin"`
		NpcB64url    string `json:"npc_b64url"`
		NphoneB64url string `json:"nphone_b64url"`
		Commitment   string `json:"commitment"`
		SHA256Hex    string `json:"sha256_hex"`
		Code         string `json:"code"`
	} `json:"sas"`
	Base64url struct {
		BytesHex string `json:"bytes_hex"`
		Encoded  string `json:"encoded"`
	} `json:"base64url"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorsDir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVectorPins(t *testing.T) {
	v := loadVectors(t)
	if len(v.Pin) != 2 {
		t.Fatalf("want 2 pin vectors, got %d", len(v.Pin))
	}
	for _, p := range v.Pin {
		data, err := os.ReadFile(filepath.Join(vectorsDir, p.Cert))
		if err != nil {
			t.Fatal(err)
		}
		cert, err := LoadCertPEM(data)
		if err != nil {
			t.Fatal(err)
		}
		if got := Pin(cert); got != p.Pin {
			t.Errorf("%s: pin %s, want %s", p.Cert, got, p.Pin)
		}
		if !ValidPin(p.Pin) {
			t.Errorf("ValidPin(%s) = false", p.Pin)
		}
		// The matching key gives the same pin: pins depend on the key only.
		keyData, err := os.ReadFile(filepath.Join(vectorsDir, strings.Replace(p.Cert, "-cert", "-key", 1)))
		if err != nil {
			t.Fatal(err)
		}
		key, err := ParseKeyPEM(keyData)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := PublicKeyPin(key.Public()); got != p.Pin {
			t.Errorf("%s: key pin %s, want %s", p.Cert, got, p.Pin)
		}
		reissued, err := SelfSigned(key, "reissued")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := CertPin(reissued); got != p.Pin {
			t.Errorf("%s: re-issued certificate pin %s, want %s", p.Cert, got, p.Pin)
		}
	}
}

func TestVectorSAS(t *testing.T) {
	s := loadVectors(t).SAS
	npc, err := DecodeB64(s.NpcB64url)
	if err != nil || len(npc) != NonceSize {
		t.Fatalf("npc: %v (%d bytes)", err, len(npc))
	}
	nphone, err := DecodeB64(s.NphoneB64url)
	if err != nil || len(nphone) != NonceSize {
		t.Fatalf("nphone: %v (%d bytes)", err, len(nphone))
	}
	if got := Commitment(npc); got != s.Commitment {
		t.Errorf("commitment %s, want %s", got, s.Commitment)
	}
	if !CheckCommitment(s.Commitment, npc) || CheckCommitment(s.Commitment, nphone) {
		t.Error("CheckCommitment")
	}
	sum := SASHash(s.PCPin, s.PhonePin, npc, nphone)
	if got := hex.EncodeToString(sum[:]); got != s.SHA256Hex {
		t.Errorf("hash %s, want %s", got, s.SHA256Hex)
	}
	if got := SASCode(s.PCPin, s.PhonePin, npc, nphone); got != s.Code {
		t.Errorf("code %s, want %s", got, s.Code)
	}
	// Swapping the pins (each side using the other's view) changes the code.
	if SASCode(s.PhonePin, s.PCPin, npc, nphone) == s.Code {
		t.Error("code does not depend on the order of the pins")
	}
	if got := FormatCode(s.Code); got != "940 411" {
		t.Errorf("FormatCode = %q", got)
	}
}

func TestVectorBase64url(t *testing.T) {
	b := loadVectors(t).Base64url
	raw, _ := hex.DecodeString(b.BytesHex)
	if got := B64(raw); got != b.Encoded {
		t.Errorf("B64 = %s, want %s", got, b.Encoded)
	}
	got, err := DecodeB64(b.Encoded)
	if err != nil || hex.EncodeToString(got) != b.BytesHex {
		t.Errorf("DecodeB64 = %x, %v", got, err)
	}
	for _, bad := range []string{"-_8A=", "+/8A", "-_9", "a", "-_8A\n", "-_\r\n8A", "\n"} {
		if _, err := DecodeB64(bad); err == nil {
			t.Errorf("DecodeB64(%q) accepted", bad)
		}
	}
}

func TestSecrets(t *testing.T) {
	if tok := NewToken(); !ValidToken(tok) || len(tok) != 43 {
		t.Errorf("token %q", tok)
	}
	if ValidToken(NewSecret()) {
		t.Error("a 16-byte secret is not a token")
	}
	if id := NewID(); len(id) != 16 {
		t.Errorf("id %q", id)
	}
	if NewToken() == NewToken() {
		t.Error("tokens repeat")
	}
	if !Equal("abc", "abc") || Equal("abc", "abd") || Equal("abc", "ab") {
		t.Error("Equal")
	}
	for _, bad := range []string{"", "sha256/", "sha1/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg", "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCg"} {
		if ValidPin(bad) {
			t.Errorf("ValidPin(%q)", bad)
		}
	}
}

func TestKeyPEMRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pemBytes), "-----BEGIN EC PRIVATE KEY-----") {
		t.Errorf("PEM %s", pemBytes)
	}
	back, err := ParseKeyPEM(pemBytes)
	if err != nil || !back.Equal(key) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseKeyPEM([]byte("nope")); err == nil {
		t.Error("garbage accepted")
	}
}

// TestPinnedTLS checks the pinned client configuration against a real TLS
// server: the right pin connects, a wrong one fails with *PinMismatchError
// before the server receives any application data.
func TestPinnedTLS(t *testing.T) {
	key, _ := GenerateKey()
	cert, err := SelfSigned(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := CertPin(cert)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLSConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 16)
				n, _ := c.Read(buf)
				got <- string(buf[:n])
			}()
		}
	}()

	dial := func(cfg *tls.Config) error {
		d := tls.Dialer{Config: cfg}
		c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write([]byte("hi"))
		return err
	}
	if err := dial(PinnedClientTLSConfig(pin)); err != nil {
		t.Fatalf("right pin: %v", err)
	}
	if s := <-got; s != "hi" {
		t.Errorf("server read %q", s)
	}

	wrong := "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg"
	err = dial(PinnedClientTLSConfig(wrong))
	var pm *PinMismatchError
	if !errors.As(err, &pm) || !errors.Is(err, ErrPinMismatch) || pm.Got != pin || pm.Want != wrong {
		t.Fatalf("wrong pin: %v", err)
	}
	if s := <-got; s != "" {
		t.Errorf("server received %q despite the pin mismatch", s)
	}

	var seen string
	if err := dial(RecordingClientTLSConfig(func(p string) { seen = p })); err != nil || seen != pin {
		t.Errorf("recording: %v, seen %q", err, seen)
	}
	<-got

	// TLS 1.2 with an AEAD suite is accepted too (§3).
	cfg := PinnedClientTLSConfig(pin)
	cfg.MaxVersion = tls.VersionTLS12
	if err := dial(cfg); err != nil {
		t.Errorf("TLS 1.2: %v", err)
	}
	<-got
}
