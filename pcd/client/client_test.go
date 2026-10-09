package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// fakeServer runs handle for each WebSocket connection. It is a scripted
// stand-in for a phone, used to test client behaviour that a well-behaved
// server can't easily provoke (reordered responses, early events, ...).
func fakeServer(t *testing.T, handle func(ctx context.Context, fc *fakeConn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handle(r.Context(), &fakeConn{t: t, conn: conn})
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):] + protocol.WSPath
}

type fakeConn struct {
	t    *testing.T
	conn *websocket.Conn
	mu   sync.Mutex
}

func (f *fakeConn) read(ctx context.Context) (*protocol.Message, error) {
	_, data, err := f.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m protocol.Message
	if err := json.Unmarshal(data, &m); err != nil {
		f.t.Errorf("client sent invalid JSON: %s", data)
		return nil, err
	}
	return &m, nil
}

// drain reads (and discards) until the connection closes, which also
// completes the client's close handshake promptly.
func (f *fakeConn) drain(ctx context.Context) {
	for {
		if _, _, err := f.conn.Read(ctx); err != nil {
			return
		}
	}
}

func (f *fakeConn) send(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Error(err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.conn.Write(ctx, websocket.MessageText, b)
}

func helloResult() protocol.HelloResult {
	return protocol.HelloResult{
		Protocol:     protocol.Version,
		Server:       protocol.ServerInfo{Name: "fake", Version: "0"},
		Device:       protocol.DeviceInfo{ID: "dev1", Name: "Fake", Platform: "test"},
		Capabilities: []string{protocol.CapabilityEvents, protocol.CapabilityGet},
	}
}

// answerHello reads the hello request and answers it.
func (f *fakeConn) answerHello(ctx context.Context) bool {
	m, err := f.read(ctx)
	if err != nil {
		return false
	}
	if m.Method != protocol.MethodHello {
		f.t.Errorf("first request is %q, want hello", m.Method)
	}
	var p protocol.HelloParams
	if err := json.Unmarshal(m.Params, &p); err != nil || p.Protocol != protocol.Version {
		f.t.Errorf("bad hello params %s", m.Params)
	}
	f.send(ctx, protocol.NewResult(m.ID, helloResult()))
	return true
}

func event(sub string, seq uint64) *protocol.Event {
	ev := &protocol.Event{Subscription: sub, Seq: seq, Reading: protocol.Reading{
		Source: "gyro", Type: "rotation", Unit: "rad/s", TS: 1, TSMonoNS: 2, Device: "dev1",
	}}
	_ = ev.SetValue([]float64{float64(seq), 0, 0})
	return ev
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8765":  "ws://127.0.0.1:8765/v1/ws",
		"phone.lan:1":     "ws://phone.lan:1/v1/ws",
		"ws://x:1/custom": "ws://x:1/custom",
		"wss://x:1/v1/ws": "wss://x:1/v1/ws",
	} {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDialDoesHello(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		f.answerHello(ctx)
		f.drain(ctx)
	})
	c, err := Dial(ctxT(t), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Server(); got.Device.ID != "dev1" || got.Protocol != protocol.Version {
		t.Errorf("Server() = %+v", got)
	}
}

func TestDialHelloError(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		m, err := f.read(ctx)
		if err != nil {
			return
		}
		e := protocol.NewError(protocol.CodeUnsupportedProtocol, "nope")
		e.Data.Supported = []string{"phonetopc/1"}
		f.send(ctx, protocol.NewErrorResponse(m.ID, e))
		f.drain(ctx)
	})
	_, err := Dial(ctxT(t), url, WithProtocol("phonetopc/9"))
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeUnsupportedProtocol || pe.Data.Supported[0] != "phonetopc/1" {
		t.Fatalf("err = %v", err)
	}
}

// The server answers requests in reverse order, with events interleaved
// before, between and after the responses.
func TestCorrelationWithReorderingAndEvents(t *testing.T) {
	const n = 20
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		var reqs []*protocol.Message
		for len(reqs) < n {
			m, err := f.read(ctx)
			if err != nil {
				return
			}
			reqs = append(reqs, m)
		}
		for i := len(reqs) - 1; i >= 0; i-- {
			f.send(ctx, protocol.NewEventNotification(event("s-unknown", uint64(i))))
			var p protocol.GetParams
			_ = json.Unmarshal(reqs[i].Params, &p)
			r := protocol.Reading{Source: p.Source, Type: "x", Unit: "", Device: "dev1"}
			_ = r.SetValue(p.Source)
			f.send(ctx, protocol.NewResult(reqs[i].ID, r))
		}
		f.drain(ctx)
	})
	c, err := Dial(ctxT(t), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			src := fmt.Sprintf("src%d", i)
			r, err := c.Get(ctxT(t), src)
			if err != nil {
				t.Error(err)
				return
			}
			var v string
			if json.Unmarshal(r.Value, &v); v != src || r.Source != src {
				t.Errorf("call for %s got response %+v", src, r)
			}
		})
	}
	wg.Wait()
}

// An event sent immediately after the subscribe response must not be lost.
func TestSubscribeNoEarlyEventLoss(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		for {
			m, err := f.read(ctx)
			if err != nil {
				return
			}
			switch m.Method {
			case protocol.MethodSubscribe:
				f.send(ctx, protocol.NewResult(m.ID, protocol.SubscribeResult{
					Subscription: "s-1", Source: "gyro", Types: []string{"rotation"}, Mode: protocol.ModeContinuous, RateHz: 50}))
				for i := range 5 {
					f.send(ctx, protocol.NewEventNotification(event("s-1", uint64(i))))
				}
			case protocol.MethodUnsubscribe:
				f.send(ctx, protocol.NewEventNotification(event("s-1", 5)))
				f.send(ctx, protocol.NewResult(m.ID, protocol.UnsubscribeResult{}))
				f.send(ctx, protocol.NewEventNotification(event("s-1", 6))) // misbehaving server
			}
		}
	})
	for range 20 { // repeat to shake out scheduling-dependent loss
		c, err := Dial(ctxT(t), url)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := c.Subscribe(ctxT(t), "gyro", SubscribeOptions{RateHz: 50})
		if err != nil {
			t.Fatal(err)
		}
		if sub.ID != "s-1" || sub.RateHz != 50 || sub.Source != "gyro" || sub.Mode != ModeContinuous {
			t.Errorf("sub = %+v", sub)
		}
		for i := range 5 {
			select {
			case ev := <-sub.Events():
				if ev.Seq != uint64(i) {
					t.Fatalf("seq %d, want %d", ev.Seq, i)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for event")
			}
		}
		if err := sub.Unsubscribe(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		var rest []uint64
		for ev := range sub.Events() {
			rest = append(rest, ev.Seq)
		}
		// Event 5 arrived between the unsubscribe request and its response,
		// event 6 after the response: both are discarded.
		if len(rest) != 0 {
			t.Errorf("events after unsubscribe = %v, want none", rest)
		}
		if sub.Err() != nil {
			t.Errorf("Err() after Unsubscribe = %v", sub.Err())
		}
		if err := sub.Unsubscribe(ctxT(t)); err != nil {
			t.Errorf("second Unsubscribe: %v", err)
		}
		c.Close()
	}
}

func TestEventBufferOverflowDrops(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		m, err := f.read(ctx)
		if err != nil {
			return
		}
		f.send(ctx, protocol.NewResult(m.ID, protocol.SubscribeResult{Subscription: "s-1", Source: "gyro", Types: []string{"rotation"}}))
		for i := range 10 {
			f.send(ctx, protocol.NewEventNotification(event("s-1", uint64(i))))
		}
		// A get after the events proves the read loop isn't stalled.
		m, err = f.read(ctx)
		if err != nil {
			return
		}
		f.send(ctx, protocol.NewErrorResponse(m.ID, protocol.NewError(protocol.CodeNoValue, "none")))
		f.drain(ctx)
	})
	c, err := Dial(ctxT(t), url, WithEventBuffer(3))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub, err := c.Subscribe(ctxT(t), "gyro", SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(ctxT(t), "ui")
	if code, _ := protocol.ErrorCodeOf(err); code != protocol.CodeNoValue {
		t.Fatalf("Get err = %v", err)
	}
	if sub.Dropped() != 7 {
		t.Errorf("Dropped() = %d, want 7", sub.Dropped())
	}
	for i := range 3 {
		if ev := <-sub.Events(); ev.Seq != uint64(i) {
			t.Errorf("seq %d, want %d", ev.Seq, i)
		}
	}
}

func TestConnectionLossFailsCallsAndClosesSubscriptions(t *testing.T) {
	gotGet := make(chan struct{})
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		m, _ := f.read(ctx)
		f.send(ctx, protocol.NewResult(m.ID, protocol.SubscribeResult{Subscription: "s-1", Source: "gyro", Types: []string{"rotation"}}))
		f.read(ctx) // the get; never answered
		close(gotGet)
		f.conn.Close(websocket.StatusGoingAway, "bye")
	})
	c, err := Dial(ctxT(t), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub, err := c.Subscribe(ctxT(t), "gyro", SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(ctxT(t), "battery")
	if err == nil {
		t.Fatal("Get succeeded on a dropped connection")
	}
	<-gotGet
	<-c.Done()
	for range sub.Events() {
	}
	if sub.Err() == nil || c.Err() == nil {
		t.Errorf("sub.Err() = %v, c.Err() = %v; want errors", sub.Err(), c.Err())
	}
	if websocket.CloseStatus(errors.Unwrap(c.Err())) != websocket.StatusGoingAway {
		t.Errorf("close status of %v", c.Err())
	}
	if err := sub.Unsubscribe(ctxT(t)); err != nil {
		t.Errorf("Unsubscribe after loss = %v, want nil (already closed)", err)
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		for {
			if _, err := f.read(ctx); err != nil {
				return
			}
		}
	})
	c, err := Dial(ctxT(t), url)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := c.ListSensors(ctxT(t)); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Errorf("pending call err = %v, want ErrClosed", err)
	}
	if _, err := c.ListSensors(ctxT(t)); !errors.Is(err, ErrClosed) {
		t.Errorf("call after Close err = %v, want ErrClosed", err)
	}
	c.Close() // idempotent
}

// A subscribe whose caller gives up before the response arrives must be
// unsubscribed in the background so the server stops sending events.
func TestAbandonedSubscribeIsUnsubscribed(t *testing.T) {
	release := make(chan struct{})
	unsubscribed := make(chan string, 1)
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		if !f.answerHello(ctx) {
			return
		}
		m, err := f.read(ctx)
		if err != nil {
			return
		}
		<-release
		f.send(ctx, protocol.NewResult(m.ID, protocol.SubscribeResult{Subscription: "s-9", Source: "gyro", Types: []string{"rotation"}}))
		m, err = f.read(ctx)
		if err != nil {
			return
		}
		var p protocol.UnsubscribeParams
		_ = json.Unmarshal(m.Params, &p)
		f.send(ctx, protocol.NewResult(m.ID, protocol.UnsubscribeResult{}))
		unsubscribed <- m.Method + " " + p.Subscription
		f.drain(ctx)
	})
	c, err := Dial(ctxT(t), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Subscribe(ctx, "gyro", SubscribeOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(release)
	select {
	case got := <-unsubscribed:
		if got != "unsubscribe s-9" {
			t.Errorf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned subscription was not unsubscribed")
	}
}

func TestTapSeesBothDirections(t *testing.T) {
	url := fakeServer(t, func(ctx context.Context, f *fakeConn) {
		f.answerHello(ctx)
		f.drain(ctx)
	})
	var mu sync.Mutex
	var dirs []Direction
	c, err := Dial(ctxT(t), url, WithTap(func(d Direction, b []byte) {
		mu.Lock()
		dirs = append(dirs, d)
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(dirs) != 2 || dirs[0] != ClientToServer || dirs[1] != ServerToClient {
		t.Errorf("tap saw %v", dirs)
	}
}
