package security

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// EncodeKeyPEM encodes key as a SEC 1 "EC PRIVATE KEY" PEM block, the form
// the test vectors use.
func EncodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM parses an ECDSA P-256 private key from PEM, either SEC 1
// ("EC PRIVATE KEY") or PKCS #8 ("PRIVATE KEY").
func ParseKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("no private key PEM block found")
		}
		var (
			key any
			err error
		)
		switch block.Type {
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok || ec.Curve != elliptic.P256() {
			return nil, errors.New("private key is not ECDSA P-256")
		}
		return ec, nil
	}
}

// CertValidity is the validity of generated certificates. Nobody checks it
// (§2), but some TLS stacks refuse expired certificates outright.
const CertValidity = 100 * 365 * 24 * time.Hour

// SelfSigned creates a self-signed certificate for key with the given common
// name, valid from an hour ago for CertValidity. The pin depends only on the
// key, so it can be regenerated at every start.
func SelfSigned(key *ecdsa.PrivateKey, commonName string) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func PublicKeyPin(pub crypto.PublicKey) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return PinSPKI(spki), nil
}

func CertPin(c tls.Certificate) (string, error) {
	if c.Leaf != nil {
		return Pin(c.Leaf), nil
	}
	if len(c.Certificate) == 0 {
		return "", errors.New("empty certificate")
	}
	return PinDER(c.Certificate[0])
}

// aeadSuites are the TLS 1.2 suites we offer and accept: ECDHE with an AEAD
// cipher only (§3). TLS 1.3 suites are not configurable and all qualify.
var aeadSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
}

// ServerTLSConfig returns the TLS configuration of a server presenting cert:
// TLS 1.3, or TLS 1.2 with ECDHE and an AEAD cipher.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		CipherSuites: aeadSuites,
	}
}

// PinnedClientTLSConfig returns a client TLS configuration that accepts the
// server if and only if its leaf certificate's pin equals pin. Chains, expiry
// and host names are deliberately not checked (§2). On a mismatch the
// handshake fails with a *PinMismatchError before any application data is
// sent.
func PinnedClientTLSConfig(pin string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		CipherSuites:       aeadSuites,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			got, err := leafPin(cs)
			if err != nil {
				return err
			}
			if !Equal(got, pin) {
				return &PinMismatchError{Want: pin, Got: got}
			}
			return nil
		},
	}
}

// RecordingClientTLSConfig returns a client TLS configuration that accepts
// any server certificate and calls seen with its pin during the handshake.
// It is only for SAS pairing (§4.3), where the pin is checked afterwards by
// comparing codes.
func RecordingClientTLSConfig(seen func(pin string)) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		CipherSuites:       aeadSuites,
		InsecureSkipVerify: true, // trust comes from the SAS comparison
		VerifyConnection: func(cs tls.ConnectionState) error {
			got, err := leafPin(cs)
			if err != nil {
				return err
			}
			seen(got)
			return nil
		},
	}
}

func leafPin(cs tls.ConnectionState) (string, error) {
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("server sent no certificate")
	}
	return Pin(cs.PeerCertificates[0]), nil
}

// LoadCertPEM parses the first certificate of a PEM file.
func LoadCertPEM(data []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no CERTIFICATE PEM block found")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}
