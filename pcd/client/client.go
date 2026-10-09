// Package client is a small Go client for the phonetopc/1 protocol.
//
// It connects to a phone (or the mock phone, or later the phonetopcd daemon),
// performs the hello handshake, and offers typed calls for list_sensors,
// subscribe, unsubscribe and get. Responses are correlated by id while events
// are delivered to per-subscription channels; all methods are safe for
// concurrent use.
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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Re-exported protocol types, so users of this package don't need to import
// an internal package.
type (
	HelloResult = protocol.HelloResult
	SensorInfo  = protocol.SensorInfo
	Reading     = protocol.Reading
	Event       = protocol.Event
	Battery     = protocol.Battery
	Error       = protocol.Error
	ErrorCode   = protocol.ErrorCode
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
	dialOptions *websocket.DialOptions
}

// Option configures Dial.
type Option func(*options)

// WithClientInfo sets the client name and version sent in hello.
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

// WithDialOptions sets the options passed to websocket.Dial.
func WithDialOptions(d *websocket.DialOptions) Option { return func(o *options) { o.dialOptions = d } }

// URL turns "host:port" into the phone's WebSocket URL
// ("ws://host:port/v1/ws"). Strings that already have a ws:// or wss://
// scheme are returned unchanged.
func URL(addr string) string {
	if strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://") {
		return addr
	}
	return "ws://" + addr + protocol.WSPath
}

// Client is a connection to a phonetopc server.
type Client struct {
	conn *websocket.Conn
	opts options

	nextID  atomic.Int64
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]*call
	subs    map[string]*Subscription
	err     error // set once the connection is gone
	done    chan struct{}
	hello   protocol.HelloResult
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
		clientInfo:  protocol.ClientInfo{Name: "phonetopc-go", Version: "0.1.0"},
		protocol:    protocol.Version,
		eventBuffer: DefaultEventBuffer,
	}
	for _, f := range opts {
		f(&o)
	}
	conn, _, err := websocket.Dial(ctx, url, o.dialOptions)
	if err != nil {
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

// Hello performs the hello handshake. Dial does this automatically.
func (c *Client) Hello(ctx context.Context) (HelloResult, error) {
	var res HelloResult
	ci := c.opts.clientInfo
	err := c.Call(ctx, protocol.MethodHello, protocol.HelloParams{Protocol: c.opts.protocol, Client: &ci}, &res)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.hello = res
	c.mu.Unlock()
	return res, nil
}

// Server returns the result of the last successful hello.
func (c *Client) Server() HelloResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// ListSensors returns the server's sensor list.
func (c *Client) ListSensors(ctx context.Context) ([]SensorInfo, error) {
	var res protocol.ListSensorsResult
	if err := c.Call(ctx, protocol.MethodListSensors, nil, &res); err != nil {
		return nil, err
	}
	return res.Sensors, nil
}

// Get returns the latest reading of source.
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
	if !ok {
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
