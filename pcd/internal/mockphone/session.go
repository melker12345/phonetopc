package mockphone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

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

	helloed bool // only used by the reader goroutine

	mu        sync.Mutex
	subs      map[string]*subscription
	nextSub   int
	closing   bool
	closeDone chan struct{} // closed when a closeWith handshake has finished
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
		resp, after := s.handle(data)
		if resp == nil {
			continue // a client notification: no reply
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

// trySend queues an event without waiting. It reports false if the queue is
// full.
func (s *session) trySend(b []byte) bool {
	select {
	case s.out <- b:
		return true
	default:
		return false
	}
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
// for a client notification, which gets no reply.
func (s *session) handle(data []byte) (resp *protocol.Response, after func()) {
	fail := func(id json.RawMessage, code protocol.ErrorCode, format string, args ...any) (*protocol.Response, func()) {
		return protocol.NewErrorResponse(id, protocol.NewError(code, format, args...)), nil
	}

	if !json.Valid(data) {
		return fail(nil, protocol.CodeParseError, "parse error: frame is not valid JSON")
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if trimmed[0] == '[' {
		return fail(nil, protocol.CodeInvalidRequest, "batch requests are not supported")
	}
	if trimmed[0] != '{' {
		return fail(nil, protocol.CodeInvalidRequest, "request must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fail(nil, protocol.CodeInvalidRequest, "invalid request: %v", err)
	}

	id, hasID := fields["id"]
	// Spec §2: an object without id is a client notification. None are
	// defined, so it is ignored without a reply, whatever else it contains.
	if !hasID {
		return nil, nil
	}
	if !protocol.ValidRequestID(id) {
		return fail(nil, protocol.CodeInvalidRequest, "id must be an integer or a string")
	}
	var version string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != protocol.JSONRPCVersion {
		return fail(id, protocol.CodeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	var method string
	if json.Unmarshal(fields["method"], &method) != nil || method == "" {
		return fail(id, protocol.CodeInvalidRequest, "method must be a non-empty string")
	}
	params := fields["params"]

	// Spec §3: any request other than hello before a successful hello is
	// rejected, including unknown methods.
	if method != protocol.MethodHello && !s.helloed {
		return fail(id, protocol.CodeHelloRequired, "send hello first")
	}

	var (
		result any
		rpcErr *protocol.Error
	)
	switch method {
	case protocol.MethodHello:
		result, rpcErr = s.hello(params)
	case protocol.MethodListSensors:
		// list_sensors takes no params, but if present they must still be an
		// object (spec §2); its members are ignored.
		if rpcErr = decodeParams(params, &struct{}{}); rpcErr == nil {
			result = protocol.ListSensorsResult{Sensors: s.srv.sensors()}
		}
	case protocol.MethodSubscribe:
		result, after, rpcErr = s.subscribe(params)
	case protocol.MethodUnsubscribe:
		result, rpcErr = s.unsubscribe(params)
	case protocol.MethodGet:
		result, rpcErr = s.get(params)
	default:
		rpcErr = protocol.NewError(protocol.CodeMethodNotFound, "method %q not found", method)
	}
	if rpcErr != nil {
		return protocol.NewErrorResponse(id, rpcErr), nil
	}
	return protocol.NewResult(id, result), after
}

// decodeParams decodes params into v. Absent or null params decode as {}.
func decodeParams(params json.RawMessage, v any) *protocol.Error {
	p := bytes.TrimSpace(params)
	if len(p) == 0 || string(p) == "null" {
		p = []byte("{}")
	}
	if p[0] != '{' {
		return protocol.NewError(protocol.CodeInvalidParams, "params must be an object")
	}
	if err := json.Unmarshal(p, v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return protocol.NewError(protocol.CodeInvalidParams, "params.%s has the wrong type (%s)", te.Field, te.Value)
		}
		return protocol.NewError(protocol.CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

func (s *session) hello(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.HelloParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Protocol == "" {
		return nil, protocol.NewError(protocol.CodeInvalidParams, "params.protocol is required")
	}
	if !slices.Contains(protocol.SupportedVersions, p.Protocol) {
		e := protocol.NewError(protocol.CodeUnsupportedProtocol, "protocol %q is not supported", p.Protocol)
		e.Data.Supported = protocol.SupportedVersions
		// A failed hello does not undo an earlier successful one.
		return nil, e
	}
	// A repeated hello is allowed and simply answered again.
	s.helloed = true
	if p.Client != nil && p.Client.Name != "" {
		s.srv.logf("mock: %s is %s %s", s.remote, p.Client.Name, p.Client.Version)
	}
	return s.srv.helloResult(), nil
}

// lookup resolves source and an optional type, returning the spec's errors.
func (s *session) lookup(source, typ string) (protocol.SensorInfo, *protocol.Error) {
	if source == "" {
		return protocol.SensorInfo{}, protocol.NewError(protocol.CodeInvalidParams, "params.source is required")
	}
	info, ok := s.srv.sensor(source)
	if !ok {
		return info, protocol.NewError(protocol.CodeUnknownSource, "unknown source %q", source)
	}
	if !info.Available {
		return info, protocol.NewError(protocol.CodeSourceUnavailable, "source %q is not available on this device", source)
	}
	if typ != "" && !info.HasType(typ) {
		return info, protocol.NewError(protocol.CodeInvalidParams, "source %q has no type %q (types: %s)", source, typ, strings.Join(info.Types, ", "))
	}
	return info, nil
}

func (s *session) subscribe(params json.RawMessage) (any, func(), *protocol.Error) {
	var p protocol.SubscribeParams
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	info, rpcErr := s.lookup(p.Source, p.Type)
	if rpcErr != nil {
		return nil, nil, rpcErr
	}
	// rate_hz is validated even for on_change sources, where it is then
	// ignored (spec §4.3: "Continuous sensors only").
	if p.RateHz != nil && !(*p.RateHz > 0) {
		return nil, nil, protocol.NewError(protocol.CodeInvalidParams, "params.rate_hz must be > 0")
	}
	if p.MinDelta != nil && !(*p.MinDelta >= 0) {
		return nil, nil, protocol.NewError(protocol.CodeInvalidParams, "params.min_delta must be >= 0")
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
	if err := decodeParams(params, &p); err != nil {
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
	if err := decodeParams(params, &p); err != nil {
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

// subscription is one active subscription of a session.
type subscription struct {
	sess     *session
	id       string
	info     protocol.SensorInfo
	types    []string
	rate     float64 // effective rate; 0 for on_change
	minDelta float64

	mu     sync.Mutex
	closed bool
	seq    uint64
	last   json.RawMessage // value of the last event actually queued
	stopFn func()
}

// start begins producing events.
func (sub *subscription) start() {
	if sub.info.Mode == protocol.ModeContinuous {
		stop := make(chan struct{})
		sub.setStop(func() { close(stop) })
		go sub.generate(stop)
		return
	}
	sub.setStop(sub.sess.srv.world.listen(sub.info.Source, sub.emit))
}

func (sub *subscription) setStop(f func()) {
	sub.mu.Lock()
	if sub.closed { // closed before it started (session ended)
		sub.mu.Unlock()
		f()
		return
	}
	sub.stopFn = f
	sub.mu.Unlock()
}

func (sub *subscription) generate(stop <-chan struct{}) {
	period := time.Duration(float64(time.Second) / sub.rate)
	rng := sub.sess.srv.newRand()
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-sub.sess.ctx.Done():
			return
		case now := <-t.C:
			r, err := sub.sess.srv.sampleContinuous(sub.info.Source, now, rng)
			if err != nil {
				sub.sess.srv.logf("mock: %v", err)
				return
			}
			sub.emit(r)
		}
	}
}

// emit filters, numbers and queues one reading. It never blocks.
func (sub *subscription) emit(r protocol.Reading) {
	if !slices.Contains(sub.types, r.Type) {
		return
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return
	}
	if sub.minDelta > 0 && sub.last != nil {
		d, err := protocol.ValueDistance(sub.info.Shape, sub.last, r.Value)
		if err == nil && d < sub.minDelta {
			return // filtered: not sent, so no seq consumed
		}
	}
	ev := &protocol.Event{Subscription: sub.id, Seq: sub.seq, Reading: r}
	b, err := json.Marshal(protocol.NewEventNotification(ev))
	if err != nil {
		sub.sess.srv.logf("mock: marshal event: %v", err)
		return
	}
	if sub.sess.trySend(b) {
		sub.seq++
		sub.last = r.Value
		return
	}
	// Queue full: the client isn't keeping up (spec §5, backpressure).
	if sub.info.Mode == protocol.ModeContinuous {
		sub.seq++ // dropped; the client sees a seq gap
		return
	}
	sub.sess.srv.logf("mock: %s: on_change queue overflow, closing", sub.sess.remote)
	sub.sess.closeWith(websocket.StatusPolicyViolation, fmt.Sprintf("event queue overflow (%s)", sub.info.Source))
}

// close stops the subscription. After it returns, emit queues nothing more.
func (sub *subscription) close() {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return
	}
	sub.closed = true
	stop := sub.stopFn
	sub.mu.Unlock()
	if stop != nil {
		stop()
	}
}
