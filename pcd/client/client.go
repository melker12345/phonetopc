// Package client is a small Go client for the phonetopc/1 protocol.
//
// It connects to a phone (or the mock phone, or later the phonetopcd daemon),
// performs the hello handshake, and offers typed calls for the methods of
// draft 4:
//
//   - sensors (spec/PROTOCOL.md §4): ListSensors, Subscribe (and
//     Subscription.Unsubscribe) and Get;
//   - fingerprint approval (spec/APPROVAL.md): RequestApproval, which
//     verifies the phone's signature itself;
//   - notifications and panels (spec/PANELS.md): Notify,
//     CancelNotification, ShowPanel, AttachPanel, UpdatePanel, ClosePanel,
//     ListPanels and ClearCanvas, whose input arrives as events of the
//     "notification" and "panel" sources;
//   - anything else through Call.
//
// Responses are correlated by id while events are delivered to
// per-subscription channels; all methods are safe for concurrent use.
//
// Secure mode (SECURITY.md) needs the phone's pin and the token from
// pairing (`phonetopc pair`). DialPaired takes both from the PC's
// credential store and finds the phone (mDNS, then its stored addresses);
// PairedDevices lists the paired phones. Dial with WithPin and WithToken
// connects to a known address:
//
//	c, err := client.DialPaired(ctx, "")    // the only paired phone
//	c, err := client.Dial(ctx, client.SecureURL("192.168.1.153:8765"),
//		client.WithPin(pin), client.WithToken(token))
//
// Dev mode is plain ws:// without either:
//
//	c, err := client.Dial(ctx, client.URL("127.0.0.1:8765"))
//	if err != nil { ... }
//	defer c.Close()
//	sub, err := c.Subscribe(ctx, "gyro", client.SubscribeOptions{RateHz: 100})
//	for ev := range sub.Events() {
//		v, _ := ev.Vec3()
//		fmt.Println(ev.Seq, v)
//	}
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

// Re-exported protocol types, so users of this package don't need to import
// an internal package.
type (
	HelloResult = protocol.HelloResult
	SensorInfo  = protocol.SensorInfo
	Reading     = protocol.Reading
	Event       = protocol.Event
	Battery     = protocol.Battery
	Position    = protocol.Position
	Tag         = protocol.Tag
	NDEFRecord  = protocol.NDEFRecord
	Error       = protocol.Error
	ErrorCode   = protocol.ErrorCode
	Mode        = protocol.Mode
	PCInfo      = protocol.PCInfo
	// ApprovalInfo is hello's approval member (APPROVAL.md §1).
	ApprovalInfo = protocol.ApprovalInfo
	// PinMismatchError is returned (wrapped) by Dial when the server's
	// certificate doesn't match the pin.
	PinMismatchError = security.PinMismatchError
)

// ErrPinMismatch matches (with errors.Is) every pin mismatch: the phone
// presented a different key than at pairing. The only fix is to re-pair.
var ErrPinMismatch = security.ErrPinMismatch

// Error codes a caller may want to tell apart.
const (
	CodeUnauthorized = protocol.CodeUnauthorized
)

// IsUnauthorized reports whether err is the server's -32010: the token is
// missing, unknown or revoked.
func IsUnauthorized(err error) bool {
	code, ok := protocol.ErrorCodeOf(err)
	return ok && code == protocol.CodeUnauthorized
}

// Sensor modes, as in SensorInfo.Mode and Subscription.Mode.
const (
	ModeContinuous = protocol.ModeContinuous
	ModeOnChange   = protocol.ModeOnChange
)

// ErrClosed is returned by calls on a client that has been closed with Close.
var ErrClosed = errors.New("phonetopc: client closed")

// DefaultEventBuffer is the default per-subscription event channel capacity.
const DefaultEventBuffer = 1024

// Direction says which way a frame went, for WithTap.
type Direction string

const (
	ClientToServer Direction = "c2s"
	ServerToClient Direction = "s2c"
)

type options struct {
	clientInfo  protocol.ClientInfo
	protocol    string
	skipHello   bool
	eventBuffer int
	tap         func(Direction, []byte)
	// approvalTrust decides whether an approval key pin is trusted
	// (APPROVAL.md §3); see WithApprovalTrust.
	approvalTrust func(pin string) error
	// testNonce replaces the approval nonce; only set by this package's
	// tests, to replay the signed test vectors.
	testNonce   string
	dialOptions *websocket.DialOptions
	pin         string
	token       string

	// DialPaired only.
	configDir        string
	discovery        bool
	discoveryTimeout time.Duration
	addrOverride     string
}

// Option configures Dial, DialPaired and PairedDevices; each ignores the
// options that don't concern it.
type Option func(*options)

// WithClientInfo names the program in hello, where the phone may show it.
// The default is "phonetopc-go" with this module's version.
func WithClientInfo(name, version string) Option {
	return func(o *options) { o.clientInfo = protocol.ClientInfo{Name: name, Version: version} }
}

// WithProtocol overrides the protocol version requested in hello. Only useful
// for testing servers.
func WithProtocol(p string) Option { return func(o *options) { o.protocol = p } }

// WithoutHello makes Dial skip the hello handshake. The caller must then call
// Hello itself before anything else. Only useful for testing servers.
func WithoutHello() Option { return func(o *options) { o.skipHello = true } }

// WithEventBuffer sets the per-subscription event channel capacity. When a
// subscription's channel is full, further events for it are dropped (counted
// by Subscription.Dropped, and visible as a seq gap) rather than stalling
// responses to other calls.
func WithEventBuffer(n int) Option { return func(o *options) { o.eventBuffer = max(n, 1) } }

// WithTap registers a function called with every text frame sent or
// received, in wire order. It is called synchronously and must not block or
// retain the slice.
func WithTap(f func(dir Direction, frame []byte)) Option { return func(o *options) { o.tap = f } }

// WithDialOptions passes extra options to websocket.Dial, e.g. headers.
// For wss:// URLs their HTTPClient is replaced by one that verifies the pin.
func WithDialOptions(d *websocket.DialOptions) Option { return func(o *options) { o.dialOptions = d } }

// WithPin makes Dial verify that the server's certificate has this pin
// (SECURITY.md §2). It is required for wss:// URLs: the connection fails
// with ErrPinMismatch before anything is sent if the pin differs.
func WithPin(pin string) Option { return func(o *options) { o.pin = pin } }

// WithToken sends the token from pairing in hello's auth member (secure
// mode). Without a valid one a secure phone refuses hello with an error
// that IsUnauthorized recognizes.
func WithToken(token string) Option { return func(o *options) { o.token = token } }

// SecureURL turns "host:port" into the phone's secure WebSocket URL
// ("wss://host:port/v1/ws"). Strings with a scheme are returned unchanged.
func SecureURL(addr string) string {
	if strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://") {
		return addr
	}
	return "wss://" + addr + protocol.WSPath
}

// URL turns "host:port" into the phone's WebSocket URL
// ("ws://host:port/v1/ws"). Strings that already have a ws:// or wss://
// scheme are returned unchanged.
func URL(addr string) string {
	if strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://") {
		return addr
	}
	return "ws://" + addr + protocol.WSPath
}

// Client is one connection to a phone (or the mock phone). Its methods are
// safe for concurrent use. Once the connection is gone, Done is closed and
// every call fails with Err; dial again to reconnect.
type Client struct {
	conn *websocket.Conn
	opts options

	nextID  atomic.Int64
	writeMu sync.Mutex

	mu       sync.Mutex
	pending  map[int64]*call
	subs     map[string]*Subscription
	err      error // set once the connection is gone
	done     chan struct{}
	hello    protocol.HelloResult
	warnings []string
}

func (c *Client) warn(msg string) {
	c.mu.Lock()
	c.warnings = append(c.warnings, msg)
	c.mu.Unlock()
}

// Warnings returns things the caller may want to tell the user about the
// connection, e.g. that a paired phone reports a different device id.
func (c *Client) Warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.warnings...)
}

type call struct {
	ch chan callResult // buffered(1)
	// onResult, if set, runs in the read loop before the result is delivered,
	// so that state it sets up (e.g. a subscription) is in place before any
	// later frame is processed.
	onResult func(json.RawMessage) error
	// abandoned is set (under Client.mu) when the caller gave up waiting.
	abandoned bool
	// onAbandoned, if set, runs in its own goroutine when a successful result
	// arrives for an abandoned call.
	onAbandoned func(json.RawMessage)
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Dial connects to url (see URL) and, unless WithoutHello is given, performs
// the hello handshake.
func Dial(ctx context.Context, url string, opts ...Option) (*Client, error) {
	o := options{
		clientInfo:  protocol.ClientInfo{Name: "phonetopc-go", Version: version.Version},
		protocol:    protocol.Version,
		eventBuffer: DefaultEventBuffer,
	}
	for _, f := range opts {
		f(&o)
	}
	dopts, err := o.secureDialOptions(url)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, url, dopts)
	if err != nil {
		// Report a pin mismatch itself, not the WebSocket and TLS layers
		// wrapped around it; errors.Is and errors.As match either way.
		var pm *PinMismatchError
		if errors.As(err, &pm) {
			err = pm
		}
		return nil, fmt.Errorf("phonetopc: dial %s: %w", url, err)
	}
	conn.SetReadLimit(1 << 20)
	c := &Client{
		conn:    conn,
		opts:    o,
		pending: make(map[int64]*call),
		subs:    make(map[string]*Subscription),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	if !o.skipHello {
		if _, err := c.Hello(ctx); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// secureDialOptions checks the pin and token against the URL scheme and, for wss://,
// installs the pinned TLS configuration.
func (o *options) secureDialOptions(url string) (*websocket.DialOptions, error) {
	secure := strings.HasPrefix(url, "wss://")
	switch {
	case secure && o.pin == "":
		return nil, errors.New("phonetopc: a wss:// URL needs the phone's pin (WithPin); certificates are never trusted otherwise")
	case !secure && o.pin != "":
		return nil, fmt.Errorf("phonetopc: a pin was given but %s is not a wss:// URL; refusing to connect in plaintext", url)
	case !secure && o.token != "":
		return nil, fmt.Errorf("phonetopc: a token was given but %s is not a wss:// URL; refusing to send it in plaintext", url)
	case !secure:
		return o.dialOptions, nil
	}
	if !security.ValidPin(o.pin) {
		return nil, fmt.Errorf("phonetopc: %q is not a valid pin", o.pin)
	}
	var d websocket.DialOptions
	if o.dialOptions != nil {
		d = *o.dialOptions
	}
	tr := &http.Transport{
		TLSClientConfig: security.PinnedClientTLSConfig(o.pin),
		Proxy:           nil, // never route a phone on the LAN through a proxy
	}
	d.HTTPClient = &http.Client{Transport: tr}
	return &d, nil
}

// Hello performs the hello handshake. Dial does this automatically.
func (c *Client) Hello(ctx context.Context) (HelloResult, error) {
	var res HelloResult
	ci := c.opts.clientInfo
	params := protocol.HelloParams{Protocol: c.opts.protocol, Client: &ci}
	if c.opts.token != "" {
		params.Auth = &protocol.Auth{Token: c.opts.token}
	}
	err := c.Call(ctx, protocol.MethodHello, params, &res)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.hello = res
	c.mu.Unlock()
	return res, nil
}

// Server returns the result of the last successful hello: the phone's
// identity, capabilities and mode. It is the zero value before hello.
func (c *Client) Server() HelloResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// ListSensors returns the sources this phone offers, with their modes and
// rates; the names are what Subscribe and Get take.
func (c *Client) ListSensors(ctx context.Context) ([]SensorInfo, error) {
	var res protocol.ListSensorsResult
	if err := c.Call(ctx, protocol.MethodListSensors, nil, &res); err != nil {
		return nil, err
	}
	return res.Sensors, nil
}

// Get returns the latest reading of source without subscribing. A sensor
// with no value yet may be started briefly, or fail with -32006 (no_value).
func (c *Client) Get(ctx context.Context, source string) (Reading, error) {
	var res Reading
	err := c.Call(ctx, protocol.MethodGet, protocol.GetParams{Source: source}, &res)
	return res, err
}

// Call sends a request and waits for its response. params may be nil; result
// may be nil to discard the result. If the server returns an error, Call
// returns it as a *protocol.Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	raw, err := c.roundTrip(ctx, method, params, &call{})
	if err != nil || result == nil {
		return err
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("phonetopc: decoding %s result: %w", method, err)
	}
	return nil
}

func (c *Client) roundTrip(ctx context.Context, method string, params any, cl *call) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	frame, err := json.Marshal(protocol.NewRequest(protocol.IntID(id), method, params))
	if err != nil {
		return nil, fmt.Errorf("phonetopc: encoding %s request: %w", method, err)
	}
	cl.ch = make(chan callResult, 1)

	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = cl
	c.mu.Unlock()

	if err := c.write(ctx, frame); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case r := <-cl.ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		defer c.mu.Unlock()
		select {
		case r := <-cl.ch: // raced with the response
			return r.result, r.err
		default:
		}
		if cl.onAbandoned != nil {
			cl.abandoned = true // keep it pending so the read loop can clean up
		} else {
			delete(c.pending, id)
		}
		return nil, ctx.Err()
	}
}

func (c *Client) write(ctx context.Context, frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.opts.tap != nil {
		c.opts.tap(ClientToServer, frame)
	}
	if err := c.conn.Write(ctx, websocket.MessageText, frame); err != nil {
		if e := c.Err(); e != nil {
			return e
		}
		return fmt.Errorf("phonetopc: write: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		typ, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.shutdown(fmt.Errorf("phonetopc: connection lost: %w", err))
			return
		}
		if typ != websocket.MessageText {
			continue // binary frames are reserved; ignore
		}
		if c.opts.tap != nil {
			c.opts.tap(ServerToClient, data)
		}
		var m protocol.Message
		if json.Unmarshal(data, &m) != nil {
			continue // not ours to fix; clients ignore garbage
		}
		switch m.Kind() {
		case protocol.KindResponse:
			c.handleResponse(&m)
		case protocol.KindNotification:
			if m.Method == protocol.MethodEvent {
				c.handleEvent(m.Params)
			}
		}
	}
}

func (c *Client) handleResponse(m *protocol.Message) {
	id, ok := protocol.ParseIntID(m.ID)
	if !ok {
		return // id null (server could not parse a request) or not one of ours
	}
	c.mu.Lock()
	cl, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	var r callResult
	if m.Error != nil {
		r.err = m.Error
	} else {
		r.result = m.Result
		if cl.onResult != nil {
			r.err = cl.onResult(m.Result)
		}
	}
	c.mu.Lock()
	abandoned := cl.abandoned
	if !abandoned {
		cl.ch <- r
	}
	c.mu.Unlock()
	if abandoned && r.err == nil && cl.onAbandoned != nil {
		go cl.onAbandoned(m.Result)
	}
}

func (c *Client) handleEvent(params json.RawMessage) {
	var ev Event
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.subs[ev.Subscription]
	if !ok || s.unsubscribing {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
	}
}

// shutdown marks the client dead, fails pending calls and closes all
// subscription channels. Only the first call has an effect.
func (c *Client) shutdown(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	for id, cl := range c.pending {
		if !cl.abandoned {
			cl.ch <- callResult{err: err}
		}
		delete(c.pending, id)
	}
	for id, s := range c.subs {
		s.closed = true
		s.err = err
		close(s.ch)
		delete(c.subs, id)
	}
}

// Close closes the connection. Pending calls fail with ErrClosed and all
// subscription channels are closed.
func (c *Client) Close() error {
	c.mu.Lock()
	alreadyDead := c.err != nil
	c.mu.Unlock()
	c.shutdown(ErrClosed)
	if alreadyDead {
		c.conn.CloseNow()
		return nil
	}
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	if err != nil {
		c.conn.CloseNow()
	}
	return nil
}

// Done is closed when the connection is gone (closed by either side).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection is gone, or nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
