package client

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// Key attestation (spec/APPROVAL.md §3.1): the phone's secure hardware
// proves that its approval and decryption keys live there and need a
// fingerprint for every use. Approvals and DecryptKey refuse a key whose
// attestation is missing or fails, unless the user accepted it explicitly.

// Attestation outcomes, as devices.json stores them.
const (
	AttestedTEE       = store.AttestedTEE
	AttestedStrongBox = store.AttestedStrongBox
	// Unattested: the user accepted the key without attestation.
	Unattested = store.Unattested
)

// ErrKeyAttestation is matched (errors.Is) by *KeyAttestationError.
var ErrKeyAttestation = errors.New("key attestation failed")

// KeyAttestationError: a key's attestation is missing or failed, or it is
// not in StrongBox though WithRequireStrongBox asks for that. Nothing was
// sent to the phone.
type KeyAttestationError struct {
	Key    string // "approval" or "decryption"
	Device string // name, if known
	// Check names what failed: one of attest's Check constants, "missing"
	// or "strongbox".
	Check string
	Err   error
}

// Checks of KeyAttestationError besides attest's.
const (
	CheckMissing   = "missing"
	CheckStrongBox = "strongbox"
)

func (e *KeyAttestationError) Error() string {
	who := "the phone"
	if e.Device != "" {
		who = fmt.Sprintf("%q", e.Device)
	}
	return fmt.Sprintf("the %s key of %s failed key attestation (%s: %v)", e.Key, who, e.Check, e.Err)
}

func (e *KeyAttestationError) Unwrap() error { return e.Err }

func (e *KeyAttestationError) Is(target error) bool { return target == ErrKeyAttestation }

// WithRequireStrongBox refuses approval and decryption keys that aren't
// attested to be in StrongBox.
func WithRequireStrongBox() Option { return func(o *options) { o.requireStrongBox = true } }

// WithRevocationCheck makes attestation also check Google's revocation list
// (attest.StatusURL), fetched with hc (nil: http.DefaultClient) and kept
// for an hour. Off by default: it is the only thing that contacts the
// internet.
func WithRevocationCheck(hc *http.Client) Option {
	return func(o *options) {
		if hc == nil {
			hc = http.DefaultClient
		}
		o.revocation = hc
	}
}

// WithAppSigningDigests makes attestation accept only a phone app signed
// with one of these certificates (SHA-256 digests of the DER). Without it,
// any signer of the app's package is accepted, since builds differ.
func WithAppSigningDigests(digests ...[]byte) Option {
	return func(o *options) { o.appDigests = digests }
}

// WithAttestationRoots replaces Google's attestation roots. Only for tests
// and private deployments: keys attested by another root prove nothing
// about Android hardware.
func WithAttestationRoots(roots ...*x509.Certificate) Option {
	return func(o *options) { o.attestRoots = roots }
}

// WithUnattestedDecryptKey makes DecryptKey return a key whose attestation
// is missing or fails, with Attestation Unattested.
func WithUnattestedDecryptKey() Option { return func(o *options) { o.unattestedDecrypt = true } }

// AttestationOptionsFromEnv returns the options that the environment
// variables PHONETOPC_REQUIRE_STRONGBOX=1, PHONETOPC_CHECK_REVOCATION=1 and
// PHONETOPC_APP_SIGNING_DIGEST (hex SHA-256, comma-separated) ask for.
func AttestationOptionsFromEnv(getenv func(string) string) ([]Option, error) {
	var opts []Option
	if b, _ := strconv.ParseBool(getenv("PHONETOPC_REQUIRE_STRONGBOX")); b {
		opts = append(opts, WithRequireStrongBox())
	}
	if b, _ := strconv.ParseBool(getenv("PHONETOPC_CHECK_REVOCATION")); b {
		opts = append(opts, WithRevocationCheck(nil))
	}
	if v := getenv("PHONETOPC_APP_SIGNING_DIGEST"); v != "" {
		var digests [][]byte
		for _, h := range strings.Split(v, ",") {
			d, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(h), ":", ""))
			if err != nil || len(d) != 32 {
				return nil, fmt.Errorf("PHONETOPC_APP_SIGNING_DIGEST: %q is not a hex SHA-256 digest", h)
			}
			digests = append(digests, d)
		}
		opts = append(opts, WithAppSigningDigests(digests...))
	}
	return opts, nil
}

// attestKey verifies chain for the key with SubjectPublicKeyInfo spki and
// returns AttestedTEE or AttestedStrongBox. The challenge is this
// connection's pin: the phone binds its keys to its TLS key.
func (c *Client) attestKey(ctx context.Context, kind string, chain []string, spki []byte, purpose attest.Purpose) (string, error) {
	fail := func(check string, err error) (string, error) {
		return "", &KeyAttestationError{Key: kind, Device: c.Server().Device.Name, Check: check, Err: err}
	}
	if len(chain) == 0 {
		return fail(CheckMissing, errors.New("the phone sent no attestation; it may be an emulator, a modified or an older app"))
	}
	challenge := c.opts.testChallenge
	if challenge == nil {
		challenge = security.PinDigest(c.opts.pin)
	}
	if challenge == nil {
		return fail(attest.CheckChallenge, errors.New("the connection isn't pinned, so nothing ties the key to the phone"))
	}
	ders := make([][]byte, len(chain))
	for i, s := range chain {
		der, err := security.DecodeB64(s)
		if err != nil {
			return fail(attest.CheckChain, fmt.Errorf("certificate %d is not base64url", i))
		}
		ders[i] = der
	}
	o := attest.Options{Roots: c.opts.attestRoots, SigningDigests: c.opts.appDigests}
	if c.opts.revocation != nil {
		l, err := statusList(ctx, c.opts.revocation)
		if err != nil {
			return fail(attest.CheckRevocation, err)
		}
		o.Status = l
	}
	res, err := attest.Verify(ders, spki, challenge, purpose, o)
	if err != nil {
		var ae *attest.Error
		if errors.As(err, &ae) {
			return fail(ae.Check, errors.New(ae.Detail))
		}
		return fail(attest.CheckChain, err)
	}
	if res.Level == attest.StrongBox {
		return AttestedStrongBox, nil
	}
	return AttestedTEE, nil
}

// checkStrongBox applies WithRequireStrongBox to an attestation outcome.
func (c *Client) checkStrongBox(kind, level string) error {
	if !c.opts.requireStrongBox || level == AttestedStrongBox {
		return nil
	}
	what := map[string]string{AttestedTEE: "in the TEE", Unattested: "accepted without attestation"}[level]
	return &KeyAttestationError{Key: kind, Device: c.Server().Device.Name, Check: CheckStrongBox,
		Err: fmt.Errorf("the key is %s, and StrongBox is required", what)}
}

var statusCache struct {
	sync.Mutex
	list *attest.StatusList
	at   time.Time
}

// statusURL is attest.StatusURL; only tests change it.
var statusURL = attest.StatusURL

func statusList(ctx context.Context, hc *http.Client) (*attest.StatusList, error) {
	statusCache.Lock()
	defer statusCache.Unlock()
	if statusCache.list != nil && time.Since(statusCache.at) < time.Hour {
		return statusCache.list, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	l, err := attest.FetchStatusList(ctx, hc, statusURL)
	if err != nil {
		return nil, err
	}
	statusCache.list, statusCache.at = l, time.Now()
	return l, nil
}
