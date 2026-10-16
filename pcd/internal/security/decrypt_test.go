package security

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

type decryptVectors struct {
	Key, Pin, Plaintext, Ciphertext string
	Reject                          []struct{ Name, Ciphertext string }
}

func loadDecryptVectors(t *testing.T) (decryptVectors, *rsa.PrivateKey) {
	t.Helper()
	var v decryptVectors
	b, err := os.ReadFile(filepath.Join(vectorsDir, "decrypt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile(filepath.Join(vectorsDir, "decrypt-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(p)
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return v, k.(*rsa.PrivateKey)
}

func TestVectorDecrypt(t *testing.T) {
	v, priv := loadDecryptVectors(t)
	pub, _, err := ParseDecryptKey(v.Key)
	if err != nil || !pub.Equal(&priv.PublicKey) {
		t.Fatalf("key: %v", err)
	}
	if key, pin, _ := EncodeDecryptKey(pub); key != v.Key || pin != v.Pin {
		t.Errorf("encoded %s %s", key, pin)
	}
	ct, _ := DecodeB64(v.Ciphertext)
	want, _ := DecodeB64(v.Plaintext)
	if got, err := DecryptAsPhone(priv, ct); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	for _, r := range v.Reject {
		ct, _ := DecodeB64(r.Ciphertext)
		if _, err := DecryptAsPhone(priv, ct); err == nil {
			t.Errorf("%s decrypted", r.Name)
		}
	}
	mine, err := EncryptToPhone(pub, want)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecryptAsPhone(priv, mine); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("round trip: %q %v", got, err)
	}
}

func TestParseDecryptKeyRejects(t *testing.T) {
	ec, _ := GenerateKey()
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	smallDER, _ := x509.MarshalPKIXPublicKey(&small.PublicKey)
	for name, key := range map[string]string{"ec": B64(ecDER), "1024 bits": B64(smallDER), "junk": "!!", "empty": ""} {
		if _, _, err := ParseDecryptKey(key); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
