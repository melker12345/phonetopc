package protocol

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Fingerprint approval (spec/APPROVAL.md).

// MethodRequestApproval asks the phone's user for an approval (§4).
const MethodRequestApproval = "request_approval"

// CapabilityApproval is listed in hello when the phone can do approvals (§1).
const CapabilityApproval = "approval"

// ApprovalInfo is the approval member of the hello result (§1): the
// approval key as base64url DER SubjectPublicKeyInfo, and its pin.
type ApprovalInfo struct {
	Key string `json:"key"`
	Pin string `json:"pin"`
}

// RequestApprovalParams are the params of request_approval (§4).
type RequestApprovalParams struct {
	Reason string `json:"reason"`
	Nonce  string `json:"nonce"`
	// TimeoutMS is optional (default 30000). A pointer, so that absent can
	// be told apart from 0.
	TimeoutMS *int64 `json:"timeout_ms,omitempty"`
}

// RequestApprovalResult is the result of request_approval (§4.1).
type RequestApprovalResult struct {
	Approved  bool   `json:"approved"`
	Signature string `json:"signature,omitempty"`
	Why       Why    `json:"why,omitempty"`
}

// Why says why an approval was not given (§4.1).
type Why string

const (
	WhyDenied      Why = "denied"      // the user pressed Deny
	WhyCancelled   Why = "cancelled"   // the user dismissed the prompt or notification
	WhyTimeout     Why = "timeout"     // timeout_ms passed without an answer
	WhyBusy        Why = "busy"        // another approval was pending
	WhyLockout     Why = "lockout"     // too many failed fingerprint attempts
	WhyUnavailable Why = "unavailable" // no strong biometric enrolled, or no approval key
)

// Whys lists every defined Why, in the order of §4.1.
var Whys = []Why{WhyDenied, WhyCancelled, WhyTimeout, WhyBusy, WhyLockout, WhyUnavailable}

// Valid reports whether w is one of the defined values.
func (w Why) Valid() bool {
	for _, x := range Whys {
		if w == x {
			return true
		}
	}
	return false
}

// Describe explains w for people.
func (w Why) Describe() string {
	switch w {
	case WhyDenied:
		return "the user pressed Deny"
	case WhyCancelled:
		return "the user dismissed the prompt"
	case WhyTimeout:
		return "nobody answered in time"
	case WhyBusy:
		return "another approval is pending on the phone"
	case WhyLockout:
		return "too many failed fingerprint attempts; biometrics are locked for now"
	case WhyUnavailable:
		return "the phone can't do approvals (no strong biometric enrolled, or no approval key)"
	}
	return string(w)
}

// Limits of request_approval params (§4).
const (
	MaxReasonLen           = 200
	DefaultApprovalTimeout = 30 * time.Second
	MinApprovalTimeout     = 5 * time.Second
	MaxApprovalTimeout     = 120 * time.Second
	ApprovalNonceSize      = 32
	ApprovalResponseLeeway = 5 * time.Second // §4: clients wait at least timeout_ms + 5 s
)

// CheckReason checks a reason as §4 requires: 1–200 characters after
// trimming, and no control characters anywhere (so not even a trailing
// newline). Characters are counted as Unicode code points. It doesn't trim;
// callers send the reason exactly as checked.
func CheckReason(reason string) error {
	if !utf8.ValidString(reason) {
		return fmt.Errorf("reason is not valid UTF-8")
	}
	n := utf8.RuneCountInString(strings.TrimSpace(reason))
	switch {
	case n == 0:
		return fmt.Errorf("reason must not be empty")
	case n > MaxReasonLen:
		return fmt.Errorf("reason has %d characters, at most %d are allowed", n, MaxReasonLen)
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return fmt.Errorf("reason must not contain control characters (%U)", r)
		}
	}
	return nil
}
