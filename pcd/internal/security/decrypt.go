package security

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
)

// Encryption to the phone's decryption key (DECRYPT.md §3): RSAES-OAEP with
// SHA-256, MGF1 with SHA-1 (the Android Keystore default) and no label.

var oaep = rsa.OAEPOptions{Hash: crypto.SHA256, MGFHash: crypto.SHA1}

// ParseDecryptKey decodes a decryption key, base64url DER
// SubjectPublicKeyInfo of an RSA key of 2048 to 4096 bits, and returns it
// with its DER.
func ParseDecryptKey(key string) (*rsa.PublicKey, []byte, error) {
	der, err := DecodeB64(key)
	if err != nil {
		return nil, nil, fmt.Errorf("decryption key is not base64url: %w", err)
	}
	pub, err := ParseDecryptKeyDER(der)
	return pub, der, err
}

// ParseDecryptKeyDER is ParseDecryptKey for the DER itself.
func ParseDecryptKeyDER(der []byte) (*rsa.PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("decryption key: %w", err)
	}
	r, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("decryption key is not an RSA key")
	}
	if bits := r.N.BitLen(); bits < 2048 || bits > 4096 {
		return nil, fmt.Errorf("decryption key has %d bits, not 2048 to 4096", bits)
	}
	// crypto/x509 accepts data after the exponent, which would give one key
	// several pins and age tags.
	if canonical, err := x509.MarshalPKIXPublicKey(r); err != nil || !bytes.Equal(canonical, der) {
		return nil, errors.New("decryption key is not in canonical DER")
	}
	return r, nil
}

// EncodeDecryptKey returns the hello form of a public key: base64url DER
// SubjectPublicKeyInfo, and its pin.
func EncodeDecryptKey(pub *rsa.PublicKey) (key, pin string, err error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return B64(der), PinSPKI(der), nil
}

// EncryptToPhone encrypts plaintext so that only the phone holding pub's
// private key can decrypt it.
func EncryptToPhone(pub *rsa.PublicKey, plaintext []byte) ([]byte, error) {
	return rsa.EncryptOAEPWithOptions(rand.Reader, pub, plaintext, &oaep)
}

// DecryptAsPhone is what the phone does for decrypt; the mock uses it.
func DecryptAsPhone(key *rsa.PrivateKey, ciphertext []byte) ([]byte, error) {
	return key.Decrypt(nil, ciphertext, &oaep)
}
