package pairing

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Prompter asks the PC's user to confirm a pairing. Both methods must
// return promptly with ctx.Err() when ctx is done (the phone went away or
// the listener expired).
type Prompter interface {
	// ConfirmQR asks `Pair with "<name>"? [Y/n]` (§4.2).
	ConfirmQR(ctx context.Context, phone PhoneInfo) (bool, error)
	// ConfirmSAS asks `Does the phone show 123 456? [y/N]` (§4.3).
	ConfirmSAS(ctx context.Context, phone PhoneInfo, code string) (bool, error)
}

// Way is how a pairing was made.
type Way string

const (
	WayQR  Way = "qr"
	WaySAS Way = "sas"
)

// Paired is the outcome of a successful pairing, as the PC stores it.
type Paired struct {
	Phone PhoneInfo
	Token string
	Way   Way
	// RemoteHost is the address the phone connected from, often also where
	// it serves.
	RemoteHost string
}

// Options configures a Listener.
type Options struct {
	PC     protocol.PCInfo
	Cert   tls.Certificate // the PC's long-term certificate
	Secret string          // QR secret; default: a fresh random one

	// Prompt asks the user. Required.
	Prompt Prompter
	// Save stores a pairing (§6). It runs before pair_finish is answered;
	// if it fails, the phone gets internal_error and may retry. Required.
	Save func(Paired) error

	// Expiry closes the listener (default 5 minutes, §4.1).
	Expiry time.Duration
	// MaxFailures failed attempts (bad secret, rejection, SAS mismatch) per
	// FailureWindow make the listener refuse further attempts until the
	// window has passed. Defaults 5 and 1 minute (§4.1).
	MaxFailures   int
	FailureWindow time.Duration
	// IdleTimeout closes a pairing connection that sends nothing for this
	// long (default 3 minutes; users may take a while to compare codes).
	IdleTimeout time.Duration

	// Logf, if set, receives progress messages for the user.
	Logf func(format string, args ...any)
}

// Errors returned by Serve.
var (
	ErrExpired = errors.New("pairing expired: no phone paired within the time limit")
)

// Listener is a PC pairing listener. It serves one pairing at a time and
// stops after the first successful one.
type Listener struct {
	opts Options
	pin  string

	ctx     context.Context // done when Serve stops
	expired chan struct{}   // closed on expiry, before ctx is cancelled

	mu       sync.Mutex
	busy     bool
	failures []time.Time
	result   chan Paired // buffered(1)
	wg       sync.WaitGroup
}

// New creates a listener. Call Serve to run it.
func New(opts Options) (*Listener, error) {
	if opts.Prompt == nil || opts.Save == nil {
		return nil, errors.New("pairing: Prompt and Save are required")
	}
	pin, err := security.CertPin(opts.Cert)
	if err != nil {
		return nil, err
	}
	if opts.Secret == "" {
		opts.Secret = security.NewSecret()
	}
	if opts.Expiry <= 0 {
		opts.Expiry = 5 * time.Minute
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = 5
	}
	if opts.FailureWindow <= 0 {
		opts.FailureWindow = time.Minute
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 3 * time.Minute
	}
	return &Listener{opts: opts, pin: pin, result: make(chan Paired, 1), expired: make(chan struct{})}, nil
}

// Secret returns the QR secret.
func (l *Listener) Secret() string { return l.opts.Secret }

// Pin returns the PC's pin.
func (l *Listener) Pin() string { return l.pin }

func (l *Listener) logf(format string, args ...any) {
	if l.opts.Logf != nil {
		l.opts.Logf(format, args...)
	}
}

// Serve runs the pairing listener on ln (wrapping it in TLS) until a phone
// has paired, the listener expires (ErrExpired), or ctx is done (ctx.Err()).
// It closes ln.
func (l *Listener) Serve(ctx context.Context, ln net.Listener) (Paired, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.ctx = ctx

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, l.serveWS)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Phones that abort the handshake after a pin check, port scanners
		// and the like are not worth a log line each.
		ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	tlsLn := tls.NewListener(ln, security.ServerTLSConfig(l.opts.Cert))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(tlsLn) }()

	expiry := time.NewTimer(l.opts.Expiry)
	defer expiry.Stop()
	var (
		res Paired
		err error
	)
	select {
	case res = <-l.result:
	case <-expiry.C:
		err = ErrExpired
		close(l.expired) // sessions answer pending requests with -32024
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	cancel()
	// Give sessions a moment to send their last answer and close frame.
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	srv.Close()
	return res, err
}

// blocked reports whether too many attempts failed recently.
func (l *Listener) blocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.opts.FailureWindow)
	l.failures = slices.DeleteFunc(l.failures, func(t time.Time) bool { return t.Before(cutoff) })
	return len(l.failures) >= l.opts.MaxFailures
}

func (l *Listener) fail() {
	l.mu.Lock()
	l.failures = append(l.failures, time.Now())
	l.mu.Unlock()
}

func (l *Listener) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Phones are not browsers and send no cookies; the Origin check
		// protects nothing here.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	l.wg.Add(1)
	defer l.wg.Done()
	defer conn.CloseNow()

	if l.blocked() {
		conn.Close(websocket.StatusTryAgainLater, "too many failed pairing attempts, try again in a minute")
		return
	}
	l.mu.Lock()
	if l.busy {
		l.mu.Unlock()
		conn.Close(websocket.StatusTryAgainLater, "another pairing is in progress")
		return
	}
	l.busy = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.busy = false
		l.mu.Unlock()
	}()

	l.logf("phone connected from %s", r.RemoteAddr)
	s := &session{l: l, conn: conn, remote: r.RemoteAddr}
	s.run()
}

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

	idle := time.NewTimer(s.l.opts.IdleTimeout)
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
			s.conn.Close(websocket.StatusPolicyViolation, "idle for too long")
			return
		case data := <-frames:
			idle.Reset(s.l.opts.IdleTimeout)
			resp, after, closeCode := s.handle(data)
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
// discard everything").
func (s *session) reset() {
	if s.stopPrompt != nil {
		s.stopPrompt()
		s.stopPrompt = nil
	}
	s.state, s.way, s.phone, s.npc, s.answer = stStart, "", PhoneInfo{}, nil, nil
}

// handle processes one frame. It returns the response (nil for none), a
// function to run once it is sent, and a close code (0 to stay open).
func (s *session) handle(data []byte) (*protocol.Response, func(), websocket.StatusCode) {
	id, method, params, errResp := parseRequest(data)
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
			s.l.fail()
			s.reset()
			if s.l.blocked() {
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
	switch method {
	case MethodQR:
		return s.pairQR(params)
	case MethodSASBegin:
		return s.sasBegin(params)
	case MethodSASNonce:
		return s.sasNonce(params)
	case MethodSASConfirm:
		return s.sasConfirm(params)
	default:
		return s.finish(params)
	}
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
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if p.Secret == "" {
		return nil, nil, invalid(errors.New("params.secret is required"))
	}
	if err := p.Phone.validate(); err != nil {
		return nil, nil, invalid(err)
	}
	if err := checkProtocol(p.Protocol); err != nil {
		return nil, nil, err
	}
	if !security.Equal(p.Secret, s.l.opts.Secret) {
		s.l.logf("wrong pairing secret from %s", s.remote)
		return nil, nil, protocol.NewError(protocol.CodeBadSecret, "wrong pairing secret")
	}
	ok, err := s.l.opts.Prompt.ConfirmQR(s.ctx, p.Phone)
	if err != nil {
		if s.ctx.Err() != nil {
			return nil, nil, s.waitErr()
		}
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
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if err := p.Phone.validate(); err != nil {
		return nil, nil, invalid(err)
	}
	if err := checkProtocol(p.Protocol); err != nil {
		return nil, nil, err
	}
	s.npc = security.RandomBytes(security.NonceSize)
	s.state, s.way, s.phone = stCommitted, WaySAS, p.Phone
	return SASBeginResult{PC: s.l.opts.PC, Commitment: security.Commitment(s.npc)}, nil, nil
}

func (s *session) sasNonce(params json.RawMessage) (any, func(), error) {
	var p NonceMessage
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	nphone, err := security.DecodeB64(p.Nonce)
	if err != nil || len(nphone) != security.NonceSize {
		return nil, nil, invalid(fmt.Errorf("params.nonce must be base64url of %d bytes", security.NonceSize))
	}
	code := security.SASCode(s.l.pin, s.phone.Pin, s.npc, nphone)
	s.state = stCoded
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
	if err := decodeParams(params, &p); err != nil {
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
	if err := decodeParams(params, &p); err != nil {
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

// parseRequest applies the framing checks of PROTOCOL.md §2 steps 1-4. It
// returns an error response, or the request's parts; id is nil for a
// notification, which gets no reply.
func parseRequest(data []byte) (id json.RawMessage, method string, params json.RawMessage, errResp *protocol.Response) {
	fail := func(id json.RawMessage, code protocol.ErrorCode, msg string) (json.RawMessage, string, json.RawMessage, *protocol.Response) {
		return nil, "", nil, protocol.NewErrorResponse(id, protocol.NewError(code, "%s", msg))
	}
	if !json.Valid(data) {
		return fail(nil, protocol.CodeParseError, "parse error: frame is not valid JSON")
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if trimmed[0] != '{' {
		return fail(nil, protocol.CodeInvalidRequest, "request must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fail(nil, protocol.CodeInvalidRequest, "invalid request")
	}
	id, hasID := fields["id"]
	if !hasID {
		return nil, "", nil, nil
	}
	if !protocol.ValidRequestID(id) {
		return fail(nil, protocol.CodeInvalidRequest, "id must be a string or an integer within ±(2^53-1)")
	}
	var version string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != protocol.JSONRPCVersion {
		return fail(id, protocol.CodeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if m := bytes.TrimSpace(fields["method"]); len(m) == 0 || m[0] != '"' || json.Unmarshal(m, &method) != nil {
		return fail(id, protocol.CodeInvalidRequest, "method must be a string")
	}
	return id, method, fields["params"], nil
}

// decodeParams decodes params into v. Absent or null params decode as {}.
func decodeParams(params json.RawMessage, v any) error {
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
			return protocol.NewError(protocol.CodeInvalidParams, "params.%s has the wrong type", te.Field)
		}
		return protocol.NewError(protocol.CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}
