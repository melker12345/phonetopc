package mockphone

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"slices"
	"strconv"
	"time"

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
	Reason  string
	Timeout time.Duration
	Remote  string // the PC's address
}

// Approver plays the phone's user. It returns "" to approve, or why not.
// ctx is done when the request times out or the session closes; the
// return value is then ignored.
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
	return approve(ctx, req)
}

// approvalKey is nil if the phone can't do approvals (dev mode, or none
// configured).
func (s *Server) approvalKey() *ecdsa.PrivateKey {
	if !s.Secure() || s.opts.ApprovalKey == nil {
		return nil
	}
	return s.opts.ApprovalKey()
}

func approvalInfo(key *ecdsa.PrivateKey) *protocol.ApprovalInfo {
	if key == nil {
		return nil
	}
	k, pin, err := security.EncodeApprovalKey(&key.PublicKey)
	if err != nil {
		return nil
	}
	return &protocol.ApprovalInfo{Key: k, Pin: pin}
}

// requestApproval handles request_approval (§4). Valid requests that can
// be shown get their answer later: then async is true and the caller sends
// nothing now; the answer follows once the Approver decided or the timeout
// passed, and if the session closes first, the prompt is cancelled and
// nothing is sent.
func (s *session) requestApproval(id json.RawMessage, params json.RawMessage) (result any, async bool, rpcErr *protocol.Error) {
	var p struct {
		Reason    *string         `json:"reason"`
		Nonce     *string         `json:"nonce"`
		TimeoutMS json.RawMessage `json:"timeout_ms"`
	}
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, false, err
	}
	if p.Reason == nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.reason is required")
	}
	if err := protocol.CheckReason(*p.Reason); err != nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.%v", err)
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

	s.mu.Lock()
	key, pc := s.approvalKey, s.pc
	s.mu.Unlock()
	if key == nil || pc == nil {
		return protocol.RequestApprovalResult{Why: protocol.WhyUnavailable}, false, nil
	}
	slot := s.srv.beginApproval(pc.ID)
	if slot == nil {
		return protocol.RequestApprovalResult{Why: protocol.WhyBusy}, false, nil
	}
	req := ApprovalRequest{PC: *pc, Reason: *p.Reason, Timeout: timeout, Remote: s.remote}
	nonce := *p.Nonce
	s.srv.logf("mock: %s: PC %q asks for approval: %q (timeout %v)", s.remote, pc.Name, req.Reason, timeout)
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
		res := protocol.RequestApprovalResult{Why: why}
		if why == "" {
			msg := security.ApprovalMessage(s.srv.opts.Device.ID, pc.ID, nonce, req.Reason)
			sig, err := security.SignApproval(key, msg)
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

// parseTimeout checks timeout_ms of request_approval and decrypt.
func parseTimeout(raw json.RawMessage) (time.Duration, *protocol.Error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return protocol.DefaultApprovalTimeout, nil
	}
	// An integer literal only: no fraction, exponent or null.
	ms, err := strconv.ParseInt(string(t), 10, 64)
	if err != nil {
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
