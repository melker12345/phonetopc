// Package attest verifies Android key attestation (APPROVAL.md §3.1): a
// certificate chain from the phone's secure hardware, up to Google's
// attestation root, that describes a Keystore key.
//
// It follows https://developer.android.com/privacy-and-security/security-key-attestation:
// the key description is taken only from the leaf, the chain is checked
// signature by signature up to an embedded root, and chains from
// factory-provisioned keys are trusted after their certificates expired.
package attest

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"slices"
	"time"
)

// Package is the phone app's package name.
const Package = "io.github.melker12345.phonetopc"

// Checks, as Error.Check names them.
const (
	CheckChain         = "chain"
	CheckRoot          = "root"
	CheckValidity      = "validity"
	CheckRevocation    = "revocation"
	CheckKey           = "key"
	CheckExtension     = "key description"
	CheckChallenge     = "challenge"
	CheckSecurityLevel = "security level"
	CheckBiometric     = "biometric"
	CheckPerUse        = "per-use authentication"
	CheckPurpose       = "purpose"
	CheckBoot          = "verified boot"
	CheckPackage       = "package"
	CheckSigner        = "signing certificate"
)

// Error says which check failed.
type Error struct {
	Check  string
	Detail string
}

func (e *Error) Error() string { return e.Check + ": " + e.Detail }

func fail(check, format string, args ...any) *Error {
	return &Error{Check: check, Detail: fmt.Sprintf(format, args...)}
}

type Options struct {
	// Roots are the trusted roots; nil means GoogleRoots. Tests pass their
	// own.
	Roots []*x509.Certificate
	// Now is the time certificates must be valid at; zero means now.
	Now time.Time
	// Package is the expected package name; "" means Package.
	Package string
	// SigningDigests, if not empty, are the SHA-256 digests of the app
	// signing certificates to accept; one of them must be attested.
	SigningDigests [][]byte
	// Status, if set, is Google's revocation list; no certificate of the
	// chain may be on it.
	Status *StatusList
}

type Result struct {
	// Level is the lower of attestationSecurityLevel and
	// keyMintSecurityLevel: TrustedEnvironment or StrongBox.
	Level              SecurityLevel
	AttestationVersion int64
	// SigningDigests are the attested app signing certificate digests.
	SigningDigests [][]byte
}

// GoogleRoots returns Google's hardware attestation roots.
func GoogleRoots() []*x509.Certificate {
	roots := make([]*x509.Certificate, len(googleRootsPEM))
	for i, p := range googleRootsPEM {
		b, _ := pem.Decode([]byte(p))
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			panic("attest: bad embedded root: " + err.Error())
		}
		roots[i] = c
	}
	return roots
}

// Verify checks that chain (DER certificates, leaf first) attests that the
// key with SubjectPublicKeyInfo spki lives in secure hardware of a device
// with verified boot, belongs to the phone app, needs a strong biometric
// for every use, may be used for purpose, and was made with challenge.
// Errors are *Error.
func Verify(chain [][]byte, spki, challenge []byte, purpose Purpose, o Options) (Result, error) {
	certs, path, err := verifyChain(chain, o)
	if err != nil {
		return Result{}, err
	}
	leaf := certs[0]
	if err := sameKey(leaf, spki); err != nil {
		return Result{}, err
	}
	var ext []byte
	for _, e := range leaf.Extensions {
		if e.Id.Equal(OID) {
			ext = e.Value
		}
	}
	if ext == nil {
		return Result{}, fail(CheckExtension, "the leaf certificate has no key description")
	}
	kd, err := ParseKeyDescription(ext)
	if err != nil {
		return Result{}, fail(CheckExtension, "%v", err)
	}
	if err := checkValidity(path, kd, o.Now); err != nil {
		return Result{}, err
	}
	return checkDescription(kd, challenge, purpose, o)
}

// verifyChain returns the certificates, and those of them below the root.
func verifyChain(chain [][]byte, o Options) (certs, path []*x509.Certificate, err error) {
	if len(chain) < 2 {
		return nil, nil, fail(CheckChain, "%d certificates; an attestation chain has at least 2", len(chain))
	}
	certs = make([]*x509.Certificate, len(chain))
	for i, der := range chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, fail(CheckChain, "certificate %d: %v", i, err)
		}
		certs[i] = c
	}
	for i, c := range certs[1:] {
		child := certs[i]
		if !bytes.Equal(child.RawIssuer, c.RawSubject) {
			return nil, nil, fail(CheckChain, "certificate %d is not issued by certificate %d", i, i+1)
		}
		if err := c.CheckSignature(child.SignatureAlgorithm, child.RawTBSCertificate, child.Signature); err != nil {
			return nil, nil, fail(CheckChain, "certificate %d: %v", i, err)
		}
		// Only the leaf may carry a key description: one further up was
		// not made by the secure hardware.
		if slices.ContainsFunc(c.Extensions, func(e pkix.Extension) bool { return e.Id.Equal(OID) }) {
			return nil, nil, fail(CheckChain, "certificate %d has a key description; only the leaf may", i+1)
		}
	}

	roots := o.Roots
	if roots == nil {
		roots = GoogleRoots()
	}
	last := certs[len(certs)-1]
	path = certs
	anchored := false
	for _, r := range roots {
		signed := r.CheckSignature(last.SignatureAlgorithm, last.RawTBSCertificate, last.Signature) == nil
		if bytes.Equal(last.RawSubjectPublicKeyInfo, r.RawSubjectPublicKeyInfo) && signed {
			// The chain ends with (an issue of) the root itself.
			anchored, path = true, certs[:len(certs)-1]
			break
		}
		if bytes.Equal(last.RawIssuer, r.RawSubject) && signed {
			anchored = true
			break
		}
	}
	if !anchored || len(path) < 2 {
		return nil, nil, fail(CheckRoot, "the chain does not end at a trusted attestation root")
	}
	if o.Status != nil {
		for i, c := range certs {
			if e, ok := o.Status.Lookup(c.SerialNumber); ok {
				return nil, nil, fail(CheckRevocation, "certificate %d (serial %x) is %s (%s)", i, c.SerialNumber, e.Status, e.Reason)
			}
		}
	}
	return certs, path, nil
}

// checkValidity checks the certificates above the leaf at the time the key
// was made, or now if the key description doesn't say. Remotely
// provisioned attestation certificates are valid for days to weeks, and a
// key's chain never changes, so checking them now would fail for a PC that
// first sees a key a few weeks old. A leaked attestation key is caught by
// the revocation list, not by expiry.
func checkValidity(path []*x509.Certificate, kd *KeyDescription, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	at := now
	created := kd.SoftwareEnforced.CreationDateTime
	if created == nil {
		created = kd.HardwareEnforced.CreationDateTime
	}
	if created != nil {
		if t := time.UnixMilli(*created); t.Before(now) {
			at = t
		}
	}
	// Factory-provisioned intermediates have a serial number in their
	// subject; their chains stay trusted after they expire, because their
	// keys can't be rotated. The leaf's validity is set on the phone, so it
	// isn't checked.
	factory := path[len(path)-1].Subject.SerialNumber != ""
	for i, c := range path[1:] {
		switch {
		case at.Before(c.NotBefore):
			return fail(CheckValidity, "certificate %d is not valid before %s (checked at %s)", i+1,
				c.NotBefore.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
		case at.After(c.NotAfter) && !factory:
			return fail(CheckValidity, "certificate %d expired %s (checked at %s)", i+1,
				c.NotAfter.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func sameKey(leaf *x509.Certificate, spki []byte) error {
	want, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return fail(CheckKey, "the key: %v", err)
	}
	k, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !k.Equal(want) {
		return fail(CheckKey, "the chain attests another key")
	}
	return nil
}

func checkDescription(kd *KeyDescription, challenge []byte, purpose Purpose, o Options) (Result, error) {
	if !bytes.Equal(kd.Challenge, challenge) {
		return Result{}, fail(CheckChallenge, "the key was not made for this phone's TLS key")
	}
	level := min(kd.AttestationSecurityLevel, kd.KeyMintSecurityLevel)
	if level != TrustedEnvironment && level != StrongBox {
		return Result{}, fail(CheckSecurityLevel, "attestation %s, KeyMint %s; the key must be in the TEE or StrongBox",
			kd.AttestationSecurityLevel, kd.KeyMintSecurityLevel)
	}

	hw, sw := &kd.HardwareEnforced, &kd.SoftwareEnforced
	if hw.NoAuthRequired || sw.NoAuthRequired {
		return Result{}, fail(CheckBiometric, "the key works without user authentication")
	}
	switch {
	case hw.UserAuthType == nil:
		return Result{}, fail(CheckBiometric, "the secure hardware doesn't enforce user authentication")
	case *hw.UserAuthType&AuthFingerprint == 0:
		return Result{}, fail(CheckBiometric, "the key doesn't need a biometric (userAuthType %d)", *hw.UserAuthType)
	case *hw.UserAuthType&AuthPassword != 0:
		return Result{}, fail(CheckBiometric, "the key also works with the PIN, pattern or password (userAuthType %d)", *hw.UserAuthType)
	}
	if hw.AuthTimeout != nil || sw.AuthTimeout != nil {
		return Result{}, fail(CheckPerUse, "one authentication unlocks the key for a while; it must need one for every use")
	}
	if !slices.Contains(hw.Purposes, int64(purpose)) {
		return Result{}, fail(CheckPurpose, "purposes %v don't include %d", hw.Purposes, purpose)
	}
	switch rot := hw.RootOfTrust; {
	case rot == nil:
		return Result{}, fail(CheckBoot, "no root of trust")
	case rot.VerifiedBootState != BootVerified:
		return Result{}, fail(CheckBoot, "verified boot state %s", bootState(rot.VerifiedBootState))
	case !rot.DeviceLocked:
		return Result{}, fail(CheckBoot, "the bootloader is unlocked")
	}

	pkg := o.Package
	if pkg == "" {
		pkg = Package
	}
	app := sw.AttestationApplicationID
	if app == nil {
		app = hw.AttestationApplicationID
	}
	if app == nil {
		return Result{}, fail(CheckPackage, "no attestation application id")
	}
	if len(app.Packages) != 1 || app.Packages[0].Name != pkg {
		names := make([]string, len(app.Packages))
		for i, p := range app.Packages {
			names[i] = p.Name
		}
		return Result{}, fail(CheckPackage, "the key belongs to %q, not %q", names, pkg)
	}
	if len(o.SigningDigests) > 0 && !slices.ContainsFunc(app.SignatureDigests, func(d []byte) bool {
		return slices.ContainsFunc(o.SigningDigests, func(w []byte) bool { return bytes.Equal(d, w) })
	}) {
		return Result{}, fail(CheckSigner, "the app is signed with %x, not an expected certificate", app.SignatureDigests)
	}
	return Result{Level: level, AttestationVersion: kd.AttestationVersion, SigningDigests: app.SignatureDigests}, nil
}

func bootState(s int) string {
	switch s {
	case BootVerified:
		return "Verified"
	case BootSelfSigned:
		return "SelfSigned (a custom OS)"
	case BootUnverified:
		return "Unverified (an unlocked bootloader)"
	case BootFailed:
		return "Failed"
	}
	return fmt.Sprint(s)
}

// Challenge is the attestation challenge a phone uses for its keys: the
// SHA-256 of its TLS SubjectPublicKeyInfo, the digest in its pin.
func Challenge(tlsSPKI []byte) []byte {
	sum := sha256.Sum256(tlsSPKI)
	return sum[:]
}
