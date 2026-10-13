package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/hostlimit"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// This file holds one phone connection to the Listener: the pairing state
// machine of SECURITY.md §4 and its method handlers.

type state int

const (
	stStart     state = iota
	stCommitted       // pair_sas_begin answered
	stCoded           // pair_sas_nonce answered, codes shown
	stConfirmed       // pair_qr confirmed, or both SAS confirmations
	stDone
)

// session is one phone connection to the listener.
type session struct {
	l      *Listener
	conn   *websocket.Conn
	remote string
	ctx    context.Context // done when the phone is gone or Serve stops

	state      state
	way        Way
	phone      PhoneInfo
	npc        []byte
	revealed   bool              // npc was sent: the run counts if it fails
	asked      bool              // pair_qr reached the prompt: the secret goes if it fails
	steps      int               // requests that advanced the pairing or reached a user
	answer     chan promptAnswer // PC user's SAS answer, buffered(1)
	stopPrompt context.CancelFunc
}

type promptAnswer struct {
	ok  bool
	err error
}

// errGone means the session ended while a handler waited.
var errGone = errors.New("session gone")

func (s *session) run() {
	// ctx ends waits (prompts) when the phone goes away or Serve stops. The
	// reader uses its own context: cancelling a Read's context closes the
	// connection, and on expiry we still want to answer -32024.
	ctx, cancel := context.WithCancel(s.l.ctx)
	defer cancel()
	s.ctx = ctx
	defer s.reset()
	readCtx, stopReading := context.WithCancel(context.Background())
	defer stopReading()

	frames := make(chan []byte)
	go func() {
		defer cancel()
		for {
			typ, data, err := s.conn.Read(readCtx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				s.conn.Close(websocket.StatusUnsupportedData, "binary frames are not supported")
				return
			}
			select {
			case frames <- data:
			case <-readCtx.Done():
				return
			}
		}
	}()

	// The timer restarts only when the pairing advances, so junk, wrong
	// secrets and repeated errors can't hold the one pairing slot (§4.1).
	idle := time.NewTimer(s.l.opts.StartTimeout)
	defer idle.Stop()
	for {
		select {
		case <-s.l.expired:
			s.conn.Close(websocket.StatusGoingAway, "pairing expired")
			return
		case <-ctx.Done():
			s.conn.Close(websocket.StatusGoingAway, "pairing listener closed")
			return
		case <-idle.C:
			s.conn.Close(websocket.StatusGoingAway, "idle for too long")
			return
		case data := <-frames:
			steps := s.steps
			resp, after, closeCode := s.handle(data)
			if s.steps != steps {
				if s.state == stStart { // after a refusal: start over
					idle.Reset(s.l.opts.StartTimeout)
				} else {
					idle.Reset(s.l.opts.IdleTimeout)
				}
			}
			if resp != nil && !s.write(resp) {
				return
			}
			if closeCode != 0 {
				reason := map[websocket.StatusCode]string{
					websocket.StatusNormalClosure:   "paired",
					websocket.StatusPolicyViolation: "too many failed pairing attempts",
					websocket.StatusGoingAway:       "pairing expired",
				}[closeCode]
				s.conn.Close(closeCode, reason)
				if after != nil {
					after()
				}
				return
			}
			if after != nil {
				after()
			}
		}
	}
}

func (s *session) write(resp *protocol.Response) bool {
	b, err := json.Marshal(resp)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, b) == nil
}

// reset discards everything learned in this session (§4.3: "both sides
// discard everything"). It runs after a failed attempt and when the
// connection ends; a SAS run that revealed Npc and didn't pair counts
// towards MaxSASRuns either way, and a QR run that reached the prompt and
// didn't pair uses up the QR secret.
func (s *session) reset() {
	if s.stopPrompt != nil {
		s.stopPrompt()
		s.stopPrompt = nil
	}
	if s.state != stDone {
		if s.revealed {
			s.l.sasFailed()
		}
		if s.asked {
			s.l.rotateSecret()
		}
	}
	s.state, s.way, s.phone, s.npc, s.revealed, s.asked, s.answer = stStart, "", PhoneInfo{}, nil, false, false, nil
}

// handle processes one frame. It returns the response (nil for none), a
// function to run once it is sent, and a close code (0 to stay open).
func (s *session) handle(data []byte) (*protocol.Response, func(), websocket.StatusCode) {
	// Framing is PROTOCOL.md §2 steps 1-4, as SECURITY.md §4.1 asks.
	id, method, params, errResp := protocol.ParseRequest(data)
	if errResp != nil {
		return errResp, nil, 0
	}
	if id == nil {
		return nil, nil, 0 // a notification: ignored
	}
	result, after, rpcErr := s.dispatch(method, params)
	switch {
	case errors.Is(rpcErr, errGone):
		return nil, nil, websocket.StatusGoingAway
	case rpcErr != nil:
		var pe *protocol.Error
		if !errors.As(rpcErr, &pe) {
			pe = protocol.NewError(protocol.CodeInternalError, "%v", rpcErr)
		}
		var code websocket.StatusCode
		switch pe.Code {
		case protocol.CodeExpired:
			code = websocket.StatusGoingAway
		case protocol.CodeBadSecret, protocol.CodeRejected, protocol.CodeSASMismatch:
			host := hostlimit.Host(s.remote)
			s.l.failures.Fail(host)
			s.reset()
			if s.l.failures.Blocked(host) {
				code = websocket.StatusPolicyViolation
			}
		}
		return protocol.NewErrorResponse(id, pe), nil, code
	}
	var code websocket.StatusCode
	if s.state == stDone {
		code = websocket.StatusNormalClosure
	}
	return protocol.NewResult(id, result), after, code
}

func (s *session) dispatch(method string, params json.RawMessage) (any, func(), error) {
	want := map[string]state{
		MethodQR:         stStart,
		MethodSASBegin:   stStart,
		MethodSASNonce:   stCommitted,
		MethodSASConfirm: stCoded,
		MethodFinish:     stConfirmed,
	}
	st, known := want[method]
	if !known {
		return nil, nil, protocol.NewError(protocol.CodeMethodNotFound, "method %q not found", method)
	}
	if s.state != st {
		return nil, nil, protocol.NewError(protocol.CodeOutOfOrder, "%s is not valid at this step of the pairing", method)
	}
	handler := map[string]func(json.RawMessage) (any, func(), error){
		MethodQR:         s.pairQR,
		MethodSASBegin:   s.sasBegin,
		MethodSASNonce:   s.sasNonce,
		MethodSASConfirm: s.sasConfirm,
		MethodFinish:     s.finish,
	}[method]
	result, after, err := handler(params)
	// A step counts if it succeeded or a user said no; malformed requests,
	// wrong secrets and the like don't.
	if code, _ := protocol.ErrorCodeOf(err); err == nil || code == protocol.CodeRejected || code == protocol.CodeSASMismatch {
		s.steps++
	}
	return result, after, err
}

func checkProtocol(p string) error {
	if p == "" {
		return protocol.NewError(protocol.CodeInvalidParams, "params.protocol is required")
	}
	if !slices.Contains(SupportedProtocols, p) {
		e := protocol.NewError(protocol.CodeUnsupportedProtocol, "pairing protocol %q is not supported", p)
		e.Data.Supported = SupportedProtocols
		return e
	}
	return nil
}

func invalid(err error) error { return protocol.NewError(protocol.CodeInvalidParams, "%v", err) }

// waitErr turns a cancelled wait into the right answer: -32024 if the
// listener expired, else errGone (nothing to answer).
func (s *session) waitErr() error {
	select {
	case <-s.l.expired:
		return protocol.NewError(protocol.CodeExpired, "the pairing listener timed out")
	default:
		return errGone
	}
}

func (s *session) pairQR(params json.RawMessage) (any, func(), error) {
	var p QRParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if b, err := security.DecodeB64(p.Secret); err != nil || len(b) != security.SecretSize {
		return nil, nil, invalid(fmt.Errorf("params.secret must be base64url of %d bytes", security.SecretSize))
	}
	if err := p.Phone.validate(); err != nil {
		return nil, nil, invalid(err)
	}
	if err := checkProtocol(p.Protocol); err != nil {
		return nil, nil, err
	}
	if !security.Equal(p.Secret, s.l.Secret()) {
		s.l.logf("wrong pairing secret from %s", s.remote)
		return nil, nil, protocol.NewError(protocol.CodeBadSecret, "wrong pairing secret")
	}
	s.asked = true
	ok, err := s.l.opts.Prompt.ConfirmQR(s.ctx, p.Phone)
	if err != nil {
		if s.ctx.Err() != nil {
			return nil, nil, s.waitErr()
		}
		s.reset()
		return nil, nil, err
	}
	if !ok {
		return nil, nil, protocol.NewError(protocol.CodeRejected, "the PC user declined the pairing")
	}
	s.state, s.way, s.phone = stConfirmed, WayQR, p.Phone
	return PCResult{PC: s.l.opts.PC}, nil, nil
}

func (s *session) sasBegin(params json.RawMessage) (any, func(), error) {
	var p SASBeginParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if err := p.Phone.validate(); err != nil {
		return nil, nil, invalid(err)
	}
	if err := checkProtocol(p.Protocol); err != nil {
		return nil, nil, err
	}
	if s.l.opts.NoSAS {
		s.l.logf("refused a code comparison from %s: this PC only accepts the QR code now", s.remote)
		return nil, nil, protocol.NewError(protocol.CodeRejected, "this PC only accepts pairing by QR code right now")
	}
	if s.l.sasExhausted() { // the listener is stopping
		return nil, nil, protocol.NewError(protocol.CodeExpired, "too many code comparisons failed; pairing stopped")
	}
	s.npc = security.RandomBytes(security.NonceSize)
	s.state, s.way, s.phone = stCommitted, WaySAS, p.Phone
	return SASBeginResult{PC: s.l.opts.PC, Commitment: security.Commitment(s.npc)}, nil, nil
}

func (s *session) sasNonce(params json.RawMessage) (any, func(), error) {
	var p NonceMessage
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	nphone, err := security.DecodeB64(p.Nonce)
	if err != nil || len(nphone) != security.NonceSize {
		return nil, nil, invalid(fmt.Errorf("params.nonce must be base64url of %d bytes", security.NonceSize))
	}
	code := security.SASCode(s.l.pin, s.phone.Pin, s.npc, nphone)
	s.state, s.revealed = stCoded, true
	ctx, cancel := context.WithCancel(s.ctx)
	s.stopPrompt = cancel
	answer := make(chan promptAnswer, 1)
	s.answer = answer
	phone := s.phone
	// Ask the PC user once the nonce has been sent, while the phone's user
	// compares too.
	after := func() {
		go func() {
			ok, err := s.l.opts.Prompt.ConfirmSAS(ctx, phone, code)
			answer <- promptAnswer{ok, err}
		}()
	}
	return NonceMessage{Nonce: security.B64(s.npc)}, after, nil
}

func (s *session) sasConfirm(params json.RawMessage) (any, func(), error) {
	var p SASConfirmParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if p.Match == nil {
		return nil, nil, invalid(errors.New("params.match is required"))
	}
	if !*p.Match {
		s.l.logf("the phone reports that the codes differ; pairing aborted")
		return nil, nil, protocol.NewError(protocol.CodeSASMismatch, "the phone user said the codes differ")
	}
	select {
	case a := <-s.answer:
		if a.err != nil {
			if s.ctx.Err() != nil {
				return nil, nil, s.waitErr()
			}
			return nil, nil, a.err
		}
		if !a.ok {
			return nil, nil, protocol.NewError(protocol.CodeSASMismatch, "the PC user said the codes differ")
		}
	case <-s.ctx.Done():
		return nil, nil, s.waitErr()
	}
	s.state = stConfirmed
	return struct{}{}, nil, nil
}

func (s *session) finish(params json.RawMessage) (any, func(), error) {
	var p FinishParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if !security.ValidToken(p.Token) {
		return nil, nil, invalid(fmt.Errorf("params.token must be base64url of %d bytes", security.TokenSize))
	}
	host, _, err := net.SplitHostPort(s.remote)
	if err != nil {
		host = ""
	}
	paired := Paired{Phone: s.phone, Token: p.Token, Way: s.way, RemoteHost: host}
	// §4.4: store first, then answer.
	if err := s.l.opts.Save(paired); err != nil {
		return nil, nil, protocol.NewError(protocol.CodeInternalError, "the PC could not store the pairing: %v", err)
	}
	s.state = stDone
	after := func() {
		select {
		case s.l.result <- paired:
		default:
		}
	}
	return PCResult{PC: s.l.opts.PC}, after, nil
}
