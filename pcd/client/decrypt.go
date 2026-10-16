package client

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// Decryption with the phone's key (spec/DECRYPT.md): anyone can encrypt to
// the key, and only the phone decrypts, after a fingerprint each time.

// WhyFailed: the user confirmed, but the ciphertext doesn't decrypt with the
// phone's key.
const WhyFailed = protocol.WhyFailed

// DecryptKey is the phone's decryption key.
type DecryptKey struct {
	Public *rsa.PublicKey
	// SPKI is its DER SubjectPublicKeyInfo, Pin the pin of that.
	SPKI []byte
	Pin  string
	// Attestation is AttestedStrongBox, AttestedTEE, or Unattested if
	// unattested keys were allowed and this is one.
	Attestation string
}

// Encrypt encrypts plaintext (at most 190 bytes) so that only the phone can
// decrypt it (DECRYPT.md §3). It doesn't need the phone.
func (k DecryptKey) Encrypt(plaintext []byte) ([]byte, error) {
	return security.EncryptToPhone(k.Public, plaintext)
}

// ErrDecryptUnavailable: the phone offers no decryption (dev mode, no
// strong biometric enrolled, or no key).
var ErrDecryptUnavailable = errors.New("phonetopc: the phone can't decrypt (it needs secure mode and an enrolled fingerprint)")

// ErrDecryptKeyChanged is matched (errors.Is) by *DecryptKeyChangedError.
var ErrDecryptKeyChanged = errors.New("decryption key changed")

// DecryptKeyChangedError: the ciphertext was made for another key than the
// phone has now. Nothing was sent. The phone replaces its key when
// fingerprints are added or removed; what was encrypted to the old one
// needs another way in (DECRYPT.md §6).
type DecryptKeyChangedError struct {
	Want, Current string
}

func (e *DecryptKeyChangedError) Error() string {
	return fmt.Sprintf("the phone's decryption key is %s, not %s (it is replaced when fingerprints are added or removed)", e.Current, e.Want)
}

func (e *DecryptKeyChangedError) Is(target error) bool { return target == ErrDecryptKeyChanged }

// DecryptKey returns this session's decryption key from hello, or
// ErrDecryptUnavailable. A hello whose pin isn't the pin of its key is an
// error. So is a key whose attestation is missing or fails (a
// *KeyAttestationError), unless WithUnattestedDecryptKey: whatever is
// encrypted to the key is only as safe as the key.
func (c *Client) DecryptKey() (DecryptKey, error) {
	return c.decryptKey(context.Background(), c.opts.unattestedDecrypt)
}

// DecryptKeyAllowUnattested is DecryptKey with WithUnattestedDecryptKey.
func (c *Client) DecryptKeyAllowUnattested() (DecryptKey, error) {
	return c.decryptKey(context.Background(), true)
}

func (c *Client) decryptKey(ctx context.Context, allowUnattested bool) (DecryptKey, error) {
	h := c.Server()
	if h.PC == nil || !h.HasCapability(protocol.CapabilityDecrypt) || h.Decrypt == nil {
		return DecryptKey{}, ErrDecryptUnavailable
	}
	pub, der, err := security.ParseDecryptKey(h.Decrypt.Key)
	if err != nil {
		return DecryptKey{}, fmt.Errorf("phonetopc: the phone's hello has a bad decryption key: %w", err)
	}
	pin := security.PinSPKI(der)
	if pin != h.Decrypt.Pin {
		return DecryptKey{}, fmt.Errorf("phonetopc: the phone's decryption key has pin %s, not the %s it claims", pin, h.Decrypt.Pin)
	}
	verify := func() (string, error) {
		return c.attestKey(ctx, "decryption", h.Decrypt.Attestation, der, attest.PurposeDecrypt)
	}
	var level string
	if c.opts.decryptTrust != nil {
		level, err = c.opts.decryptTrust(pin, verify)
	} else {
		level, err = verify()
	}
	if err == nil {
		err = c.checkStrongBox("decryption", level)
	}
	if err != nil {
		if !allowUnattested || !errors.Is(err, ErrKeyAttestation) {
			return DecryptKey{}, err
		}
		level = Unattested
	}
	return DecryptKey{Public: pub, SPKI: der, Pin: pin, Attestation: level}, nil
}

// storeDecryptTrust remembers in devices.json which decryption key's
// attestation verified, so it is checked once per key, while its
// certificates are fresh.
func storeDecryptTrust(st *store.Store, dev store.Device) func(string, func() (string, error)) (string, error) {
	return func(pin string, verify func() (string, error)) (string, error) {
		if d, err := st.Find(dev.ID); err == nil && d.DecryptKey == pin && d.DecryptAttestation != "" {
			return d.DecryptAttestation, nil
		}
		level, err := verify()
		if err != nil {
			return "", err
		}
		_ = st.NoteDecryptKey(dev.ID, pin, level) // best effort: it is checked again next time
		return level, nil
	}
}

// DecryptResult is the outcome of Decrypt. A refusal is a result, not an
// error.
type DecryptResult struct {
	// Plaintext is set when decrypted.
	Plaintext []byte
	// Why says why not; empty when decrypted.
	Why Why
}

// Decrypted reports whether the phone decrypted.
func (r DecryptResult) Decrypted() bool { return r.Why == "" }

// Decrypt asks the phone's user to confirm reason with a fingerprint and
// decrypt ciphertext, made for the key with pin keyPin (DECRYPT.md §4). It
// waits up to timeout (0 means 30 s; allowed 5 s to 2 min).
//
// Not decrypted (denied, cancelled, timeout, busy, lockout, unavailable,
// failed) is a result. Errors: invalid arguments, ErrDecryptUnavailable,
// a *DecryptKeyChangedError (nothing sent), and connection problems. It
// doesn't need the key's attestation: that matters when encrypting.
//
// Check what the plaintext unlocks: the phone can't prove it is correct.
func (c *Client) Decrypt(ctx context.Context, reason, keyPin string, ciphertext []byte, timeout time.Duration) (DecryptResult, error) {
	if err := protocol.CheckReason(reason); err != nil {
		return DecryptResult{}, fmt.Errorf("phonetopc: %w", err)
	}
	if timeout == 0 {
		timeout = DefaultApprovalTimeout
	}
	if timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		return DecryptResult{}, fmt.Errorf("phonetopc: decrypt timeout %v is outside %v–%v", timeout, MinApprovalTimeout, MaxApprovalTimeout)
	}
	timeout = timeout.Truncate(time.Millisecond)
	key, err := c.decryptKey(ctx, true)
	if err != nil {
		return DecryptResult{}, err
	}
	if key.Pin != keyPin {
		return DecryptResult{}, &DecryptKeyChangedError{Want: keyPin, Current: key.Pin}
	}
	if len(ciphertext) != key.Public.Size() {
		return DecryptResult{}, fmt.Errorf("phonetopc: the ciphertext is %d bytes; this key's are %d", len(ciphertext), key.Public.Size())
	}
	ms := timeout.Milliseconds()
	params := protocol.DecryptParams{Reason: reason, KeyPin: keyPin, Ciphertext: security.B64(ciphertext), TimeoutMS: &ms}
	wait := timeout + protocol.ApprovalResponseLeeway + approvalWaitSlack
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var raw json.RawMessage
	err = c.Call(cctx, protocol.MethodDecrypt, params, &raw)
	switch {
	case err == nil:
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return DecryptResult{}, fmt.Errorf("%w: no answer from the phone within %v", ErrApprovalNoAnswer, wait)
	default:
		return DecryptResult{}, err
	}
	var r protocol.DecryptResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return DecryptResult{}, fmt.Errorf("phonetopc: decoding decrypt result: %w", err)
	}
	if !r.Decrypted {
		if r.Why == "" {
			r.Why = WhyDenied
		}
		return DecryptResult{Why: r.Why}, nil
	}
	pt, err := security.DecodeB64(r.Plaintext)
	if err != nil {
		return DecryptResult{}, fmt.Errorf("phonetopc: the phone's plaintext is not base64url: %w", err)
	}
	return DecryptResult{Plaintext: pt}, nil
}
