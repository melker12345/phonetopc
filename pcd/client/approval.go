package client

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// Fingerprint approval (spec/APPROVAL.md): the phone's user confirms with a
// fingerprint, and the phone signs the request with a key that only works
// right after that. RequestApproval verifies the signature itself.

// Why says why an approval was not given.
type Why = protocol.Why

// Outcomes of an approval that was not given (APPROVAL.md §4.1).
const (
	WhyDenied      = protocol.WhyDenied
	WhyCancelled   = protocol.WhyCancelled
	WhyMismatch    = protocol.WhyMismatch
	WhyTimeout     = protocol.WhyTimeout
	WhyBusy        = protocol.WhyBusy
	WhyCooldown    = protocol.WhyCooldown
	WhyLockout     = protocol.WhyLockout
	WhyUnavailable = protocol.WhyUnavailable
)

// DefaultProgram is the program name sent when none is given.
const DefaultProgram = protocol.DefaultProgram

// Limits of RequestApproval's timeout (APPROVAL.md §4).
const (
	DefaultApprovalTimeout = protocol.DefaultApprovalTimeout
	MinApprovalTimeout     = protocol.MinApprovalTimeout
	MaxApprovalTimeout     = protocol.MaxApprovalTimeout
)

// ApprovalResult is the outcome of RequestApproval. A refusal is a result,
// not an error; check Approved, not the fields.
type ApprovalResult struct {
	approved bool
	// Why says why the approval was not given; empty when approved.
	Why Why
	// Reason, Program and Nonce are as sent.
	Reason, Program, Nonce string
	// Match is the number the user had to pick, and Expiry when the
	// signature expires, in Unix milliseconds.
	Match  int
	Expiry int64
	// Signature is the verified signature (base64url DER) when approved.
	Signature string
	// Key is the approval key (base64url DER SubjectPublicKeyInfo) and
	// KeyPin its pin.
	Key, KeyPin string
	// PhoneID and PCID are the ids the signed message names.
	PhoneID, PCID string
	// Attestation is the approval key's attestation outcome:
	// AttestedStrongBox, AttestedTEE, or Unattested if the user accepted it
	// without (APPROVAL.md §3.1).
	Attestation string
}

// Approved reports whether the phone's user approved. It is true only if
// the phone answered approved and its signature verified (APPROVAL.md §5):
// RequestApproval never returns an unverified approval.
func (r ApprovalResult) Approved() bool { return r.approved }

// String describes the outcome for people.
func (r ApprovalResult) String() string {
	if r.approved {
		return "approved"
	}
	return fmt.Sprintf("not approved: %s (%s)", r.Why, r.Why.Describe())
}

// ErrApprovalKeyChanged is matched (errors.Is) by *ApprovalKeyChangedError.
var ErrApprovalKeyChanged = errors.New("approval key changed")

// ApprovalKeyChangedError: the phone's approval key differs from the one
// accepted earlier (APPROVAL.md §3). No request was sent. Typical cause:
// fingerprints were added or removed on the phone. Accept the new key with
// `phonetopc approval-key --accept` (store.AcceptApprovalKey).
type ApprovalKeyChangedError struct {
	Device   string // name, if known
	DeviceID string
	Stored   string // accepted pin
	Current  string // pin in this session's hello
}

func (e *ApprovalKeyChangedError) Error() string {
	who := "the phone"
	if e.Device != "" {
		who = fmt.Sprintf("%q", e.Device)
	}
	return fmt.Sprintf("the approval key of %s changed (accepted %s, now %s); approvals are refused until you accept it", who, e.Stored, e.Current)
}

// Is makes errors.Is(err, ErrApprovalKeyChanged) true.
func (e *ApprovalKeyChangedError) Is(target error) bool { return target == ErrApprovalKeyChanged }

// ErrApprovalInvalid is matched by every *ApprovalInvalidError.
var ErrApprovalInvalid = errors.New("invalid approval")

// ApprovalInvalidError: the phone answered approved, but the answer failed
// verification (a missing or invalid signature, or a bad key in hello). It
// is never an approval, and may mean an attack or a broken phone app.
type ApprovalInvalidError struct {
	Problem string
	Err     error
}

func (e *ApprovalInvalidError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("approval rejected: %s: %v", e.Problem, e.Err)
	}
	return "approval rejected: " + e.Problem
}

func (e *ApprovalInvalidError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrApprovalInvalid) true.
func (e *ApprovalInvalidError) Is(target error) bool { return target == ErrApprovalInvalid }

// ErrNoApprovalTrust: a client made with Dial has no stored approval key to
// compare with. Use DialPaired, WithApprovalKey or WithApprovalTrust.
var ErrNoApprovalTrust = errors.New("phonetopc: no trusted approval key for this connection; use DialPaired, or Dial with WithApprovalKey")

// approvalWaitSlack is how much longer than timeout_ms + 5 s RequestApproval
// waits for the answer; only tests change it.
var approvalWaitSlack = 5 * time.Second

// ErrApprovalNoAnswer: the phone didn't answer request_approval, not even
// with "timeout", within timeout + 10 s (RequestApproval wraps it).
var ErrApprovalNoAnswer = errors.New("phonetopc: approval request timed out")

// ErrApprovalNeedsSecureMode: approvals don't exist in dev mode.
var ErrApprovalNeedsSecureMode = errors.New("phonetopc: approvals need secure mode (a paired phone); dev mode has none")

// ApprovalTrust decides whether this session's approval key, with pin, is
// trusted (APPROVAL.md §3). attest verifies the key's attestation (§3.1)
// when called. It returns the key's attestation outcome, or an error:
// typically an *ApprovalKeyChangedError or a *KeyAttestationError.
type ApprovalTrust func(pin string, attest func() (string, error)) (string, error)

// WithApprovalKey makes RequestApproval of a Dial'd client accept only the
// approval key with this pin, and only if its attestation verifies.
// DialPaired uses devices.json instead.
func WithApprovalKey(pin string) Option {
	return WithApprovalTrust(func(current string, attest func() (string, error)) (string, error) {
		if current != pin {
			return "", &ApprovalKeyChangedError{Stored: pin, Current: current}
		}
		return attest()
	})
}

// WithApprovalTrust sets what decides whether this session's approval key
// is trusted.
func WithApprovalTrust(f ApprovalTrust) Option {
	return func(o *options) { o.approvalTrust = f }
}

// storeApprovalTrust is the trust of DialPaired. First sight stores the pin
// in devices.json, with the attestation outcome, if attestation verifies;
// a different stored pin is a change. Attestation is checked once per key:
// later sessions use the stored outcome.
func storeApprovalTrust(st *store.Store, dev store.Device) ApprovalTrust {
	return func(pin string, attest func() (string, error)) (string, error) {
		changed := func(stored string) error {
			return &ApprovalKeyChangedError{Device: dev.Name, DeviceID: dev.ID, Stored: stored, Current: pin}
		}
		d, err := st.Find(dev.ID)
		if err != nil {
			return "", fmt.Errorf("phonetopc: approval key: %w", err)
		}
		switch {
		case d.ApprovalKey != "" && d.ApprovalKey != pin:
			return "", changed(d.ApprovalKey)
		case d.ApprovalKey == pin && d.ApprovalAttestation != "":
			return d.ApprovalAttestation, nil
		}
		level, err := attest()
		if err != nil {
			return "", err
		}
		stored, err := st.TrustApprovalKey(dev.ID, pin, level)
		if err != nil {
			return "", fmt.Errorf("phonetopc: approval key: %w", err)
		}
		if stored != "" && stored != pin {
			return "", changed(stored)
		}
		return level, nil
	}
}

// CheckApprovalKey reports whether this session's approval key is trusted:
// nil if so (storing it on first sight with DialPaired), an
// *ApprovalKeyChangedError if it changed, ErrNoApprovalTrust without a way
// to tell. It returns nil, false if the phone offers no approvals.
//
// The pin compared is computed here from hello.approval.key; a hello whose
// claimed pin differs is an *ApprovalInvalidError (APPROVAL.md §5 step 2).
// A phone that lists capability approval without hello.approval, or the
// reverse, offers no approvals.
func (c *Client) CheckApprovalKey() (offered bool, err error) {
	_, _, _, err = c.approvalKey(context.Background())
	if err == ErrNoApprovalKey {
		return false, nil
	}
	return true, err
}

// ApprovalKeyAttestation is CheckApprovalKey for a phone that offers
// approvals: it returns the trusted key's attestation outcome, or why the
// key isn't trusted.
func (c *Client) ApprovalKeyAttestation(ctx context.Context) (string, error) {
	_, _, level, err := c.approvalKey(ctx)
	return level, err
}

// AttestApprovalKey verifies the attestation of this session's approval
// key now, whatever was stored, and returns AttestedTEE or
// AttestedStrongBox, or a *KeyAttestationError. Without an approval key it
// returns ErrNoApprovalKey.
func (c *Client) AttestApprovalKey(ctx context.Context) (string, error) {
	h := c.Server()
	if !h.HasCapability(protocol.CapabilityApproval) || h.Approval == nil {
		return "", ErrNoApprovalKey
	}
	der, err := security.DecodeB64(h.Approval.Key)
	if err != nil {
		return "", &ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: err}
	}
	return c.attestKey(ctx, "approval", h.Approval.Attestation, der, attest.PurposeSign)
}

// ErrNoApprovalKey: the phone offers no approvals.
var ErrNoApprovalKey = errors.New("phonetopc: the phone offers no approvals")

// approvalKey returns this session's trusted approval key, its pin and its
// attestation outcome.
func (c *Client) approvalKey(ctx context.Context) (*ecdsa.PublicKey, string, string, error) {
	h := c.Server()
	if !h.HasCapability(protocol.CapabilityApproval) || h.Approval == nil {
		return nil, "", "", ErrNoApprovalKey
	}
	key, err := security.ParseApprovalKey(h.Approval.Key)
	if err != nil {
		return nil, "", "", &ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: err}
	}
	pin, err := security.ApprovalKeyPin(h.Approval.Key)
	if err != nil {
		return nil, "", "", &ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: err}
	}
	if pin != h.Approval.Pin {
		return nil, "", "", &ApprovalInvalidError{Problem: fmt.Sprintf("the phone's approval key has pin %s, not the %s it claims", pin, h.Approval.Pin)}
	}
	if c.opts.approvalTrust == nil {
		return nil, "", "", ErrNoApprovalTrust
	}
	level, err := c.opts.approvalTrust(pin, func() (string, error) { return c.AttestApprovalKey(ctx) })
	if err != nil {
		return nil, "", "", err
	}
	if err := c.checkStrongBox("approval", level); err != nil {
		return nil, "", "", err
	}
	return key, pin, level, nil
}

// ApprovalRequest is what RequestApproval asks for.
type ApprovalRequest struct {
	// Reason is shown on the phone and signed exactly as given.
	Reason string
	// Program names who asks, shown on the phone as "<Program> on <PC>";
	// "" sends DefaultProgram.
	Program string
	// Timeout is how long the user has; 0 means 30 s, allowed 5 s to 2 min.
	Timeout time.Duration
	// Nonce is base64url of 32 bytes, which must never be reused; "" makes
	// a fresh one. A program that picks the nonce can verify the signature
	// itself, without trusting whoever relayed it.
	Nonce string
	// ShowMatch shows the user the number to pick on the phone
	// (APPROVAL.md §4.2). It is required, and called once, right before the
	// request is sent; it must not block.
	ShowMatch func(match int)
}

// RequestApproval asks the phone's user to approve req.Reason with a
// fingerprint (APPROVAL.md §4), after picking the number req.ShowMatch
// shows, and waits for the answer.
//
// The result is approved only if the phone signed this request's fresh
// nonce, reason, program, number, expiry, phone id and PC id with the
// trusted approval key. Not approved (denied, cancelled, mismatch,
// timeout, busy, cooldown, lockout, unavailable) is a result, not an
// error. Errors: invalid arguments; an *ApprovalKeyChangedError or a
// *KeyAttestationError, in which case nothing was sent; an
// *ApprovalInvalidError for an approval that failed verification, or,
// with nothing sent, for a phone whose hello ids differ from the stored
// pairing; and connection problems.
func (c *Client) RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
	nonce := req.Nonce
	switch {
	case c.opts.testNonce != "":
		nonce = c.opts.testNonce // tests replaying signed vectors only
	case nonce == "":
		nonce = security.B64(security.RandomBytes(protocol.ApprovalNonceSize))
	}
	if n, err := security.DecodeB64(nonce); err != nil || len(n) != protocol.ApprovalNonceSize {
		return ApprovalResult{}, fmt.Errorf("phonetopc: the nonce must be base64url of %d bytes", protocol.ApprovalNonceSize)
	}
	// APPROVAL.md §4: the reason is sent and signed exactly as given, never
	// trimmed; check it before sending, as the phone would.
	if err := protocol.CheckReason(req.Reason); err != nil {
		return ApprovalResult{}, fmt.Errorf("phonetopc: %w", err)
	}
	program, timeout, err := checkAsk(req.Program, req.Timeout, req.ShowMatch, "approval")
	if err != nil {
		return ApprovalResult{}, err
	}
	res := ApprovalResult{Reason: req.Reason, Program: program}

	h := c.Server()
	if h.PC == nil {
		return res, ErrApprovalNeedsSecureMode
	}
	// §5: sign over the ids stored at pairing; a phone reporting others
	// gets no approvals, so a signature always names this pairing.
	phoneID, pcID := h.Device.ID, h.PC.ID
	if c.opts.pairedPhoneID != "" {
		if phoneID != c.opts.pairedPhoneID || pcID != c.opts.pairedPCID {
			return res, &ApprovalInvalidError{Problem: fmt.Sprintf(
				"the phone paired as %s with this PC as %s reports %s and %s; re-pair to use approvals",
				c.opts.pairedPhoneID, c.opts.pairedPCID, phoneID, pcID)}
		}
	}
	res.PhoneID, res.PCID = phoneID, pcID
	key, pin, level, err := c.approvalKey(ctx)
	switch {
	case err == ErrNoApprovalKey:
		// §4: no capability, or capability without hello.approval (or the
		// reverse): unavailable, and nothing is sent.
		res.Why = WhyUnavailable
		return res, nil
	case err != nil:
		return res, err
	}
	res.Key, res.KeyPin, res.Attestation = h.Approval.Key, pin, level
	res.Nonce = nonce
	res.Match = c.match()
	res.Expiry = c.now().Add(timeout + protocol.ExpiryAfterTimeout).UnixMilli()
	ms := timeout.Milliseconds()
	params := protocol.RequestApprovalParams{
		Reason: req.Reason, Program: program, Nonce: nonce, Match: res.Match, Expiry: res.Expiry, TimeoutMS: &ms,
	}
	req.ShowMatch(res.Match)
	var r protocol.RequestApprovalResult
	if err := c.callAsk(ctx, protocol.MethodRequestApproval, params, timeout, &r); err != nil {
		return res, err
	}
	if !r.Approved {
		res.Why = r.Why
		if res.Why == "" {
			res.Why = WhyDenied // malformed, but certainly not an approval
		}
		return res, nil
	}
	if r.Signature == "" {
		return res, &ApprovalInvalidError{Problem: "the phone said approved without a signature"}
	}
	// §5: built from our own values, never echoed ones.
	msg := security.Approval{
		PhoneID: phoneID, PCID: pcID, Nonce: nonce, Reason: req.Reason,
		Program: program, Match: res.Match, Expiry: res.Expiry,
	}.Message()
	if err := security.VerifyApproval(key, r.Signature, msg); err != nil {
		return res, &ApprovalInvalidError{Problem: "the phone said approved, but the signature is invalid", Err: err}
	}
	res.approved = true
	res.Signature = r.Signature
	return res, nil
}

// checkAsk checks what request_approval and decrypt have in common, and
// returns the program to send and the timeout.
func checkAsk(program string, timeout time.Duration, show func(int), what string) (string, time.Duration, error) {
	if program == "" {
		program = DefaultProgram
	}
	if err := protocol.CheckProgram(program); err != nil {
		return "", 0, fmt.Errorf("phonetopc: %w", err)
	}
	if show == nil {
		return "", 0, errors.New("phonetopc: ShowMatch is required: the user must see the number to pick on the phone")
	}
	if timeout == 0 {
		timeout = DefaultApprovalTimeout
	}
	if timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		return "", 0, fmt.Errorf("phonetopc: %s timeout %v is outside %v–%v", what, timeout, MinApprovalTimeout, MaxApprovalTimeout)
	}
	return program, timeout.Truncate(time.Millisecond), nil
}

// match picks the number the user must pick (APPROVAL.md §4).
func (c *Client) match() int {
	if c.opts.testMatch != 0 {
		return c.opts.testMatch
	}
	return protocol.MinMatch + rand.IntN(protocol.MaxMatch-protocol.MinMatch+1)
}

func (c *Client) now() time.Time {
	if c.opts.testNow != nil {
		return c.opts.testNow()
	}
	return time.Now()
}

// callAsk calls request_approval or decrypt, which answer once the user
// acted, and decodes the result into res.
func (c *Client) callAsk(ctx context.Context, method string, params any, timeout time.Duration, res any) error {
	// §4: never give up before timeout_ms + 5 s; allow a little more.
	wait := timeout + protocol.ApprovalResponseLeeway + approvalWaitSlack
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var raw json.RawMessage
	err := c.Call(cctx, method, params, &raw)
	switch {
	case err == nil:
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		// §4: our own wait running out is an error, not a why.
		return fmt.Errorf("%w: no answer from the phone within %v", ErrApprovalNoAnswer, wait)
	default:
		return err
	}
	if err := json.Unmarshal(raw, res); err != nil {
		return fmt.Errorf("phonetopc: decoding the %s result: %w", method, err)
	}
	return nil
}
