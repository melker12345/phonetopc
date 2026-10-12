package mockphone_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// quietOptions returns options with all background on_change activity
// disabled, so tests control every event.
func quietOptions() mockphone.Options {
	return mockphone.Options{
		TapEvery:       -1,
		LightEvery:     -1,
		ProximityEvery: -1,
		BatteryEvery:   -1,
		LocationEvery:  -1,
		NFCEvery:       -1,
		Seed:           1,
	}
}

// start runs a mock on a random port and returns it and its ws URL.
func start(t *testing.T, opts mockphone.Options) (*mockphone.Server, string) {
	t.Helper()
	srv := mockphone.New(opts)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.Close()
		hs.Close()
	})
	return srv, "ws" + hs.URL[len("http"):] + protocol.WSPath
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func dial(t *testing.T, url string, opts ...client.Option) *client.Client {
	t.Helper()
	c, err := client.Dial(testCtx(t), url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// rawConn speaks the protocol by hand, for cases the client can't produce.
type rawConn struct {
	t      *testing.T
	conn   *websocket.Conn
	nextID int
}

func dialRaw(t *testing.T, url string, opts *websocket.DialOptions) *rawConn {
	t.Helper()
	conn, _, err := websocket.Dial(testCtx(t), url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return &rawConn{t: t, conn: conn}
}

func (r *rawConn) sendRaw(frame string) {
	r.t.Helper()
	if err := r.conn.Write(testCtx(r.t), websocket.MessageText, []byte(frame)); err != nil {
		r.t.Fatal(err)
	}
}

// read returns the next message, or an error if the connection closed.
func (r *rawConn) read(ctx context.Context) (*protocol.Message, error) {
	_, data, err := r.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m protocol.Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("server sent invalid JSON %q: %w", data, err)
	}
	return &m, nil
}

func (r *rawConn) mustRead() *protocol.Message {
	r.t.Helper()
	m, err := r.read(testCtx(r.t))
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

// response reads until a response arrives, skipping events, and returns it.
func (r *rawConn) response() *protocol.Message {
	r.t.Helper()
	for {
		m, err := r.read(testCtx(r.t))
		if err != nil {
			r.t.Fatal(err)
		}
		if m.Kind() == protocol.KindResponse {
			return m
		}
	}
}

// call sends a request with params and returns the response.
func (r *rawConn) call(method string, params any) *protocol.Message {
	r.t.Helper()
	r.nextID++
	b, err := json.Marshal(protocol.NewRequest(protocol.IntID(int64(r.nextID)), method, params))
	if err != nil {
		r.t.Fatal(err)
	}
	r.sendRaw(string(b))
	m := r.response()
	if id, _ := protocol.ParseIntID(m.ID); id != int64(r.nextID) {
		r.t.Fatalf("response id %s, want %d", m.ID, r.nextID)
	}
	return m
}

func (r *rawConn) hello() {
	r.t.Helper()
	m := r.call("hello", protocol.HelloParams{Protocol: protocol.Version})
	if m.Error != nil {
		r.t.Fatal(m.Error)
	}
}

// wantError checks that m is an error response with code and matching
// data.name, and the given id (nil for null).
func wantError(t *testing.T, m *protocol.Message, code protocol.ErrorCode, id string) {
	t.Helper()
	if m.Kind() != protocol.KindResponse || m.Error == nil {
		t.Fatalf("want error %v, got %+v", code, m)
	}
	if m.Error.Code != code {
		t.Errorf("error code %d (%s), want %v", m.Error.Code, m.Error.Message, code)
	}
	if m.Error.Data == nil || m.Error.Data.Name != code.Name() {
		t.Errorf("error data %+v, want name %q", m.Error.Data, code.Name())
	}
	if m.Error.Message == "" {
		t.Error("empty error message")
	}
	if string(m.ID) != id {
		t.Errorf("error id %s, want %s", m.ID, id)
	}
}

func wantCode(t *testing.T, err error, code protocol.ErrorCode) {
	t.Helper()
	var e *protocol.Error
	if !errors.As(err, &e) {
		t.Fatalf("want error %v, got %v", code, err)
	}
	if e.Code != code || e.Data == nil || e.Data.Name != code.Name() {
		t.Errorf("got %v (data %+v), want %v", e, e.Data, code)
	}
}

func nextEvent(t *testing.T, sub *client.Subscription) client.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed: %v", sub.Err())
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	panic("unreachable")
}
