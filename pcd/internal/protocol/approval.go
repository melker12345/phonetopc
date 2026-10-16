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
// approval key as base64url DER SubjectPublicKeyInfo, its pin, and its
// attestation chain (§3.1), base64url DER certificates, leaf first.
type ApprovalInfo struct {
	Key         string   `json:"key"`
	Pin         string   `json:"pin"`
	Attestation []string `json:"attestation,omitempty"`
}

// §4.
type RequestApprovalParams struct {
	Reason  string `json:"reason"`
	Program string `json:"program"`
	Nonce   string `json:"nonce"`
	Match   int    `json:"match"`
	// Expiry is in Unix milliseconds.
	Expiry int64 `json:"expiry"`
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
	WhyMismatch    Why = "mismatch"    // the user picked a number other than match
	WhyTimeout     Why = "timeout"     // timeout_ms passed without an answer
	WhyBusy        Why = "busy"        // another approval was pending
	WhyCooldown    Why = "cooldown"    // this PC was refused too often lately (§4.3)
	WhyLockout     Why = "lockout"     // too many failed fingerprint attempts
	WhyUnavailable Why = "unavailable" // no strong biometric enrolled, or no approval key
)

// In the order of §4.1.
var Whys = []Why{WhyDenied, WhyCancelled, WhyMismatch, WhyTimeout, WhyBusy, WhyCooldown, WhyLockout, WhyUnavailable}

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
	case WhyMismatch:
		return "the user picked another number than the one shown on the PC"
	case WhyTimeout:
		return "nobody answered in time"
	case WhyBusy:
		return "another approval is pending on the phone"
	case WhyCooldown:
		return "this PC's requests were refused 3 times within 10 minutes; the phone ignores it for 5 minutes"
	case WhyLockout:
		return "too many failed fingerprint attempts; biometrics are locked for now"
	case WhyUnavailable:
		return "the phone can't do approvals (no strong biometric enrolled, or no approval key)"
	case WhyFailed:
		return "the ciphertext doesn't decrypt with the phone's key"
	}
	return string(w)
}

// Limits of request_approval params (§4).
const (
	MaxReasonLen           = 200
	MaxProgramLen          = 60
	MinMatch, MaxMatch     = 10, 99
	DefaultApprovalTimeout = 30 * time.Second
	MinApprovalTimeout     = 5 * time.Second
	MaxApprovalTimeout     = 120 * time.Second
	ApprovalNonceSize      = 32
	ApprovalResponseLeeway = 5 * time.Second // §4: clients wait at least timeout_ms + 5 s
	// ExpiryAfterTimeout is how long after timeout_ms a PC's expiry is.
	ExpiryAfterTimeout = 10 * time.Second
	// A phone refuses an expiry further than timeout_ms + MaxExpiryAfterTimeout
	// ahead of its clock, or ExpiryClockTolerance further either way.
	MaxExpiryAfterTimeout = 60 * time.Second
	ExpiryClockTolerance  = 5 * time.Minute
	// DefaultProgram is what a PC sends as program when it can't tell
	// which program asks.
	DefaultProgram = "a program"
)

// Cool-down of §4.3: CooldownRefusals refusals within CooldownWindow
// start a cool-down of CooldownDuration.
const (
	CooldownRefusals = 3
	CooldownWindow   = 10 * time.Minute
	CooldownDuration = 5 * time.Minute
)

// CountsForCooldown reports the outcomes that count towards a cool-down.
func (w Why) CountsForCooldown() bool {
	return w == WhyDenied || w == WhyCancelled || w == WhyMismatch
}

// CheckExpiry checks expiry (Unix ms) against the phone's clock now, as §4
// requires.
func CheckExpiry(expiry int64, timeout time.Duration, now time.Time) error {
	ms := now.UnixMilli()
	switch {
	case expiry < ms-ExpiryClockTolerance.Milliseconds():
		return fmt.Errorf("expiry is %v behind the phone's clock; check that the PC's and the phone's clocks are right",
			time.Duration(ms-expiry)*time.Millisecond)
	case expiry > ms+(timeout+MaxExpiryAfterTimeout+ExpiryClockTolerance).Milliseconds():
		return fmt.Errorf("expiry is %v ahead of the phone's clock, more than timeout_ms + 60 s; check that the PC's and the phone's clocks are right",
			time.Duration(expiry-ms)*time.Millisecond)
	}
	return nil
}

// CheckMatch checks a match number (§4).
func CheckMatch(match int) error {
	if match < MinMatch || match > MaxMatch {
		return fmt.Errorf("match must be %d-%d", MinMatch, MaxMatch)
	}
	return nil
}

// CheckReason checks a reason exactly as received, as §4 requires: 1–200
// Unicode code points, no leading or trailing whitespace (Unicode
// White_Space), no control characters (Cc) and no hidden characters.
// Nobody trims or normalizes a reason: it is shown, sent and signed as is.
func CheckReason(reason string) error { return checkShown("reason", reason, MaxReasonLen) }

// CheckProgram checks a program name exactly as received: the rules of
// CheckReason, with at most 60 code points.
func CheckProgram(program string) error { return checkShown("program", program, MaxProgramLen) }

func checkShown(name, text string, maxLen int) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	n := utf8.RuneCountInString(text)
	switch {
	case n == 0:
		return fmt.Errorf("%s must not be empty", name)
	case n > maxLen:
		return fmt.Errorf("%s has %d characters, at most %d are allowed", name, n, maxLen)
	}
	first, _ := utf8.DecodeRuneInString(text)
	last, _ := utf8.DecodeLastRuneInString(text)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return fmt.Errorf("%s must not start or end with whitespace", name)
	}
	for _, r := range text {
		if unicode.Is(unicode.Cc, r) {
			return fmt.Errorf("%s must not contain control characters (%U)", name, r)
		}
		if IsHidden(r) {
			return fmt.Errorf("%s must not contain bidirectional controls, invisible characters or line separators (%U)", name, r)
		}
	}
	return nil
}

// IsHidden reports the characters APPROVAL.md §4 and PANELS.md §8 forbid in
// display text besides Cc: bidirectional controls, which reorder what is
// shown, and invisible characters and line separators, which hide or move
// parts of it. A fixed list rather than a Unicode category, so every
// implementation agrees whatever its Unicode version. ZWNJ and ZWJ
// (U+200C, U+200D) stay allowed: scripts and emoji need them.
func IsHidden(r rune) bool {
	switch {
	case r == 0x061C, r == 0x200E, r == 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // bidi
		return true
	case r == 0x00AD, r == 0x180E, r == 0x200B, r >= 0x2060 && r <= 0x2064, r >= 0x206A && r <= 0x206F,
		r == 0xFEFF, r >= 0xFFF9 && r <= 0xFFFB, r >= 0xE0000 && r <= 0xE007F: // invisible
		return true
	case r == 0x2028, r == 0x2029: // line and paragraph separators
		return true
	}
	return false
}
