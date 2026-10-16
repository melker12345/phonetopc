package attest

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The key description extension, as specified at
// https://source.android.com/docs/security/features/keystore/attestation#schema.

// OID is the key description extension's OID.
var OID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}

type SecurityLevel int

const (
	Software           SecurityLevel = 0
	TrustedEnvironment SecurityLevel = 1
	StrongBox          SecurityLevel = 2
)

func (l SecurityLevel) String() string {
	switch l {
	case Software:
		return "software"
	case TrustedEnvironment:
		return "tee"
	case StrongBox:
		return "strongbox"
	}
	return fmt.Sprintf("level %d", int(l))
}

// Purpose is a KeyMint KeyPurpose.
type Purpose int64

const (
	PurposeDecrypt Purpose = 1
	PurposeSign    Purpose = 2
)

// VerifiedBootState values of RootOfTrust.
const (
	BootVerified   = 0
	BootSelfSigned = 1
	BootUnverified = 2
	BootFailed     = 3
)

// HardwareAuthenticatorType bits of userAuthType.
const (
	AuthPassword    = 1 << 0
	AuthFingerprint = 1 << 1
)

type KeyDescription struct {
	AttestationVersion       int64
	AttestationSecurityLevel SecurityLevel
	KeyMintVersion           int64
	KeyMintSecurityLevel     SecurityLevel
	Challenge                []byte
	UniqueID                 []byte
	SoftwareEnforced         AuthorizationList
	HardwareEnforced         AuthorizationList
}

// AuthorizationList holds the members this package checks; it skips the
// others. Absent integers are nil, absent NULL members false.
type AuthorizationList struct {
	Purposes                 []int64
	Algorithm                *int64
	KeySize                  *int64
	NoAuthRequired           bool
	UserAuthType             *int64
	AuthTimeout              *int64
	CreationDateTime         *int64 // milliseconds since 1970
	RootOfTrust              *RootOfTrust
	OSVersion                *int64
	OSPatchLevel             *int64
	AttestationApplicationID *ApplicationID
}

type RootOfTrust struct {
	VerifiedBootKey   []byte
	DeviceLocked      bool
	VerifiedBootState int
	VerifiedBootHash  []byte // absent before attestation version 3
}

type ApplicationID struct {
	Packages []PackageInfo
	// SignatureDigests are SHA-256 digests of the app's signing
	// certificates.
	SignatureDigests [][]byte
}

type PackageInfo struct {
	Name    string
	Version int64
}

// Tags of AuthorizationList members.
const (
	tagPurpose                  = 1
	tagAlgorithm                = 2
	tagKeySize                  = 3
	tagNoAuthRequired           = 503
	tagUserAuthType             = 504
	tagAuthTimeout              = 505
	tagCreationDateTime         = 701
	tagRootOfTrust              = 704
	tagOSVersion                = 705
	tagOSPatchLevel             = 706
	tagAttestationApplicationID = 709
)

type keyDescriptionASN1 struct {
	AttestationVersion       int64
	AttestationSecurityLevel asn1.Enumerated
	KeyMintVersion           int64
	KeyMintSecurityLevel     asn1.Enumerated
	Challenge                []byte
	UniqueID                 []byte
	SoftwareEnforced         asn1.RawValue
	HardwareEnforced         asn1.RawValue
}

type rootOfTrustASN1 struct {
	VerifiedBootKey   []byte
	DeviceLocked      bool
	VerifiedBootState asn1.Enumerated
	VerifiedBootHash  []byte `asn1:"optional"`
}

type applicationIDASN1 struct {
	Packages []packageInfoASN1 `asn1:"set"`
	Digests  [][]byte          `asn1:"set"`
}

type packageInfoASN1 struct {
	Name    []byte
	Version int64
}

// ParseKeyDescription parses the DER value of the key description
// extension.
func ParseKeyDescription(der []byte) (*KeyDescription, error) {
	var kd keyDescriptionASN1
	if err := unmarshalAll(der, &kd, ""); err != nil {
		return nil, fmt.Errorf("key description: %w", err)
	}
	out := &KeyDescription{
		AttestationVersion:       kd.AttestationVersion,
		AttestationSecurityLevel: SecurityLevel(kd.AttestationSecurityLevel),
		KeyMintVersion:           kd.KeyMintVersion,
		KeyMintSecurityLevel:     SecurityLevel(kd.KeyMintSecurityLevel),
		Challenge:                kd.Challenge,
		UniqueID:                 kd.UniqueID,
	}
	var err error
	if out.SoftwareEnforced, err = parseAuthorizationList(kd.SoftwareEnforced); err != nil {
		return nil, fmt.Errorf("softwareEnforced: %w", err)
	}
	if out.HardwareEnforced, err = parseAuthorizationList(kd.HardwareEnforced); err != nil {
		return nil, fmt.Errorf("hardwareEnforced: %w", err)
	}
	return out, nil
}

// parseAuthorizationList reads a SEQUENCE of [tag] EXPLICIT members, which
// DER requires in ascending tag order, each at most once.
func parseAuthorizationList(raw asn1.RawValue) (AuthorizationList, error) {
	var l AuthorizationList
	if raw.Class != asn1.ClassUniversal || raw.Tag != asn1.TagSequence || !raw.IsCompound {
		return l, errors.New("not a SEQUENCE")
	}
	rest := raw.Bytes
	last := -1
	for len(rest) > 0 {
		var m asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &m); err != nil {
			return l, err
		}
		if m.Class != asn1.ClassContextSpecific || !m.IsCompound {
			return l, fmt.Errorf("member with tag %d is not an EXPLICIT context-specific tag", m.Tag)
		}
		if m.Tag <= last {
			return l, fmt.Errorf("tag %d after tag %d: members must be in ascending order, each once", m.Tag, last)
		}
		last = m.Tag
		if err := l.set(m.Tag, m.Bytes); err != nil {
			return l, fmt.Errorf("tag %d: %w", m.Tag, err)
		}
	}
	return l, nil
}

func (l *AuthorizationList) set(tag int, inner []byte) error {
	integer := func(p **int64) error {
		var v int64
		if err := unmarshalAll(inner, &v, ""); err != nil {
			return err
		}
		*p = &v
		return nil
	}
	null := func(p *bool) error {
		var v asn1.RawValue
		if err := unmarshalAll(inner, &v, ""); err != nil {
			return err
		}
		if v.Class != asn1.ClassUniversal || v.Tag != asn1.TagNull || len(v.Bytes) != 0 {
			return errors.New("not NULL")
		}
		*p = true
		return nil
	}
	switch tag {
	case tagPurpose:
		return unmarshalAll(inner, &l.Purposes, "set")
	case tagAlgorithm:
		return integer(&l.Algorithm)
	case tagKeySize:
		return integer(&l.KeySize)
	case tagNoAuthRequired:
		return null(&l.NoAuthRequired)
	case tagUserAuthType:
		return integer(&l.UserAuthType)
	case tagAuthTimeout:
		return integer(&l.AuthTimeout)
	case tagCreationDateTime:
		return integer(&l.CreationDateTime)
	case tagRootOfTrust:
		var r rootOfTrustASN1
		if err := unmarshalAll(inner, &r, ""); err != nil {
			return err
		}
		l.RootOfTrust = &RootOfTrust{
			VerifiedBootKey:   r.VerifiedBootKey,
			DeviceLocked:      r.DeviceLocked,
			VerifiedBootState: int(r.VerifiedBootState),
			VerifiedBootHash:  r.VerifiedBootHash,
		}
	case tagOSVersion:
		return integer(&l.OSVersion)
	case tagOSPatchLevel:
		return integer(&l.OSPatchLevel)
	case tagAttestationApplicationID:
		// An OCTET STRING that holds the DER of the structure.
		var b []byte
		if err := unmarshalAll(inner, &b, ""); err != nil {
			return err
		}
		var a applicationIDASN1
		if err := unmarshalAll(b, &a, ""); err != nil {
			return err
		}
		id := &ApplicationID{SignatureDigests: a.Digests}
		for _, p := range a.Packages {
			id.Packages = append(id.Packages, PackageInfo{Name: string(p.Name), Version: p.Version})
		}
		l.AttestationApplicationID = id
	default:
		var v asn1.RawValue
		return unmarshalAll(inner, &v, "")
	}
	return nil
}

func unmarshalAll(b []byte, v any, params string) error {
	rest, err := asn1.UnmarshalWithParams(b, v, params)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("trailing data")
	}
	return nil
}
