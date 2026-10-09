package client

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	WhyTimeout     = protocol.WhyTimeout
	WhyBusy        = protocol.WhyBusy
	WhyLockout     = protocol.WhyLockout
	WhyUnavailable = protocol.WhyUnavailable
)

// Limits of RequestApproval's timeout (APPROVAL.md §4).
const (
	DefaultApprovalTimeout = protocol.DefaultApprovalTimeout
	MinApprovalTimeout     = protocol.MinApprovalTimeout
	MaxApprovalTimeout     = protocol.MaxApprovalTimeout
)

// ApprovalResult is the outcome of RequestApproval.
type ApprovalResult struct {
	approved bool
	// Why says why the approval was not given; empty when approved.
	Why Why
	// Reason is the reason as sent, Nonce the nonce generated for this
	// request.
	Reason, Nonce string
	// Signature is the verified signature (base64url DER) when approved.
	Signature string
	// KeyPin is the pin of the approval key that signed.
	KeyPin string
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

// WithApprovalKey makes RequestApproval of a Dial'd client accept only the
// approval key with this pin. DialPaired uses devices.json instead.
func WithApprovalKey(pin string) Option {
	return WithApprovalTrust(func(current string) error {
		if current != pin {
			return &ApprovalKeyChangedError{Stored: pin, Current: current}
		}
		return nil
	})
}

// WithApprovalTrust sets the function deciding whether the approval key
// pin of this session is trusted (APPROVAL.md §3): it returns nil to
// trust it, typically an *ApprovalKeyChangedError otherwise.
func WithApprovalTrust(f func(pin string) error) Option {
	return func(o *options) { o.approvalTrust = f }
}

// storeApprovalTrust is the trust of DialPaired: first sight stores the
// pin in devices.json; a different stored pin is a change.
func storeApprovalTrust(st *store.Store, dev store.Device) func(string) error {
	return func(pin string) error {
		stored, err := st.TrustApprovalKey(dev.ID, pin)
		if err != nil {
			return fmt.Errorf("phonetopc: approval key: %w", err)
		}
		if stored != "" && stored != pin {
			return &ApprovalKeyChangedError{Device: dev.Name, DeviceID: dev.ID, Stored: stored, Current: pin}
		}
		return nil
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
	_, _, err = c.approvalKey()
	if err == errNoApprovalOffered {
		return false, nil
	}
	return true, err
}

var errNoApprovalOffered = errors.New("no approvals offered")

// approvalKey returns this session's trusted approval key and its pin.
func (c *Client) approvalKey() (*ecdsa.PublicKey, string, error) {
	h := c.Server()
	if !h.HasCapability(protocol.CapabilityApproval) || h.Approval == nil {
		return nil, "", errNoApprovalOffered
	}
	key, err := security.ParseApprovalKey(h.Approval.Key)
	if err != nil {
		return nil, "", &ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: err}
	}
	pin, err := security.ApprovalKeyPin(h.Approval.Key)
	if err != nil {
		return nil, "", &ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: err}
	}
	if pin != h.Approval.Pin {
		return nil, "", &ApprovalInvalidError{Problem: fmt.Sprintf("the phone's approval key has pin %s, not the %s it claims", pin, h.Approval.Pin)}
	}
	if c.opts.approvalTrust == nil {
		return nil, "", ErrNoApprovalTrust
	}
	if err := c.opts.approvalTrust(pin); err != nil {
		return nil, "", err
	}
	return key, pin, nil
}

// RequestApproval asks the phone's user to approve reason with a
// fingerprint (APPROVAL.md §4) and waits up to timeout (0 means 30 s;
// allowed 5 s to 2 min) for the answer.
//
// The result is approved only if the phone signed this request's fresh
// nonce, reason, phone id and PC id with the trusted approval key. Not
// approved (denied, cancelled, timeout, busy, lockout, unavailable) is a
// result, not an error. Errors: invalid arguments; an
// *ApprovalKeyChangedError, in which case nothing was sent; an
// *ApprovalInvalidError for an approval that failed verification; and
// connection problems.
func (c *Client) RequestApproval(ctx context.Context, reason string, timeout time.Duration) (ApprovalResult, error) {
	// APPROVAL.md §4: the reason is sent and signed exactly as given, never
	// trimmed; check it before sending, as the phone would.
	if err := protocol.CheckReason(reason); err != nil {
		return ApprovalResult{}, fmt.Errorf("phonetopc: %w", err)
	}
	if timeout == 0 {
		timeout = DefaultApprovalTimeout
	}
	if timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		return ApprovalResult{}, fmt.Errorf("phonetopc: approval timeout %v is outside %v–%v", timeout, MinApprovalTimeout, MaxApprovalTimeout)
	}
	timeout = timeout.Truncate(time.Millisecond)
	res := ApprovalResult{Reason: reason}

	h := c.Server()
	if h.PC == nil {
		return res, ErrApprovalNeedsSecureMode
	}
	key, pin, err := c.approvalKey()
	switch {
	case err == errNoApprovalOffered:
		// §4: no capability, or capability without hello.approval (or the
		// reverse): unavailable, and nothing is sent.
		res.Why = WhyUnavailable
		return res, nil
	case err != nil:
		return res, err
	}
	res.KeyPin = pin

	res.Nonce = security.B64(security.RandomBytes(protocol.ApprovalNonceSize))
	if c.opts.testNonce != "" {
		res.Nonce = c.opts.testNonce // tests replaying signed vectors only
	}
	ms := timeout.Milliseconds()
	params := protocol.RequestApprovalParams{Reason: reason, Nonce: res.Nonce, TimeoutMS: &ms}
	// §4: never give up before timeout_ms + 5 s; allow a little more.
	wait := timeout + protocol.ApprovalResponseLeeway + approvalWaitSlack
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var raw json.RawMessage
	err = c.Call(cctx, protocol.MethodRequestApproval, params, &raw)
	switch {
	case err == nil:
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		// §4: our own wait running out is an error, not a why.
		return res, fmt.Errorf("%w: no answer from the phone within %v", ErrApprovalNoAnswer, wait)
	default:
		return res, err
	}
	var r protocol.RequestApprovalResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return res, fmt.Errorf("phonetopc: decoding request_approval result: %w", err)
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
	// §5: built from our own nonce, reason and ids, never echoed values.
	msg := security.ApprovalMessage(h.Device.ID, h.PC.ID, res.Nonce, reason)
	if err := security.VerifyApproval(key, r.Signature, msg); err != nil {
		return res, &ApprovalInvalidError{Problem: "the phone said approved, but the signature is invalid", Err: err}
	}
	res.approved = true
	res.Signature = r.Signature
	return res, nil
}
