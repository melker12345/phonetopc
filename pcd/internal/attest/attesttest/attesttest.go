// Package attesttest makes key attestation chains from a test root, for the
// mock phone and tests. Only a verifier given CA.Root accepts them; the
// production roots are Google's.
package attesttest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
)

// CA is a test attestation root and an intermediate below it.
type CA struct {
	Root         *x509.Certificate
	Intermediate *x509.Certificate
	interKey     *ecdsa.PrivateKey
}

// Validity of the test certificates: long, so that recorded chains keep
// verifying.
var (
	NotBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	NotAfter  = time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC)
)

func NewCA() (*CA, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "phonetopc test attestation root"},
		NotBefore:             NotBefore,
		NotAfter:              NotAfter,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, err
	}
	interTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "phonetopc test attestation intermediate"},
		NotBefore:             NotBefore,
		NotAfter:              NotAfter,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	interDER, err := x509.CreateCertificate(rand.Reader, interTmpl, root, &interKey.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	inter, err := x509.ParseCertificate(interDER)
	if err != nil {
		return nil, err
	}
	return &CA{Root: root, Intermediate: inter, interKey: interKey}, nil
}

// Description is what a test chain's leaf attests. Good returns one that
// passes every check of attest.Verify.
type Description struct {
	AttestationLevel, KeyMintLevel attest.SecurityLevel
	Challenge                      []byte
	Purposes                       []int64
	UserAuthType                   *int64 // nil: absent
	NoAuthRequired                 bool
	AuthTimeout                    *int64 // nil: absent
	BootState                      int
	DeviceLocked                   bool
	NoRootOfTrust                  bool
	Packages                       []string
	SigningDigests                 [][]byte
	Created                        time.Time // zero: Created
}

// Created is when test keys are made, unless Description says otherwise.
var Created = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// Good describes a key in the TEE that needs a fingerprint for every use,
// on a locked device with verified boot, of the phonetopc app.
func Good(challenge []byte, purpose attest.Purpose) Description {
	fp := int64(attest.AuthFingerprint)
	return Description{
		AttestationLevel: attest.TrustedEnvironment,
		KeyMintLevel:     attest.TrustedEnvironment,
		Challenge:        challenge,
		Purposes:         []int64{int64(purpose)},
		UserAuthType:     &fp,
		BootState:        attest.BootVerified,
		DeviceLocked:     true,
		Packages:         []string{attest.Package},
		SigningDigests:   [][]byte{make([]byte, 32)},
	}
}

func Int(v int64) *int64 { return &v }

// Chain returns the DER chain, leaf first, attesting pub as d says.
func (ca *CA) Chain(pub crypto.PublicKey, d Description) ([][]byte, error) {
	ext, err := Encode(d)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:    serial.Add(serial, big.NewInt(1)),
		Subject:         pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore:       time.Unix(0, 0),
		NotAfter:        time.Date(2048, 1, 1, 0, 0, 0, 0, time.UTC),
		ExtraExtensions: []pkix.Extension{{Id: attest.OID, Value: ext}},
	}
	leaf, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Intermediate, pub, ca.interKey)
	if err != nil {
		return nil, err
	}
	return [][]byte{leaf, ca.Intermediate.Raw, ca.Root.Raw}, nil
}

// Encode returns the DER of the key description extension.
func Encode(d Description) ([]byte, error) {
	var hw, sw []byte
	add := func(list *[]byte, tag int, v any, params string) error {
		inner, err := asn1.MarshalWithParams(v, params)
		if err != nil {
			return err
		}
		m, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, IsCompound: true, Bytes: inner})
		if err != nil {
			return err
		}
		*list = append(*list, m...)
		return nil
	}
	null := asn1.RawValue{Tag: asn1.TagNull}
	var err error
	try := func(e error) {
		if err == nil {
			err = e
		}
	}
	try(add(&hw, 1, d.Purposes, "set"))
	if d.NoAuthRequired {
		try(add(&hw, 503, null, ""))
	}
	if d.UserAuthType != nil {
		try(add(&hw, 504, *d.UserAuthType, ""))
	}
	if d.AuthTimeout != nil {
		try(add(&hw, 505, *d.AuthTimeout, ""))
	}
	if !d.NoRootOfTrust {
		rot := struct {
			Key    []byte
			Locked bool
			State  asn1.Enumerated
			Hash   []byte
		}{make([]byte, 32), d.DeviceLocked, asn1.Enumerated(d.BootState), make([]byte, 32)}
		try(add(&hw, 704, rot, ""))
	}
	try(add(&hw, 705, int64(160000), ""))
	created := d.Created
	if created.IsZero() {
		created = Created
	}
	try(add(&sw, 701, created.UnixMilli(), ""))
	if d.Packages != nil {
		type pkg struct {
			Name    []byte
			Version int64
		}
		app := struct {
			Packages []pkg    `asn1:"set"`
			Digests  [][]byte `asn1:"set"`
		}{Digests: d.SigningDigests}
		for _, p := range d.Packages {
			app.Packages = append(app.Packages, pkg{[]byte(p), 1})
		}
		b, e := asn1.Marshal(app)
		try(e)
		try(add(&sw, 709, b, ""))
	}
	if err != nil {
		return nil, err
	}
	seq := func(b []byte) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: b}
	}
	return asn1.Marshal(struct {
		AttestationVersion       int64
		AttestationSecurityLevel asn1.Enumerated
		KeyMintVersion           int64
		KeyMintSecurityLevel     asn1.Enumerated
		Challenge                []byte
		UniqueID                 []byte
		SoftwareEnforced         asn1.RawValue
		HardwareEnforced         asn1.RawValue
	}{300, asn1.Enumerated(d.AttestationLevel), 300, asn1.Enumerated(d.KeyMintLevel), d.Challenge, []byte{}, seq(sw), seq(hw)})
}
