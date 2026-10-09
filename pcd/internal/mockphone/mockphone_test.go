package mockphone_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func TestHelloAndListSensors(t *testing.T) {
	opts := quietOptions()
	opts.Unavailable = []string{protocol.SourceProximity}
	_, url := start(t, opts)
	c := dial(t, url)

	h := c.Server()
	if h.Protocol != protocol.Version || h.Server.Name != mockphone.ServerName || h.Device.ID != "mock-0001" ||
		h.Device.Platform == "" || len(h.Capabilities) != 2 {
		t.Errorf("hello result %+v", h)
	}

	sensors, err := c.ListSensors(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	cat := protocol.Catalog()
	if len(sensors) != len(cat) {
		t.Fatalf("%d sensors, want %d", len(sensors), len(cat))
	}
	for i, s := range sensors {
		want := cat[i]
		want.Available = s.Source != protocol.SourceProximity
		b1, _ := json.Marshal(s)
		b2, _ := json.Marshal(want)
		if string(b1) != string(b2) {
			t.Errorf("sensor %d:\n got %s\nwant %s", i, b1, b2)
		}
	}
}

func TestRepeatedHelloAndFailedHelloKeepsSession(t *testing.T) {
	_, url := start(t, quietOptions())
	r := dialRaw(t, url, nil)
	r.hello()
	r.hello()
	wantError(t, r.call("hello", protocol.HelloParams{Protocol: "phonetopc/2"}), protocol.CodeUnsupportedProtocol, "3")
	if m := r.call("list_sensors", nil); m.Error != nil {
		t.Errorf("session lost hello state: %v", m.Error)
	}
}

func TestHelloRequired(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url, client.WithoutHello())
	_, err := c.ListSensors(testCtx(t))
	wantCode(t, err, protocol.CodeHelloRequired)
	_, err = c.Get(testCtx(t), "battery")
	wantCode(t, err, protocol.CodeHelloRequired)
	_, err = c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{})
	wantCode(t, err, protocol.CodeHelloRequired)
	err = c.Call(testCtx(t), "no_such_method", nil, nil)
	wantCode(t, err, protocol.CodeHelloRequired)
	if _, err := c.Hello(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListSensors(testCtx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedProtocol(t *testing.T) {
	_, url := start(t, quietOptions())
	_, err := client.Dial(testCtx(t), url, client.WithProtocol("phonetopc/2"))
	wantCode(t, err, protocol.CodeUnsupportedProtocol)
	var e *protocol.Error
	errors.As(err, &e)
	if len(e.Data.Supported) != 1 || e.Data.Supported[0] != protocol.Version {
		t.Errorf("data.supported = %v", e.Data.Supported)
	}

	r := dialRaw(t, url, nil)
	wantError(t, r.call("hello", map[string]any{}), protocol.CodeInvalidParams, "1")
	wantError(t, r.call("hello", nil), protocol.CodeInvalidParams, "2")
	wantError(t, r.call("hello", map[string]any{"protocol": 1}), protocol.CodeInvalidParams, "3")
}

func TestFramingErrors(t *testing.T) {
	_, url := start(t, quietOptions())
	r := dialRaw(t, url, nil)
	cases := []struct {
		frame string
		code  protocol.ErrorCode
		id    string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"hello"`, protocol.CodeParseError, "null"},
		{`not json`, protocol.CodeParseError, "null"},
		{`[{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1"}}]`, protocol.CodeInvalidRequest, "null"},
		{`[]`, protocol.CodeInvalidRequest, "null"},
		{`42`, protocol.CodeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","method":"hello","params":{"protocol":"phonetopc/1"}}`, protocol.CodeInvalidRequest, "null"},
		{`{"jsonrpc":"1.0","id":7,"method":"hello","params":{"protocol":"phonetopc/1"}}`, protocol.CodeInvalidRequest, "7"},
		{`{"id":8,"method":"hello","params":{"protocol":"phonetopc/1"}}`, protocol.CodeInvalidRequest, "8"},
		{`{"jsonrpc":"2.0","id":{"a":1},"method":"hello"}`, protocol.CodeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":1.5,"method":"hello"}`, protocol.CodeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":null,"method":"hello"}`, protocol.CodeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":"x","method":5}`, protocol.CodeInvalidRequest, `"x"`},
		{`{"jsonrpc":"2.0","id":"y"}`, protocol.CodeInvalidRequest, `"y"`},
	}
	for _, c := range cases {
		r.sendRaw(c.frame)
		m := r.response()
		t.Run(c.frame, func(t *testing.T) { wantError(t, m, c.code, c.id) })
	}
	// None of that counted as a hello, and the session is still usable.
	wantError(t, r.call("list_sensors", nil), protocol.CodeHelloRequired, "1")
	r.hello()
	// String ids are echoed back verbatim.
	r.sendRaw(`{"jsonrpc":"2.0","id":"req-é","method":"list_sensors"}`)
	if m := r.response(); string(m.ID) != `"req-é"` || m.Error != nil {
		t.Errorf("string id response %+v", m)
	}
}

func TestMethodAndParamErrors(t *testing.T) {
	opts := quietOptions()
	opts.Unavailable = []string{protocol.SourceLight}
	_, url := start(t, opts)
	r := dialRaw(t, url, nil)
	r.hello()
	cases := []struct {
		method string
		params any
		code   protocol.ErrorCode
	}{
		{"no_such_method", nil, protocol.CodeMethodNotFound},
		{"event", nil, protocol.CodeMethodNotFound},
		{"subscribe", nil, protocol.CodeInvalidParams},
		{"subscribe", []any{"gyro"}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": 3}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "gyro", "type": "bogus"}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "gyro", "rate_hz": 0}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "gyro", "rate_hz": -5}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "gyro", "rate_hz": "fast"}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "gyro", "min_delta": -1}, protocol.CodeInvalidParams},
		{"subscribe", map[string]any{"source": "nope"}, protocol.CodeUnknownSource},
		{"subscribe", map[string]any{"source": "light"}, protocol.CodeSourceUnavailable},
		{"unsubscribe", nil, protocol.CodeInvalidParams},
		{"unsubscribe", map[string]any{"subscription": "s-99"}, protocol.CodeUnknownSubscription},
		{"get", nil, protocol.CodeInvalidParams},
		{"get", map[string]any{"source": "nope"}, protocol.CodeUnknownSource},
		{"get", map[string]any{"source": "light"}, protocol.CodeSourceUnavailable},
		{"get", map[string]any{"source": "battery", "type": "bogus"}, protocol.CodeInvalidParams},
		{"get", map[string]any{"source": "ui"}, protocol.CodeNoValue},
	}
	for _, c := range cases {
		m := r.call(c.method, c.params)
		b, _ := json.Marshal(c.params)
		t.Run(c.method+" "+string(b), func(t *testing.T) {
			if id, _ := protocol.ParseIntID(m.ID); id == 0 {
				t.Errorf("bad id %s", m.ID)
			}
			wantError(t, m, c.code, string(m.ID))
		})
	}
	// Unknown members are ignored.
	m := r.call("get", map[string]any{"source": "battery", "extra": true})
	if m.Error != nil {
		t.Errorf("unknown params member rejected: %v", m.Error)
	}
}

func TestSubscribeGyro(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url)
	sub, err := c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 100})
	if err != nil {
		t.Fatal(err)
	}
	if sub.RateHz != 100 || sub.Source != "gyro" || len(sub.Types) != 1 || sub.Types[0] != "rotation" {
		t.Errorf("subscription %+v", sub)
	}
	begin := time.Now()
	var prev client.Event
	for i := range 50 {
		ev := nextEvent(t, sub)
		if ev.Seq != uint64(i) || ev.Subscription != sub.ID || ev.Source != "gyro" || ev.Type != "rotation" ||
			ev.Unit != "rad/s" || ev.Device != "mock-0001" {
			t.Fatalf("event %d: %+v", i, ev)
		}
		v, err := ev.Vec3()
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range v {
			if math.Abs(x) > 2 {
				t.Errorf("implausible angular rate %v", v)
			}
		}
		if i > 0 && (ev.TSMonoNS <= prev.TSMonoNS || ev.TS < prev.TS) {
			t.Errorf("timestamps not increasing: %d/%d after %d/%d", ev.TS, ev.TSMonoNS, prev.TS, prev.TSMonoNS)
		}
		if d := time.Since(time.UnixMilli(ev.TS)); d < -time.Second || d > 5*time.Second {
			t.Errorf("ts %d is not wall-clock ms", ev.TS)
		}
		prev = ev
	}
	// 50 events at 100 Hz take ~0.5 s.
	if el := time.Since(begin); el < 300*time.Millisecond || el > 3*time.Second {
		t.Errorf("50 events at 100 Hz took %v", el)
	}
	if err := sub.Unsubscribe(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	// A second unsubscribe of the same id is unknown to the server.
	err = c.Call(testCtx(t), "unsubscribe", protocol.UnsubscribeParams{Subscription: sub.ID}, nil)
	wantCode(t, err, protocol.CodeUnknownSubscription)
}

func TestRateClamping(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url)
	for _, tc := range []struct{ req, want float64 }{{1000, 200}, {0.25, 1}, {0, 50}, {12.5, 12.5}} {
		sub, err := c.Subscribe(testCtx(t), "accelerometer", client.SubscribeOptions{RateHz: tc.req})
		if err != nil {
			t.Fatal(err)
		}
		if sub.RateHz != tc.want {
			t.Errorf("rate %v -> %v, want %v", tc.req, sub.RateHz, tc.want)
		}
		sub.Unsubscribe(testCtx(t))
	}
	// rate_hz is ignored (and not reported) for on_change sources.
	sub, err := c.Subscribe(testCtx(t), "battery", client.SubscribeOptions{RateHz: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sub.RateHz != 0 {
		t.Errorf("on_change rate = %v", sub.RateHz)
	}
}

func TestMotionDataIsPlausible(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url)
	acc, _ := c.Subscribe(testCtx(t), "accelerometer", client.SubscribeOptions{RateHz: 200})
	rot, _ := c.Subscribe(testCtx(t), "game_rotation_vector", client.SubscribeOptions{RateHz: 200})
	for range 20 {
		v, err := nextEvent(t, acc).Vec3()
		if err != nil {
			t.Fatal(err)
		}
		g := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
		if g < 9.0 || g > 10.6 || v[2] < 7 {
			t.Errorf("accelerometer %v (|a| = %.2f) doesn't look like a phone held face up", v, g)
		}
		ev := nextEvent(t, rot)
		q, err := ev.Quat()
		if err != nil {
			t.Fatal(err)
		}
		n := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
		if math.Abs(n-1) > 1e-5 || ev.Unit != "" {
			t.Errorf("quaternion %v has norm %v (unit %q)", q, n, ev.Unit)
		}
	}
	if r, err := c.Get(testCtx(t), "gyro"); err != nil || r.Source != "gyro" || r.Unit != "rad/s" {
		t.Errorf("get gyro: %+v, %v", r, err)
	}
}

func TestMinDelta(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url)
	// The accelerometer never moves 100 m/s^2, so only the first event passes.
	sub, err := c.Subscribe(testCtx(t), "accelerometer", client.SubscribeOptions{RateHz: 200, MinDelta: 100})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, sub); ev.Seq != 0 {
		t.Errorf("first seq %d", ev.Seq)
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("event passed min_delta=100: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// With a moderate delta, consecutive sent values differ by at least it,
	// and seq has no gaps (filtered events don't consume seq numbers).
	const delta = 0.05
	sub2, err := c.Subscribe(testCtx(t), "accelerometer", client.SubscribeOptions{RateHz: 200, MinDelta: delta})
	if err != nil {
		t.Fatal(err)
	}
	prev := nextEvent(t, sub2)
	for i := 1; i < 20; i++ {
		ev := nextEvent(t, sub2)
		if ev.Seq != uint64(i) {
			t.Fatalf("seq %d, want %d", ev.Seq, i)
		}
		d, _ := protocol.ValueDistance(protocol.ShapeVec3, prev.Value, ev.Value)
		if d < delta {
			t.Errorf("distance %v < min_delta", d)
		}
		prev = ev
	}
}

func TestMultipleSubscriptionsHaveIndependentSeq(t *testing.T) {
	_, url := start(t, quietOptions())
	c := dial(t, url)
	a, _ := c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 50})
	b, _ := c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 20})
	if a.ID == b.ID {
		t.Fatalf("same subscription id %q", a.ID)
	}
	for i := range 5 {
		if ea, eb := nextEvent(t, a), nextEvent(t, b); ea.Seq != uint64(i) || eb.Seq != uint64(i) {
			t.Errorf("seqs %d/%d, want %d", ea.Seq, eb.Seq, i)
		}
	}
	a.Unsubscribe(testCtx(t))
	if ev := nextEvent(t, b); ev.Subscription != b.ID {
		t.Errorf("event for %s", ev.Subscription)
	}
}

func TestOnChangeSources(t *testing.T) {
	srv, url := start(t, quietOptions())
	c := dial(t, url)

	// State-like on_change sources deliver their current value on subscribe.
	for _, src := range []string{"light", "proximity", "battery"} {
		sub, err := c.Subscribe(testCtx(t), src, client.SubscribeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ev := nextEvent(t, sub)
		if ev.Seq != 0 || ev.Source != src {
			t.Errorf("initial %s event %+v", src, ev)
		}
		got, err := c.Get(testCtx(t), src)
		if err != nil || string(got.Value) != string(ev.Value) {
			t.Errorf("get %s = %+v, %v; want value %s", src, got, err, ev.Value)
		}
	}
	b, _ := c.Get(testCtx(t), "battery")
	if bat, err := b.Battery(); err != nil || bat.Level < 0 || bat.Level > 100 || b.Unit != "%" {
		t.Errorf("battery %+v %v", bat, err)
	}

	// ui has no value until a tap.
	ui, err := c.Subscribe(testCtx(t), "ui", client.SubscribeOptions{Type: "tile_tap"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ui.Events():
		t.Fatalf("ui event before any tap: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	_, err = c.Get(testCtx(t), "ui")
	wantCode(t, err, protocol.CodeNoValue)
	for i := range 3 {
		srv.Tap()
		ev := nextEvent(t, ui)
		if v, err := ev.Bool(); err != nil || !v || ev.Seq != uint64(i) || ev.Type != "tile_tap" {
			t.Errorf("tap event %+v", ev)
		}
	}
	if r, err := c.Get(testCtx(t), "ui"); err != nil || r.Type != "tile_tap" {
		t.Errorf("get ui after tap: %+v %v", r, err)
	}
}

func TestBackgroundOnChangeActivity(t *testing.T) {
	opts := quietOptions()
	opts.TapEvery = 30 * time.Millisecond
	opts.LightEvery = 20 * time.Millisecond
	opts.ProximityEvery = 20 * time.Millisecond
	opts.BatteryEvery = 20 * time.Millisecond
	_, url := start(t, opts)
	c := dial(t, url)
	for _, src := range []string{"ui", "light", "proximity", "battery"} {
		sub, err := c.Subscribe(testCtx(t), src, client.SubscribeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			if ev := nextEvent(t, sub); ev.Seq != uint64(i) {
				t.Errorf("%s seq %d, want %d", src, ev.Seq, i)
			}
		}
	}
}

// After the unsubscribe response, no events of that subscription arrive.
func TestNoEventsAfterUnsubscribeResponse(t *testing.T) {
	_, url := start(t, quietOptions())
	r := dialRaw(t, url, nil)
	r.hello()
	for range 10 {
		m := r.call("subscribe", map[string]any{"source": "gyro", "rate_hz": 200})
		var res protocol.SubscribeResult
		json.Unmarshal(m.Result, &res)
		// Let a few events queue up, then unsubscribe without reading them.
		time.Sleep(20 * time.Millisecond)
		r.nextID++
		b, _ := json.Marshal(protocol.NewRequest(protocol.IntID(int64(r.nextID)), "unsubscribe", map[string]any{"subscription": res.Subscription}))
		r.sendRaw(string(b))
		// Read through the unsubscribe response, give a stray event time to
		// show up, then use a get round-trip as a barrier. (Read deadlines
		// can't be used: they close the connection.)
		sawResponse := false
		for !sawResponse {
			m := r.mustRead()
			sawResponse = m.Kind() == protocol.KindResponse
		}
		time.Sleep(30 * time.Millisecond)
		r.nextID++
		r.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"get","params":{"source":"battery"}}`, r.nextID))
		for {
			m := r.mustRead()
			if m.Kind() == protocol.KindResponse {
				break
			}
			var ev protocol.Event
			json.Unmarshal(m.Params, &ev)
			if ev.Subscription == res.Subscription {
				t.Fatalf("event seq %d after unsubscribe response", ev.Seq)
			}
		}
	}
}

func TestBinaryFrameCloses1003(t *testing.T) {
	_, url := start(t, quietOptions())
	r := dialRaw(t, url, nil)
	r.hello()
	if err := r.conn.Write(testCtx(t), websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	_, err := r.read(testCtx(t))
	if s := websocket.CloseStatus(err); s != websocket.StatusUnsupportedData {
		t.Errorf("close status %v (err %v), want 1003", s, err)
	}
}

func TestPings(t *testing.T) {
	opts := quietOptions()
	opts.PingInterval = 20 * time.Millisecond
	opts.PingTimeout = 100 * time.Millisecond
	_, url := start(t, opts)

	// A client that answers pings (by reading) stays connected and sees them.
	var pings atomic.Int32
	c := dial(t, url, client.WithDialOptions(&websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool { pings.Add(1); return true },
	}))
	time.Sleep(200 * time.Millisecond)
	if n := pings.Load(); n < 3 {
		t.Errorf("got %d pings in 200ms at 20ms interval", n)
	}
	if _, err := c.ListSensors(testCtx(t)); err != nil {
		t.Errorf("responsive client was disconnected: %v", err)
	}

	// A client that doesn't answer is dropped after the timeout.
	r := dialRaw(t, url, &websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool { return false }, // never pong
	})
	r.hello()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if _, err := r.read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("unresponsive client was not disconnected")
			}
			break
		}
	}
}

func TestOnChangeOverflowCloses1008(t *testing.T) {
	opts := quietOptions()
	opts.QueueSize = 1
	srv, url := start(t, opts)
	r := dialRaw(t, url, nil)
	r.hello()
	if m := r.call("subscribe", map[string]any{"source": "ui"}); m.Error != nil {
		t.Fatal(m.Error)
	}
	// Flood taps faster than the writer can send while not reading.
	for range 20000 {
		srv.Tap()
	}
	var err error
	for err == nil {
		_, err = r.read(testCtx(t))
	}
	if s := websocket.CloseStatus(err); s != websocket.StatusPolicyViolation {
		t.Errorf("close status %v (err %v), want 1008", s, err)
	}
}

func TestContinuousOverflowDropsWithSeqGaps(t *testing.T) {
	opts := quietOptions()
	opts.QueueSize = 1
	_, url := start(t, opts)
	c := dial(t, url, client.WithEventBuffer(1<<16))
	subs := make([]*client.Subscription, 8)
	for i := range subs {
		var err error
		subs[i], err = c.Subscribe(testCtx(t), "gyro", client.SubscribeOptions{RateHz: 200})
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	// With a one-slot queue and 8 x 200 Hz, some events are dropped; the
	// connection stays open, and seq stays strictly increasing per sub.
	if _, err := c.ListSensors(testCtx(t)); err != nil {
		t.Fatalf("connection died: %v", err)
	}
	var got, maxSeq uint64
	for _, s := range subs {
		var last int64 = -1
	drain:
		for {
			select {
			case ev := <-s.Events():
				if int64(ev.Seq) <= last {
					t.Fatalf("seq went from %d to %d", last, ev.Seq)
				}
				last = int64(ev.Seq)
				got++
				maxSeq = max(maxSeq, ev.Seq)
			default:
				break drain
			}
		}
	}
	t.Logf("received %d events, highest seq %d", got, maxSeq)
}

func TestServerCloseSendsGoingAway(t *testing.T) {
	srv, url := start(t, quietOptions())
	c := dial(t, url)
	srv.Close()
	<-c.Done()
	if s := websocket.CloseStatus(errors.Unwrap(c.Err())); s != websocket.StatusGoingAway {
		t.Errorf("close status %v (%v), want 1001", s, c.Err())
	}
}

// min_delta has no defined distance for bool values; repeated taps (always
// true) must not be suppressed.
func TestMinDeltaDoesNotSuppressTaps(t *testing.T) {
	srv, url := start(t, quietOptions())
	c := dial(t, url)
	sub, err := c.Subscribe(testCtx(t), "ui", client.SubscribeOptions{MinDelta: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		srv.Tap()
		if ev := nextEvent(t, sub); ev.Seq != uint64(i) {
			t.Errorf("seq %d, want %d", ev.Seq, i)
		}
	}
}
