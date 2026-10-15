package protocol

import (
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// Fingerprint approval (spec/APPROVAL.md).

// §4.
const MethodRequestApproval = "request_approval"

// CapabilityApproval is listed in hello when the phone can do approvals (§1).
const CapabilityApproval = "approval"

// ApprovalInfo is the approval member of the hello result (§1): the
// approval key as base64url DER SubjectPublicKeyInfo, and its pin.
type ApprovalInfo struct {
	Key string `json:"key"`
	Pin string `json:"pin"`
}

// §4.
type RequestApprovalParams struct {
	Reason string `json:"reason"`
	Nonce  string `json:"nonce"`
	// TimeoutMS is optional (default 30000). A pointer, so that absent can
	// be told apart from 0.
	TimeoutMS *int64 `json:"timeout_ms,omitempty"`
}

// §4.1.
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

// In the order of §4.1.
var Whys = []Why{WhyDenied, WhyCancelled, WhyTimeout, WhyBusy, WhyLockout, WhyUnavailable}

func (w Why) Valid() bool {
	for _, x := range Whys {
		if w == x {
			return true
		}
	}
	return false
}

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

// CheckReason checks a reason exactly as received, as §4 requires: 1–200
// Unicode code points, no leading or trailing whitespace (Unicode
// White_Space), no control characters (Cc) and no bidirectional controls.
// Nobody trims or normalizes a reason: it is shown, sent and signed as is.
func CheckReason(reason string) error {
	if !utf8.ValidString(reason) {
		return fmt.Errorf("reason is not valid UTF-8")
	}
	n := utf8.RuneCountInString(reason)
	switch {
	case n == 0:
		return fmt.Errorf("reason must not be empty")
	case n > MaxReasonLen:
		return fmt.Errorf("reason has %d characters, at most %d are allowed", n, MaxReasonLen)
	}
	first, _ := utf8.DecodeRuneInString(reason)
	last, _ := utf8.DecodeLastRuneInString(reason)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return fmt.Errorf("reason must not start or end with whitespace")
	}
	for _, r := range reason {
		if unicode.Is(unicode.Cc, r) {
			return fmt.Errorf("reason must not contain control characters (%U)", r)
		}
		if isBidiControl(r) {
			return fmt.Errorf("reason must not contain bidirectional controls (%U)", r)
		}
	}
	return nil
}

// isBidiControl reports the bidirectional controls §4 forbids in a reason.
func isBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}
