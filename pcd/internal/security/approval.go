package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
)

// Fingerprint approval signatures (APPROVAL.md §5).

// ApprovalLabel is the first line of the signed approval message.
const ApprovalLabel = "phonetopc-approval/1"

// ApprovalMessage returns the bytes a phone signs for an approval: the
// label, phone id, PC id, nonce (base64url as sent) and
// base64url(SHA-256(reason)), joined by "\n" without a trailing newline.
func ApprovalMessage(phoneID, pcID, nonce, reason string) []byte {
	sum := sha256.Sum256([]byte(reason))
	return []byte(strings.Join([]string{ApprovalLabel, phoneID, pcID, nonce, B64(sum[:])}, "\n"))
}

// ErrBadApprovalSignature is returned (wrapped) by VerifyApproval for a
// signature that doesn't verify.
var ErrBadApprovalSignature = errors.New("approval signature does not verify")

// ParseApprovalKey decodes an approval key: base64url DER
// SubjectPublicKeyInfo of an ECDSA P-256 key.
func ParseApprovalKey(key string) (*ecdsa.PublicKey, error) {
	der, err := DecodeB64(key)
	if err != nil {
		return nil, fmt.Errorf("approval key is not base64url: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("approval key: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("approval key is not an ECDSA P-256 key")
	}
	return ec, nil
}

// ApprovalKeyPin returns the pin of an approval key as sent in hello
// (SECURITY.md §2 applied to the decoded SubjectPublicKeyInfo).
func ApprovalKeyPin(key string) (string, error) {
	der, err := DecodeB64(key)
	if err != nil {
		return "", fmt.Errorf("approval key is not base64url: %w", err)
	}
	return PinSPKI(der), nil
}

// EncodeApprovalKey returns the hello form of a public key: base64url DER
// SubjectPublicKeyInfo, and its pin.
func EncodeApprovalKey(pub *ecdsa.PublicKey) (key, pin string, err error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return B64(der), PinSPKI(der), nil
}

// VerifyApproval checks that signature (base64url DER ECDSA) is key's
// SHA256withECDSA signature over msg.
func VerifyApproval(key *ecdsa.PublicKey, signature string, msg []byte) error {
	sig, err := DecodeB64(signature)
	if err != nil {
		return fmt.Errorf("%w: not base64url", ErrBadApprovalSignature)
	}
	sum := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(key, sum[:], sig) {
		return ErrBadApprovalSignature
	}
	return nil
}

// SignApproval signs msg with key (SHA256withECDSA, DER, base64url). Only
// the mock phone signs; real phones use their Keystore key.
func SignApproval(key *ecdsa.PrivateKey, msg []byte) (string, error) {
	sum := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	return B64(sig), nil
}
