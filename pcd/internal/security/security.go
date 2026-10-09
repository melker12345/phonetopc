// Package security holds the cryptographic building blocks of
// spec/SECURITY.md: SPKI pins, base64url, the SAS commitment and code,
// constant-time comparisons, random secrets, and the long-term ECDSA P-256
// keys with their self-signed certificates and pinned TLS configurations.
//
// There is no custom cryptography here: only SHA-256, TLS and crypto/rand.
package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// PinPrefix starts every pin.
const PinPrefix = "sha256/"

// SASLabel is the domain-separation prefix of the SAS code hash (§4.3).
const SASLabel = "phonetopc-sas/1"

// B64 encodes b as base64url without padding (RFC 4648 §5).
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeB64 decodes base64url without padding. It is strict: padding,
// characters of the standard alphabet and non-canonical trailing bits are
// rejected, so every value has exactly one accepted spelling.
func DecodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// PinSPKI returns the pin of a DER-encoded SubjectPublicKeyInfo.
func PinSPKI(spki []byte) string {
	sum := sha256.Sum256(spki)
	return PinPrefix + B64(sum[:])
}

// Pin returns the pin of a certificate: the pin of its SubjectPublicKeyInfo.
func Pin(cert *x509.Certificate) string { return PinSPKI(cert.RawSubjectPublicKeyInfo) }

// PinDER returns the pin of a DER-encoded certificate.
func PinDER(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	return Pin(cert), nil
}

// ValidPin reports whether s is syntactically a pin: "sha256/" followed by
// the base64url encoding of 32 bytes.
func ValidPin(s string) bool {
	rest, ok := strings.CutPrefix(s, PinPrefix)
	if !ok {
		return false
	}
	b, err := DecodeB64(rest)
	return err == nil && len(b) == sha256.Size
}

// Commitment returns base64url(SHA-256(npc)), the PC's commitment to its SAS
// nonce (§4.3 step 2).
func Commitment(npc []byte) string {
	sum := sha256.Sum256(npc)
	return B64(sum[:])
}

// CheckCommitment reports, in constant time, whether commitment is the
// commitment to npc.
func CheckCommitment(commitment string, npc []byte) bool {
	return Equal(commitment, Commitment(npc))
}

// SASHash returns SHA-256("phonetopc-sas/1" ‖ pcPin ‖ phonePin ‖ npc ‖ nphone).
func SASHash(pcPin, phonePin string, npc, nphone []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(SASLabel))
	h.Write([]byte(pcPin))
	h.Write([]byte(phonePin))
	h.Write(npc)
	h.Write(nphone)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// SASCode returns the 6-digit comparison code of §4.3: the first four bytes
// of SASHash as a big-endian uint32, mod 1 000 000, zero-padded.
func SASCode(pcPin, phonePin string, npc, nphone []byte) string {
	sum := SASHash(pcPin, phonePin, npc, nphone)
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// FormatCode renders a 6-digit code for people: "940411" -> "940 411".
func FormatCode(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[:3] + " " + code[3:]
}

// Equal compares two secrets in constant time (the time depends only on
// the lengths).
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomBytes returns n bytes from the operating system's CSPRNG.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error (it crashes the program
	// instead if the OS generator fails), per its documentation.
	_, _ = rand.Read(b)
	return b
}

// Sizes of the secrets of SECURITY.md.
const (
	TokenSize  = 32 // §4.4
	SecretSize = 16 // §4.2, the QR secret
	NonceSize  = 32 // §4.3, Npc and Nphone
	IDSize     = 8  // §2, at least 64 bits
)

// NewToken returns a fresh pairing token, base64url-encoded.
func NewToken() string { return B64(RandomBytes(TokenSize)) }

// NewSecret returns a fresh QR pairing secret, base64url-encoded.
func NewSecret() string { return B64(RandomBytes(SecretSize)) }

// NewID returns a fresh device or PC id: 64 random bits as 16 hex digits.
func NewID() string { return hex.EncodeToString(RandomBytes(IDSize)) }

// ValidToken reports whether s is a base64url token of exactly TokenSize
// bytes.
func ValidToken(s string) bool {
	b, err := DecodeB64(s)
	return err == nil && len(b) == TokenSize
}

// ErrPinMismatch is matched (with errors.Is) by every *PinMismatchError.
var ErrPinMismatch = errors.New("pin mismatch")

// PinMismatchError reports that a peer presented a different key than the
// pinned one.
type PinMismatchError struct {
	Want, Got string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("certificate pin mismatch: expected %s, server presented %s", e.Want, e.Got)
}

// Is makes errors.Is(err, ErrPinMismatch) true.
func (e *PinMismatchError) Is(target error) bool { return target == ErrPinMismatch }
