package mockphone

import (
	"context"
	"crypto/rsa"
	"encoding/json"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Decryption (spec/DECRYPT.md), with a software RSA key in place of the
// phone's biometric-bound Keystore key. It shares the approvals' pending
// slot and Approver.

// decryptKey is nil if the phone can't decrypt (dev mode, or none set).
func (s *Server) decryptKey() *rsa.PrivateKey {
	if !s.Secure() || s.opts.DecryptKey == nil {
		return nil
	}
	return s.opts.DecryptKey()
}

func decryptInfo(key *rsa.PrivateKey) *protocol.DecryptInfo {
	if key == nil {
		return nil
	}
	k, pin, err := security.EncodeDecryptKey(&key.PublicKey)
	if err != nil {
		return nil
	}
	return &protocol.DecryptInfo{Key: k, Pin: pin}
}

// decrypt handles decrypt (§4) like requestApproval handles
// request_approval.
func (s *session) decrypt(id json.RawMessage, params json.RawMessage) (result any, async bool, rpcErr *protocol.Error) {
	var p struct {
		Reason     *string         `json:"reason"`
		KeyPin     *string         `json:"key_pin"`
		Ciphertext *string         `json:"ciphertext"`
		TimeoutMS  json.RawMessage `json:"timeout_ms"`
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
	if p.KeyPin == nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.key_pin is required")
	}
	if p.Ciphertext == nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.ciphertext is required")
	}
	ct, err := security.DecodeB64(*p.Ciphertext)
	if err != nil {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.ciphertext must be base64url")
	}
	timeout, rpcErr := parseTimeout(p.TimeoutMS)
	if rpcErr != nil {
		return nil, false, rpcErr
	}

	s.mu.Lock()
	key, pc := s.decryptKey, s.pc
	s.mu.Unlock()
	if key != nil && len(ct) != key.Size() {
		return nil, false, protocol.NewError(protocol.CodeInvalidParams, "params.ciphertext must be %d bytes", key.Size())
	}
	if key == nil || pc == nil || decryptInfo(key).Pin != *p.KeyPin {
		return protocol.DecryptResult{Why: protocol.WhyUnavailable}, false, nil
	}
	if !s.srv.beginApproval() {
		return protocol.DecryptResult{Why: protocol.WhyBusy}, false, nil
	}
	req := ApprovalRequest{Decrypt: true, PC: *pc, Reason: *p.Reason, Timeout: timeout, Remote: s.remote}
	s.srv.logf("mock: %s: PC %q asks to decrypt: %q (timeout %v)", s.remote, pc.Name, req.Reason, timeout)
	s.srv.wg.Go(func() {
		ended := false
		end := func() {
			if !ended {
				ended = true
				s.srv.endApproval()
			}
		}
		defer end()
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		approve := s.srv.opts.Approve
		if approve == nil {
			approve = ApproveAll
		}
		why := approve(ctx, req)
		switch {
		case s.ctx.Err() != nil:
			s.srv.logf("mock: %s: session closed, decrypt prompt cancelled", s.remote)
			return
		case ctx.Err() != nil:
			why = protocol.WhyTimeout
		}
		res := protocol.DecryptResult{Why: why}
		if why == "" {
			if pt, err := security.DecryptAsPhone(key, ct); err != nil {
				res.Why = protocol.WhyFailed
			} else {
				res = protocol.DecryptResult{Decrypted: true, Plaintext: security.B64(pt)}
			}
		}
		s.srv.logf("mock: %s: decrypt answered: decrypted=%v %s", s.remote, res.Decrypted, res.Why)
		end()
		s.send(protocol.NewResult(id, res))
	})
	return nil, true, nil
}
