package mockphone

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/hostlimit"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

const writeTimeout = 10 * time.Second

// session is one client connection.
//
// Goroutines: run (the reader, which also handles requests one at a time),
// a writer draining out, a pinger, and one generator per continuous
// subscription. Everything sent goes through the single FIFO out queue, so
// a response is never overtaken by an event produced after it was queued.
type session struct {
	srv    *Server
	conn   *websocket.Conn
	remote string
	ctx    context.Context
	cancel context.CancelFunc
	out    chan []byte

	helloed  bool // only used by the reader goroutine
	rejected bool // only used by the reader goroutine: unauthorized, closing

	helloDone chan struct{} // closed on the first successful hello
	helloOnce sync.Once

	mu        sync.Mutex
	subs      map[string]*subscription
	nextSub   int
	closing   bool
	closeDone chan struct{} // closed when a closeWith handshake has finished
	pc        *protocol.PCInfo
	token     string
	// approvalKey is the approval key announced in this session's hello.
	approvalKey *ecdsa.PrivateKey

	// pendingClose is the close the writer performs when it dequeues the
	// nil frame queued by closeAfterFlush.
	pendingCode   websocket.StatusCode
	pendingReason string
}

func newSession(srv *Server, conn *websocket.Conn, remote string) *session {
	// Not derived from srv.ctx: on server shutdown we want to send a close
	// frame first (see run), not abort the connection.
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		srv:       srv,
		conn:      conn,
		remote:    remote,
		ctx:       ctx,
		cancel:    cancel,
		out:       make(chan []byte, srv.opts.QueueSize),
		subs:      make(map[string]*subscription),
		closeDone: make(chan struct{}),
		helloDone: make(chan struct{}),
	}
}

// auth returns the PC this session authenticated as.
func (s *session) auth() (protocol.PCInfo, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pc == nil {
		return protocol.PCInfo{}, "", false
	}
	return *s.pc, s.token, true
}

// closeAfterFlush closes the connection with code once everything queued so
// far (in particular the response just queued) has been written.
func (s *session) closeAfterFlush(code websocket.StatusCode, reason string) {
	s.mu.Lock()
	s.pendingCode, s.pendingReason = code, reason
	s.mu.Unlock()
	select {
	case s.out <- nil:
	case <-s.ctx.Done():
	}
}

// helloTimeout closes a secure session that hasn't completed hello in time
// (SECURITY.md §5).
func (s *session) helloTimeout() {
	d := s.srv.opts.HelloTimeout
	if !s.srv.Secure() || d < 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		s.srv.logf("mock: %s: no successful hello within %v, closing", s.remote, d)
		s.closeWith(websocket.StatusPolicyViolation, "no successful hello in time")
	case <-s.helloDone:
	case <-s.ctx.Done():
	}
}

func (s *session) run() {
	s.srv.logf("mock: %s connected", s.remote)
	// Requests are small; anything bigger is a misbehaving client and gets
	// closed with 1009 by the library.
	s.conn.SetReadLimit(64 << 10)

	var wg sync.WaitGroup
	wg.Go(s.writeLoop)
	wg.Go(s.pingLoop)
	wg.Go(s.helloTimeout)
	wg.Go(func() {
		select {
		case <-s.srv.ctx.Done():
			s.closeWith(websocket.StatusGoingAway, "server shutting down")
		case <-s.ctx.Done():
		}
	})
	err := s.readLoop()
	s.cancel()
	s.stopAll()
	s.srv.panels.sessionClosed(s)
	wg.Wait()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		<-s.closeDone // let the close frame go out before tearing down
	}
	s.conn.CloseNow()
	if code := websocket.CloseStatus(err); code != -1 {
		s.srv.logf("mock: %s disconnected (close code %d)", s.remote, code)
	} else {
		s.srv.logf("mock: %s disconnected: %v", s.remote, err)
	}
}

func (s *session) readLoop() error {
	for {
		typ, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			// Spec §1: binary frames are reserved; close with 1003.
			s.closeWith(websocket.StatusUnsupportedData, "binary frames are not supported")
			return errors.New("binary frame received")
		}
		if t := s.srv.opts.Tap; t != nil {
			t(ClientToServer, data)
		}
		if s.rejected {
			continue // unauthorized: closing, nothing more is answered
		}
		resp, after := s.handle(data)
		if resp == nil {
			continue // a client notification, or a request answered later
		}
		if !s.send(resp) {
			return s.ctx.Err()
		}
		if after != nil {
			after()
		}
	}
}

// closeWith starts a close handshake with the given code without blocking
// the caller. The reader then sees the peer's close frame and ends the
// session.
func (s *session) closeWith(code websocket.StatusCode, reason string) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	s.mu.Unlock()
	go func() {
		s.conn.Close(code, reason)
		s.cancel()
		close(s.closeDone)
	}()
}

func (s *session) writeLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.out:
			if frame == nil {
				s.mu.Lock()
				code, reason := s.pendingCode, s.pendingReason
				s.mu.Unlock()
				s.closeWith(code, reason)
				return
			}
			if t := s.srv.opts.Tap; t != nil {
				t(ServerToClient, frame)
			}
			ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
			err := s.conn.Write(ctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *session) pingLoop() {
	t := time.NewTicker(s.srv.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.ctx, s.srv.opts.PingTimeout)
			err := s.conn.Ping(ctx)
			cancel()
			if err != nil && s.ctx.Err() == nil {
				s.srv.logf("mock: %s: no pong within %v, closing", s.remote, s.srv.opts.PingTimeout)
				s.cancel()
				s.conn.CloseNow()
				return
			}
		}
	}
}

// send queues a response, waiting for room. It reports false if the session
// is over.
func (s *session) send(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		s.srv.logf("mock: marshal: %v", err)
		return false
	}
	select {
	case s.out <- b:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// trySend queues an event without waiting. It reports false if there is no
// room for it.
//
// Spec §5 asks servers to keep a backlog of continuous events from causing
// the on_change overflow close. Responses and events share one FIFO here (so
// a response always precedes the events it announces), so instead continuous
// events may only fill continuousLimit slots and the rest of the queue is
// reserved for on_change events and responses. The length check and the send
// are not atomic, so concurrent generators can overshoot the limit by a few
// slots; that only eats into the reserve.
func (s *session) trySend(b []byte, continuous bool) bool {
	if continuous && len(s.out) >= continuousLimit(cap(s.out)) {
		return false
	}
	select {
	case s.out <- b:
		return true
	default:
		return false
	}
}

// continuousLimit is how many slots of a queue of size n continuous events
// may occupy: three quarters, but at least one.
func continuousLimit(n int) int {
	return max(1, n-n/4)
}

func (s *session) stopAll() {
	s.mu.Lock()
	subs := s.subs
	s.subs = map[string]*subscription{}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
}

// handle processes one text frame and returns the response to send, plus an
// optional function to run after the response has been queued. resp is nil
// when nothing is to be sent now: for a client notification, which gets no
// reply, and for a request answered later (request_approval).
func (s *session) handle(data []byte) (resp *protocol.Response, after func()) {
	id, method, params, errResp := protocol.ParseRequest(data)
	if errResp != nil {
		return errResp, nil
	}
	// Spec §2: an object without id is a client notification. None are
	// defined, so it is ignored without a reply, whatever else it contains.
	if id == nil {
		return nil, nil
	}

	// Spec §3: any request other than hello before a successful hello is
	// rejected, including unknown methods.
	if method != protocol.MethodHello && !s.helloed {
		return protocol.NewErrorResponse(id, protocol.NewError(protocol.CodeHelloRequired, "send hello first")), nil
	}

	var (
		result any
		rpcErr *protocol.Error
	)
	switch method {
	case protocol.MethodHello:
		wasHelloed := s.helloed
		result, rpcErr = s.hello(params)
		switch {
		case rpcErr == nil:
		case rpcErr.Code == protocol.CodeUnauthorized:
			// SECURITY.md §5: -32010, then close with 1008.
			s.rejected = true
			s.srv.authFailures.Fail(hostlimit.Host(s.remote))
			after = func() { s.closeAfterFlush(websocket.StatusPolicyViolation, "unauthorized") }
		case wasHelloed && s.srv.Secure():
			// SECURITY.md §5: a failed hello after a successful one also
			// closes the connection with 1008.
			s.rejected = true
			after = func() { s.closeAfterFlush(websocket.StatusPolicyViolation, "failed hello") }
		}
	case protocol.MethodListSensors:
		// list_sensors takes no params, but if present they must still be an
		// object (spec §2); its members are ignored.
		if rpcErr = protocol.DecodeParams(params, &struct{}{}); rpcErr == nil {
			result = protocol.ListSensorsResult{Sensors: s.srv.sensors()}
		}
	case protocol.MethodSubscribe:
		result, after, rpcErr = s.subscribe(params)
	case protocol.MethodUnsubscribe:
		result, rpcErr = s.unsubscribe(params)
	case protocol.MethodGet:
		result, rpcErr = s.get(params)
	case protocol.MethodRequestApproval:
		if !s.srv.Secure() {
			// APPROVAL.md §1: in dev mode the method doesn't exist.
			rpcErr = protocol.NewError(protocol.CodeMethodNotFound, "method %q not found (approvals need secure mode)", method)
			break
		}
		var async bool
		result, async, rpcErr = s.requestApproval(id, params)
		if async {
			return nil, nil // answered later, by requestApproval
		}
	case protocol.MethodNotify:
		result, rpcErr = s.notify(params)
	case protocol.MethodNotifyCancel:
		result, rpcErr = s.notifyCancel(params)
	case protocol.MethodPanelShow:
		result, after, rpcErr = s.panelShow(params)
	case protocol.MethodPanelAttach:
		result, after, rpcErr = s.panelAttach(params)
	case protocol.MethodPanelUpdate:
		result, rpcErr = s.panelUpdate(params)
	case protocol.MethodPanelClose:
		result, rpcErr = s.panelClose(params)
	case protocol.MethodPanelList:
		result, rpcErr = s.panelList(params)
	case protocol.MethodCanvasClear:
		result, rpcErr = s.canvasClear(params)
	default:
		rpcErr = protocol.NewError(protocol.CodeMethodNotFound, "method %q not found", method)
	}
	if rpcErr != nil {
		return protocol.NewErrorResponse(id, rpcErr), after // after is only set on error for a closing hello
	}
	return protocol.NewResult(id, result), after
}

func (s *session) hello(params json.RawMessage) (any, *protocol.Error) {
	var p struct {
		Protocol string               `json:"protocol"`
		Client   *protocol.ClientInfo `json:"client"`
		Auth     json.RawMessage      `json:"auth"`
	}
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Protocol == "" {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "params.protocol is required")
	}
	// Secure mode: every problem with auth (absent, null, not an object, no
	// or empty or unknown token) is -32010, checked after the params shape
	// and before the protocol version.
	var pc *protocol.PCInfo
	var token string
	if s.srv.Secure() {
		var a struct {
			Token *string `json:"token"`
		}
		auth := bytes.TrimSpace(p.Auth)
		if len(auth) == 0 || auth[0] != '{' || json.Unmarshal(auth, &a) != nil || a.Token == nil || *a.Token == "" {
			return nil, protocol.NewError(protocol.CodeUnauthorized, "hello needs auth.token in secure mode; pair this PC first")
		}
		info, ok := s.srv.opts.Authenticate(*a.Token)
		if !ok {
			return nil, protocol.NewError(protocol.CodeUnauthorized, "unknown or revoked token; pair this PC again")
		}
		pc, token = &info, *a.Token
	}
	if !slices.Contains(protocol.SupportedVersions, p.Protocol) {
		e := protocol.NewError(protocol.CodeUnsupportedProtocol, "protocol %q is not supported", p.Protocol)
		e.Data.Supported = protocol.SupportedVersions
		// In dev mode a failed hello does not undo an earlier successful
		// one; in secure mode the caller closes the connection.
		return nil, e
	}
	// A repeated hello is allowed and simply answered again.
	s.helloed = true
	s.helloOnce.Do(func() { close(s.helloDone) })
	var approval *protocol.ApprovalInfo
	if pc != nil {
		key := s.srv.approvalKey()
		approval = approvalInfo(key)
		s.mu.Lock()
		s.pc, s.token, s.approvalKey = pc, token, key
		s.mu.Unlock()
		s.srv.logf("mock: %s authenticated as PC %q (%s)", s.remote, pc.Name, pc.ID)
	}
	if p.Client != nil && p.Client.Name != "" {
		s.srv.logf("mock: %s is %s %s", s.remote, p.Client.Name, p.Client.Version)
	}
	return s.srv.helloResult(pc, approval), nil
}

// lookup resolves source and an optional type against the catalog. This is
// step 8 of the order of checks (spec §2), after params were validated:
// unknown source, then a type the source doesn't have, then availability.
func (s *session) lookup(source, typ string) (protocol.SensorInfo, *protocol.Error) {
	info, ok := s.srv.sensor(source)
	if !ok {
		return info, protocol.NewError(protocol.CodeUnknownSource, "unknown source %q", source)
	}
	if typ != "" && !info.HasType(typ) {
		return info, protocol.NewError(protocol.CodeInvalidParams, "source %q has no type %q (types: %s)", source, typ, strings.Join(info.Types, ", "))
	}
	if !info.Available {
		return info, protocol.NewError(protocol.CodeSourceUnavailable, "source %q is not available on this device", source)
	}
	return info, nil
}

// requireSource checks that the required source param is present.
func requireSource(source string) *protocol.Error {
	if source == "" {
		return protocol.NewError(protocol.CodeInvalidParams, "params.source is required")
	}
	return nil
}

func (s *session) subscribe(params json.RawMessage) (any, func(), *protocol.Error) {
	var p protocol.SubscribeParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if err := requireSource(p.Source); err != nil {
		return nil, nil, err
	}
	// rate_hz is validated for any source, even on_change ones where it is
	// then ignored (spec §4.3), and before the catalog lookup (spec §2).
	if p.RateHz != nil && !(*p.RateHz > 0) {
		return nil, nil, protocol.NewError(protocol.CodeInvalidParams, "params.rate_hz must be > 0")
	}
	if p.MinDelta != nil && !(*p.MinDelta >= 0) {
		return nil, nil, protocol.NewError(protocol.CodeInvalidParams, "params.min_delta must be >= 0")
	}
	info, rpcErr := s.lookup(p.Source, p.Type)
	if rpcErr != nil {
		return nil, nil, rpcErr
	}

	types := info.Types
	if p.Type != "" {
		types = []string{p.Type}
	}
	sub := &subscription{
		sess:  s,
		info:  info,
		types: types,
		rate:  info.ClampRate(p.RateHz),
	}
	if p.MinDelta != nil {
		sub.minDelta = *p.MinDelta
	}
	s.mu.Lock()
	s.nextSub++
	sub.id = "s-" + strconv.Itoa(s.nextSub)
	s.subs[sub.id] = sub
	s.mu.Unlock()

	res := protocol.SubscribeResult{Subscription: sub.id, Source: info.Source, Types: types, Mode: info.Mode, RateHz: sub.rate}
	// Start producing only after the response is queued, so it precedes the
	// subscription's first event.
	return res, sub.start, nil
}

func (s *session) unsubscribe(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.UnsubscribeParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Subscription == "" {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "params.subscription is required")
	}
	s.mu.Lock()
	sub, ok := s.subs[p.Subscription]
	delete(s.subs, p.Subscription)
	s.mu.Unlock()
	if !ok {
		return nil, protocol.NewError(protocol.CodeUnknownSubscription, "no subscription %q in this session", p.Subscription)
	}
	// After close returns no further event of sub can be queued, so none
	// follows the response.
	sub.close()
	return protocol.UnsubscribeResult{}, nil
}

func (s *session) get(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.GetParams
	if err := protocol.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := requireSource(p.Source); err != nil {
		return nil, err
	}
	info, rpcErr := s.lookup(p.Source, p.Type)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if info.Mode == protocol.ModeContinuous {
		r, err := s.srv.sampleContinuous(info.Source, time.Now(), s.srv.newRand())
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternalError, "%v", err)
		}
		return r, nil
	}
	r, ok := s.srv.world.get(info.Source)
	if !ok {
		return nil, protocol.NewError(protocol.CodeNoValue, "no value for %q yet", info.Source)
	}
	return r, nil
}
