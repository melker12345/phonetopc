package mockphone

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Fingerprint approval (spec/APPROVAL.md), with a software P-256 key in
// place of the phone's biometric-bound Keystore key, and an Approver in
// place of the user.

// ApprovalRequest is what the phone's user would see.
type ApprovalRequest struct {
	// Decrypt is set for a decrypt request (DECRYPT.md §4).
	Decrypt bool
	PC      protocol.PCInfo
	Program string
	Reason  string
	// Choices are the numbers offered to pick from (APPROVAL.md §4.2), in
	// the order shown; Match is the right one, which a real user reads on
	// the PC.
	Choices [3]int
	Match   int
	Timeout time.Duration
	Remote  string // the PC's address
}

// Approver plays the phone's user. It returns "" to pick Match and
// approve, or why not: protocol.WhyMismatch for a wrong pick. ctx is done
// when the request times out or the session closes; the return value is
// then ignored.
type Approver func(ctx context.Context, req ApprovalRequest) protocol.Why

func ApproveAll(context.Context, ApprovalRequest) protocol.Why { return "" }

func Answer(why protocol.Why) Approver {
	return func(context.Context, ApprovalRequest) protocol.Why { return why }
}

// NeverAnswer is a user who ignores the prompt: every request times out.
func NeverAnswer(ctx context.Context, _ ApprovalRequest) protocol.Why {
	<-ctx.Done()
	return protocol.WhyTimeout
}

// MaxWaitingApprovals is how many PCs' approvals may wait behind the one
// shown; more are answered busy.
const MaxWaitingApprovals = 8

type approvalSlot struct {
	pc   string
	turn chan struct{} // closed when the approval is shown
}

// beginApproval returns nil if the PC already has an approval pending or
// the queue is full (§4). Otherwise the slot's turn comes after those of
// the PCs before it.
func (s *Server) beginApproval(pcID string) *approvalSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approvalShown != nil && s.approvalShown.pc == pcID ||
		slices.ContainsFunc(s.approvalWaiting, func(w *approvalSlot) bool { return w.pc == pcID }) {
		return nil
	}
	slot := &approvalSlot{pc: pcID, turn: make(chan struct{})}
	switch {
	case s.approvalShown == nil:
		s.approvalShown = slot
		close(slot.turn)
	case len(s.approvalWaiting) < MaxWaitingApprovals:
		s.approvalWaiting = append(s.approvalWaiting, slot)
	default:
		return nil
	}
	return slot
}

// endApproval is idempotent. If slot was shown, the next one gets its turn.
func (s *Server) endApproval(slot *approvalSlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approvalShown != slot {
		s.approvalWaiting = slices.DeleteFunc(s.approvalWaiting, func(w *approvalSlot) bool { return w == slot })
		return
	}
	s.approvalShown = nil
	if len(s.approvalWaiting) > 0 {
		s.approvalShown = s.approvalWaiting[0]
		s.approvalWaiting = s.approvalWaiting[1:]
		close(s.approvalShown.turn)
	}
}

// ask waits for slot's turn, then asks the user. If ctx ends first, the
// user never sees the request.
func (s *Server) ask(ctx context.Context, slot *approvalSlot, req ApprovalRequest) protocol.Why {
	select {
	case <-slot.turn:
	case <-ctx.Done():
		return ""
	}
	approve := s.opts.Approve
	if approve == nil {
		approve = ApproveAll
	}
	req.Choices = choices(s.newRand(), req.Match)
	return approve(ctx, req)
}

// choices returns match and two other numbers of 10–99, all different, in
// random order (APPROVAL.md §4.2).
func choices(r *rand.Rand, match int) [3]int {
	c := [3]int{match}
	for i := 1; i < 3; {
		n := protocol.MinMatch + r.IntN(protocol.MaxMatch-protocol.MinMatch+1)
		if !slices.Contains(c[:i], n) {
			c[i] = n
			i++
		}
	}
	r.Shuffle(3, func(i, j int) { c[i], c[j] = c[j], c[i] })
	return c
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// cooldown is a PC's recent refusals (APPROVAL.md §4.3).
type cooldown struct {
	refusals []time.Time
	until    time.Time
}

func (s *Server) inCooldown(pcID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cooldowns[pcID]
	return c != nil && s.now().Before(c.until)
}

// noteOutcome counts why towards pcID's cool-down.
func (s *Server) noteOutcome(pcID string, why protocol.Why) {
	if !why.CountsForCooldown() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cooldowns[pcID]
	if c == nil {
		c = &cooldown{}
		s.cooldowns[pcID] = c
	}
	now := s.now()
	c.refusals = slices.DeleteFunc(c.refusals, func(t time.Time) bool { return now.Sub(t) >= protocol.CooldownWindow })
	c.refusals = append(c.refusals, now)
	if len(c.refusals) >= protocol.CooldownRefusals {
		c.refusals = nil
		c.until = now.Add(protocol.CooldownDuration)
		s.logf("mock: PC %s was refused %d times; cool-down until %s", pcID, protocol.CooldownRefusals, c.until.Format(time.TimeOnly))
	}
}

// approvalKey is nil if the phone can't do approvals (dev mode, or none
// configured).
func (s *Server) approvalKey() *ecdsa.PrivateKey {
	if !s.Secure() || s.opts.ApprovalKey == nil {
		return nil
	}
	return s.opts.ApprovalKey()
}

func (s *Server) approvalInfo(key *ecdsa.PrivateKey) *protocol.ApprovalInfo {
	if key == nil {
		return nil
	}
	k, pin, err := security.EncodeApprovalKey(&key.PublicKey)
	if err != nil {
		return nil
	}
	return &protocol.ApprovalInfo{Key: k, Pin: pin, Attestation: s.attestation(k, attest.PurposeSign)}
}

func (s *Server) attestation(key string, purpose attest.Purpose) []string {
	if s.opts.Attest == nil {
		return nil
	}
	spki, _ := security.DecodeB64(key)
	return s.opts.Attest(spki, purpose)
}

// TestAttestation returns an Options.Attest that attests keys as a phone's
// Keystore does, bound to the phone's TLS pin, but with ca as the root: only
// a PC given ca.Root accepts it. tweak, if set, changes what is attested.
// Chains are made once per key.
func TestAttestation(ca *attesttest.CA, tlsPin string, tweak func(*attesttest.Description)) func([]byte, attest.Purpose) []string {
	var (
		mu    sync.Mutex
		cache = map[string][]string{}
	)
	return func(spki []byte, purpose attest.Purpose) []string {
		mu.Lock()
		defer mu.Unlock()
		id := string(spki) + strconv.Itoa(int(purpose))
		if c, ok := cache[id]; ok {
			return c
		}
		pub, err := x509.ParsePKIXPublicKey(spki)
		if err != nil {
			return nil
		}
		d := attesttest.Good(security.PinDigest(tlsPin), purpose)
		if tweak != nil {
			tweak(&d)
		}
		ders, err := ca.Chain(pub, d)
		if err != nil {
			return nil
		}
		chain := make([]string, len(ders))
		for i, der := range ders {
			chain[i] = security.B64(der)
		}
		cache[id] = chain
		return chain
	}
}

// requestApproval handles request_approval (§4). Valid requests that can
// be shown get their answer later: then async is true and the caller sends
// nothing now; the answer follows once the Approver decided or the timeout
// passed, and if the session closes first, the prompt is cancelled and
// nothing is sent.
func (s *session) requestApproval(id json.RawMessage, params json.RawMessage) (result any, async bool, rpcErr *protocol.Error) {
	var p struct {
		Reason    *string         `json:"reason"`
		Program   *string         `json:"program"`
		Nonce     *string         `json:"nonce"`
		Match     json.RawMessage `json:"match"`
		Expiry    json.RawMessage `json:"expiry"`
		TimeoutMS json.RawMessage `json:"timeout_ms"`
	}
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, false, err
	}
	req, rpcErr := parseShown(p.Reason, p.Program, p.Match)
	if rpcErr != nil {
		return nil, false, rpcErr
	}
	if p.Nonce == nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.nonce is required")
	}
	if n, err := security.DecodeB64(*p.Nonce); err != nil || len(n) != protocol.ApprovalNonceSize {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.nonce must be base64url of %d bytes", protocol.ApprovalNonceSize)
	}
	timeout, rpcErr := parseTimeout(p.TimeoutMS)
	if rpcErr != nil {
		return nil, false, rpcErr
	}
	expiry, ok := parseInt(p.Expiry)
	if !ok {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.expiry is required, an integer")
	}
	if err := protocol.CheckExpiry(expiry, timeout, s.srv.now()); err != nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.%v", err)
	}

	s.mu.Lock()
	key, pc := s.approvalKey, s.pc
	s.mu.Unlock()
	if key == nil || pc == nil {
		return protocol.RequestApprovalResult{Why: protocol.WhyUnavailable}, false, nil
	}
	if s.srv.inCooldown(pc.ID) {
		return protocol.RequestApprovalResult{Why: protocol.WhyCooldown}, false, nil
	}
	slot := s.srv.beginApproval(pc.ID)
	if slot == nil {
		return protocol.RequestApprovalResult{Why: protocol.WhyBusy}, false, nil
	}
	req.PC, req.Timeout, req.Remote = *pc, timeout, s.remote
	signed := security.Approval{
		PhoneID: s.srv.opts.Device.ID, PCID: pc.ID, Nonce: *p.Nonce,
		Reason: req.Reason, Program: req.Program, Match: req.Match, Expiry: expiry,
	}
	s.srv.logf("mock: %s: %s on PC %q asks for approval: %q (pick %d, timeout %v)", s.remote, req.Program, pc.Name, req.Reason, req.Match, timeout)
	s.srv.wg.Go(func() {
		end := func() { s.srv.endApproval(slot) }
		defer end()
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		why := s.srv.ask(ctx, slot, req)
		switch {
		case s.ctx.Err() != nil:
			s.srv.logf("mock: %s: session closed, approval prompt cancelled", s.remote)
			return
		case ctx.Err() != nil:
			why = protocol.WhyTimeout
		}
		s.srv.noteOutcome(pc.ID, why)
		res := protocol.RequestApprovalResult{Why: why}
		if why == "" {
			sig, err := security.SignApproval(key, signed.Message())
			if err != nil {
				s.srv.logf("mock: signing: %v", err)
				res.Why = protocol.WhyUnavailable
			} else {
				res = protocol.RequestApprovalResult{Approved: true, Signature: sig}
				if s.srv.opts.TamperApprovals {
					res.Signature = tamper(sig)
				}
			}
		}
		s.srv.logf("mock: %s: approval answered: approved=%v %s", s.remote, res.Approved, res.Why)
		end() // before the answer, so the PC may ask again at once
		s.send(protocol.NewResult(id, res))
	})
	return nil, true, nil
}

// parseShown checks what request_approval and decrypt show the user:
// reason, program and match.
func parseShown(reason, program *string, match json.RawMessage) (ApprovalRequest, *protocol.Error) {
	if reason == nil {
		return ApprovalRequest{}, protocol.NewError(protocol.CodeInvalidParams, "params.reason is required")
	}
	if err := protocol.CheckReason(*reason); err != nil {
		return ApprovalRequest{}, protocol.NewError(protocol.CodeInvalidParams, "params.%v", err)
	}
	if program == nil {
		return ApprovalRequest{}, protocol.NewError(protocol.CodeInvalidParams, "params.program is required")
	}
	if err := protocol.CheckProgram(*program); err != nil {
		return ApprovalRequest{}, protocol.NewError(protocol.CodeInvalidParams, "params.%v", err)
	}
	m, ok := parseInt(match)
	if !ok || protocol.CheckMatch(int(m)) != nil {
		return ApprovalRequest{}, protocol.NewError(protocol.CodeInvalidParams,
			"params.match is required, an integer %d-%d", protocol.MinMatch, protocol.MaxMatch)
	}
	return ApprovalRequest{Reason: *reason, Program: *program, Match: int(m)}, nil
}

// parseInt accepts an integer literal only: no fraction, exponent, string
// or null.
func parseInt(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	return n, err == nil
}

// parseTimeout checks timeout_ms of request_approval and decrypt.
func parseTimeout(raw json.RawMessage) (time.Duration, *protocol.Error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return protocol.DefaultApprovalTimeout, nil
	}
	ms, ok := parseInt(raw)
	if !ok {
		return 0, protocol.NewError(protocol.CodeInvalidParams, "params.timeout_ms must be an integer")
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout < protocol.MinApprovalTimeout || timeout > protocol.MaxApprovalTimeout {
		return 0, protocol.NewError(protocol.CodeInvalidParams, "params.timeout_ms must be %d-%d",
			protocol.MinApprovalTimeout.Milliseconds(), protocol.MaxApprovalTimeout.Milliseconds())
	}
	return timeout, nil
}

// tamper corrupts a base64url signature so that it no longer verifies
// (a test hook for clients).
func tamper(sig string) string {
	b, err := security.DecodeB64(sig)
	if err != nil || len(b) == 0 {
		return sig + "A"
	}
	b[len(b)-1] ^= 0x01
	return security.B64(b)
}
