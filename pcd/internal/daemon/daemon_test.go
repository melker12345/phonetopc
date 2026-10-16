package daemon_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	crand "crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

func quietOptions() mockphone.Options {
	return mockphone.Options{
		TapEvery: -1, LightEvery: -1, ProximityEvery: -1, BatteryEvery: -1,
		LocationEvery: -1, NFCEvery: -1, UserEvery: -1, Seed: 1,
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// phone is a dev-mode mock that can be stopped and started again on the
// same address.
type phone struct {
	t    *testing.T
	addr string
	opts mockphone.Options

	mu  sync.Mutex
	srv *mockphone.Server
	hs  *httptest.Server
}

// freeAddr returns a free loopback address below the ephemeral port range,
// so that no outgoing connection or ":0" listener takes it before the
// phone listens on it (again).
func freeAddr(t *testing.T) string {
	t.Helper()
	for range 100 {
		addr := "127.0.0.1:" + strconv.Itoa(20000+rand.IntN(12000))
		if ln, err := net.Listen("tcp", addr); err == nil {
			ln.Close()
			return addr
		}
	}
	t.Fatal("no free port")
	return ""
}

func newPhone(t *testing.T, opts mockphone.Options) *phone {
	t.Helper()
	p := &phone{t: t, addr: freeAddr(t), opts: opts}
	t.Cleanup(p.stop)
	return p
}

func (p *phone) start() *mockphone.Server {
	p.t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	srv := mockphone.New(p.opts)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener.Close()
	hs.Listener = ln
	hs.Start()
	p.mu.Lock()
	p.srv, p.hs = srv, hs
	p.mu.Unlock()
	return srv
}

func (p *phone) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srv != nil {
		p.srv.Close()
		p.hs.CloseClientConnections()
		p.hs.Close()
		p.srv, p.hs = nil, nil
	}
}

func (p *phone) dial(ctx context.Context) (*client.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.Dial(dctx, client.URL(p.addr))
}

// droppable wraps a dialer so a test can drop the current connection
// (httptest's CloseClientConnections doesn't reach WebSockets).
type droppable struct {
	dial daemon.Dialer
	mu   sync.Mutex
	cl   *client.Client
}

func (dr *droppable) Dial(ctx context.Context) (*client.Client, error) {
	cl, err := dr.dial(ctx)
	dr.mu.Lock()
	dr.cl = cl
	dr.mu.Unlock()
	return cl, err
}

// drop closes the connection and waits for the daemon's next one.
func (dr *droppable) drop(t *testing.T, d *daemon.Daemon) {
	t.Helper()
	before := d.Status().Since
	dr.mu.Lock()
	dr.cl.Close()
	dr.mu.Unlock()
	waitFor(t, func() bool { s := d.Status(); return s.Connected && s.Since != before })
}

// startDaemon runs a daemon on dial and serves its API without a token.
func startDaemon(t *testing.T, dial daemon.Dialer) (*daemon.Daemon, string) {
	t.Helper()
	d := daemon.New(daemon.Options{Dial: dial, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, Logf: t.Logf})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	hs := httptest.NewServer(d.Handler())
	t.Cleanup(func() { cancel(); <-done; hs.Close() })
	return d, hs.URL
}

// connectedDaemon runs a daemon on a fresh dev-mode mock.
func connectedDaemon(t *testing.T, opts mockphone.Options) (*mockphone.Server, *daemon.Daemon, string) {
	t.Helper()
	p := newPhone(t, opts)
	srv := p.start()
	d, url := startDaemon(t, p.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	return srv, d, url
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func do(t *testing.T, method, url, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(testCtx(t), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := response{status: res.StatusCode, raw: string(b)}
	_ = json.Unmarshal(b, &r.body)
	return r
}

func (r response) errorCode() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func wantStatus(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.status != status || r.errorCode() != code {
		t.Fatalf("got %d %q, want %d %q: %s", r.status, r.errorCode(), status, code, r.raw)
	}
}

// lines opens an NDJSON event stream and returns its lines.
type lineStream struct {
	t    *testing.T
	body io.ReadCloser
	ch   chan map[string]any
}

func openLines(t *testing.T, url string, header http.Header) *lineStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		cancel()
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, b)
	}
	ls := &lineStream{t: t, body: res.Body, ch: make(chan map[string]any, 1000)}
	go func() {
		defer close(ls.ch)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := strings.TrimPrefix(sc.Text(), "data: ")
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Errorf("bad line %q: %v", line, err)
				return
			}
			ls.ch <- m
		}
	}()
	t.Cleanup(func() { cancel(); res.Body.Close() })
	return ls
}

func (ls *lineStream) next() map[string]any {
	ls.t.Helper()
	select {
	case m, ok := <-ls.ch:
		if !ok {
			ls.t.Fatal("stream ended")
		}
		return m
	case <-time.After(10 * time.Second):
		ls.t.Fatal("no event within 10s")
	}
	return nil
}

// nextFrom skips events of other sources.
func (ls *lineStream) nextFrom(source string) map[string]any {
	ls.t.Helper()
	for {
		if m := ls.next(); m["source"] == source {
			return m
		}
	}
}

func (ls *lineStream) close() { ls.body.Close() }

func TestStatusSensorsAndGet(t *testing.T) {
	_, _, url := connectedDaemon(t, quietOptions())

	r := do(t, "GET", url+"/v1/status", "")
	if r.status != 200 || r.body["connected"] != true {
		t.Fatalf("status: %s", r.raw)
	}
	if dev, _ := r.body["device"].(map[string]any); dev["id"] != "6d6f636b00000001" {
		t.Fatalf("device: %s", r.raw)
	}

	r = do(t, "GET", url+"/v1/sensors", "")
	if s, _ := r.body["sensors"].([]any); r.status != 200 || len(s) == 0 {
		t.Fatalf("sensors: %s", r.raw)
	}

	r = do(t, "GET", url+"/v1/sensors/battery", "")
	if r.status != 200 || r.body["source"] != "battery" || r.body["value"] == nil {
		t.Fatalf("get: %s", r.raw)
	}

	wantStatus(t, do(t, "GET", url+"/v1/sensors/nope", ""), 404, "unknown_source")
	wantStatus(t, do(t, "GET", url+"/v1/nope", ""), 404, "not_found")
	wantStatus(t, do(t, "POST", url+"/v1/status", ""), 405, "method_not_allowed")
}

func TestStatusWhileDisconnected(t *testing.T) {
	p := newPhone(t, quietOptions()) // never started
	_, url := startDaemon(t, p.dial)
	r := do(t, "GET", url+"/v1/status", "")
	if r.status != 200 || r.body["connected"] != false || r.body["error"] == "" {
		t.Fatalf("status: %s", r.raw)
	}
	wantStatus(t, do(t, "GET", url+"/v1/sensors/battery", ""), 503, "phone_unavailable")
	wantStatus(t, do(t, "POST", url+"/v1/notifications", `{"title":"hi"}`), 503, "phone_unavailable")
}

func TestEventsNDJSON(t *testing.T) {
	_, _, url := connectedDaemon(t, quietOptions())
	ls := openLines(t, url+"/v1/events?source=gyro&rate=100", nil)
	var last float64 = -1
	for range 3 {
		ev := ls.next()
		if ev["source"] != "gyro" || ev["type"] != "rotation" || ev["device"] != "6d6f636b00000001" {
			t.Fatalf("event: %v", ev)
		}
		if _, ok := ev["subscription"]; ok {
			t.Fatalf("subscription id leaked: %v", ev)
		}
		seq := ev["seq"].(float64)
		if seq <= last {
			t.Fatalf("seq %v after %v", seq, last)
		}
		last = seq
	}
}

func TestEventsSSEAndSeveralSources(t *testing.T) {
	srv, _, url := connectedDaemon(t, quietOptions())
	ls := openLines(t, url+"/v1/events?source=ui,nfc", http.Header{"Accept": {"text/event-stream"}})
	srv.Tap()
	if ev := ls.next(); ev["source"] != "ui" {
		t.Fatalf("event: %v", ev)
	}
	srv.ScanTag()
	if ev := ls.next(); ev["source"] != "nfc" {
		t.Fatalf("event: %v", ev)
	}
}

func TestEventsWebSocket(t *testing.T) {
	srv, _, url := connectedDaemon(t, quietOptions())
	ctx := testCtx(t)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/v1/events?source=ui", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// The stream is subscribed before the upgrade is answered.
	srv.Tap()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if json.Unmarshal(b, &ev); ev["source"] != "ui" || ev["type"] != "tile_tap" {
		t.Fatalf("event: %s", b)
	}
}

func TestEventsBadRequests(t *testing.T) {
	_, _, url := connectedDaemon(t, quietOptions())
	wantStatus(t, do(t, "GET", url+"/v1/events", ""), 400, "invalid_request")
	wantStatus(t, do(t, "GET", url+"/v1/events?source=gyro&rate=0", ""), 400, "invalid_request")
	wantStatus(t, do(t, "GET", url+"/v1/events?source=gyro&min_delta=x", ""), 400, "invalid_request")
	wantStatus(t, do(t, "GET", url+"/v1/events?source=nope", ""), 404, "unknown_source")
	wantStatus(t, do(t, "GET", url+"/v1/events?source=gyro,nope", ""), 404, "unknown_source")
}

// countingOptions counts subscribe and unsubscribe requests the phone gets.
func countingOptions(subs, unsubs *int, mu *sync.Mutex) mockphone.Options {
	o := quietOptions()
	o.Tap = func(dir mockphone.Direction, frame []byte) {
		var m struct{ Method string }
		json.Unmarshal(frame, &m)
		mu.Lock()
		defer mu.Unlock()
		switch m.Method {
		case protocol.MethodSubscribe:
			*subs++
		case protocol.MethodUnsubscribe:
			*unsubs++
		}
	}
	return o
}

func TestStreamsShareOneSubscription(t *testing.T) {
	var (
		mu           sync.Mutex
		subs, unsubs int
	)
	srv, d, url := connectedDaemon(t, countingOptions(&subs, &unsubs, &mu))
	a := openLines(t, url+"/v1/events?source=ui", nil)
	b := openLines(t, url+"/v1/events?source=ui", nil)
	c := openLines(t, url+"/v1/events?source=ui&type=tile_tap", nil) // different options: its own
	srv.Tap()
	for _, ls := range []*lineStream{a, b, c} {
		if ev := ls.next(); ev["source"] != "ui" {
			t.Fatalf("event: %v", ev)
		}
	}
	mu.Lock()
	if subs != 2 {
		t.Errorf("%d subscribes, want 2", subs)
	}
	mu.Unlock()
	if n := d.Status().Streams; n != 3 {
		t.Errorf("status streams %d", n)
	}

	a.close()
	b.close()
	c.close()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return unsubs == 2 && d.Status().Streams == 0
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamSurvivesReconnect(t *testing.T) {
	p := newPhone(t, quietOptions())
	p.start()
	d, url := startDaemon(t, p.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	ls := openLines(t, url+"/v1/events?source=ui", nil)
	p.mu.Lock()
	srv := p.srv
	p.mu.Unlock()
	srv.Tap()
	ls.nextFrom("ui")

	p.stop()
	if ev := ls.next(); ev["source"] != daemon.SourceDaemon || ev["type"] != daemon.TypeDisconnected {
		t.Fatalf("want disconnected, got %v", ev)
	}
	srv = p.start()
	if ev := ls.nextFrom(daemon.SourceDaemon); ev["type"] != daemon.TypeConnected {
		t.Fatalf("want connected, got %v", ev)
	}
	// Resubscribed on the new connection.
	waitFor(t, func() bool { srv.Tap(); return len(ls.ch) > 0 })
	if ev := ls.nextFrom("ui"); ev["type"] != "tile_tap" {
		t.Fatalf("event: %v", ev)
	}
}

func TestStreamOpenedWhileDisconnected(t *testing.T) {
	p := newPhone(t, quietOptions())
	d, url := startDaemon(t, p.dial)
	ls := openLines(t, url+"/v1/events?source=ui", nil)
	if ev := ls.next(); ev["type"] != daemon.TypeDisconnected {
		t.Fatalf("want disconnected first, got %v", ev)
	}
	srv := p.start()
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { srv.Tap(); return len(ls.ch) > 0 })
	if ev := ls.nextFrom("ui"); ev["type"] != "tile_tap" {
		t.Fatalf("event: %v", ev)
	}
}

func TestStreamOpenedWhileDisconnectedUnknownSource(t *testing.T) {
	p := newPhone(t, quietOptions())
	_, url := startDaemon(t, p.dial)
	ls := openLines(t, url+"/v1/events?source=nope", nil)
	ls.next() // disconnected
	p.start()
	ev := ls.nextFrom(daemon.SourceDaemon)
	for ev["type"] == daemon.TypeConnected {
		ev = ls.nextFrom(daemon.SourceDaemon)
	}
	if ev["type"] != daemon.TypeError || !strings.Contains(ev["value"].(map[string]any)["error"].(string), "unknown source") {
		t.Fatalf("want an error event, got %v", ev)
	}
	if _, ok := <-ls.ch; ok {
		t.Fatal("stream didn't end")
	}
}

func TestNotifications(t *testing.T) {
	srv, _, url := connectedDaemon(t, quietOptions())
	ls := openLines(t, url+"/v1/events?source=notification", nil)

	r := do(t, "POST", url+"/v1/notifications", `{"title":"Build done","body":"3 files","actions":[{"id":"ok","label":"OK"}]}`)
	id, _ := r.body["id"].(string)
	if r.status != 200 || id == "" {
		t.Fatalf("notify: %s", r.raw)
	}
	if err := srv.User("").Act(id, "ok"); err != nil {
		t.Fatal(err)
	}
	ev := ls.next()
	if v := ev["value"].(map[string]any); ev["type"] != "action" || v["notification"] != id || v["action"] != "ok" {
		t.Fatalf("event: %v", ev)
	}

	if r := do(t, "DELETE", url+"/v1/notifications/"+id, ""); r.status != 200 {
		t.Fatalf("cancel: %s", r.raw)
	}
	wantStatus(t, do(t, "POST", url+"/v1/notifications", `{"title":""}`), 400, "invalid_request")
	wantStatus(t, do(t, "POST", url+"/v1/notifications", `{"titel":"typo"}`), 400, "invalid_request")
	wantStatus(t, do(t, "POST", url+"/v1/notifications", `not json`), 400, "invalid_request")

	srv.AllowNotify(false)
	r = do(t, "POST", url+"/v1/notifications", `{"title":"hi"}`)
	wantStatus(t, r, 403, "not_allowed")
	if r.body["error"].(map[string]any)["rpc_code"] != float64(protocol.CodeNotAllowed) {
		t.Fatalf("rpc_code: %s", r.raw)
	}
}

func TestPanels(t *testing.T) {
	srv, d, url := connectedDaemon(t, quietOptions())
	ls := openLines(t, url+"/v1/events?source=panel", nil)
	layout := `{"type":"column","children":[{"type":"text","id":"status","text":"idle"},{"type":"button","id":"go","label":"Go"}]}`

	r := do(t, "PUT", url+"/v1/panels/deck", `{"title":"Deck","layout":`+layout+`}`)
	if r.status != 200 || r.body["panel"] != "deck" {
		t.Fatalf("show: %s", r.raw)
	}
	if got := d.Status().Panels; len(got) != 1 || got[0] != "deck" {
		t.Fatalf("status panels %v", got)
	}
	u := srv.User("")
	if err := u.Look("deck"); err != nil {
		t.Fatal(err)
	}
	if err := u.Press("deck", "go"); err != nil {
		t.Fatal(err)
	}
	for {
		ev := ls.next()
		if ev["type"] == "press" {
			if v := ev["value"].(map[string]any); v["panel"] != "deck" || v["widget"] != "go" {
				t.Fatalf("press: %v", ev)
			}
			break
		}
	}

	if r := do(t, "PATCH", url+"/v1/panels/deck", `{"updates":[{"widget":"status","set":{"text":"going"}}]}`); r.status != 200 {
		t.Fatalf("update: %s", r.raw)
	}
	r = do(t, "GET", url+"/v1/panels", "")
	if p, _ := r.body["panels"].([]any); r.status != 200 || len(p) != 1 {
		t.Fatalf("list: %s", r.raw)
	}
	if r := do(t, "DELETE", url+"/v1/panels/deck", ""); r.status != 200 {
		t.Fatalf("close: %s", r.raw)
	}
	if got := d.Status().Panels; len(got) != 0 {
		t.Fatalf("status panels %v after close", got)
	}
	wantStatus(t, do(t, "PATCH", url+"/v1/panels/deck", `{"updates":[{"widget":"status","set":{"text":"x"}}]}`), 404, "unknown_panel")
	wantStatus(t, do(t, "PUT", url+"/v1/panels/x", `{"title":"X","layout":{"type":"nope"}}`), 400, "invalid_request")
	wantStatus(t, do(t, "PUT", url+"/v1/panels/x", `{"title":"X"}`), 400, "invalid_request")
}

func TestPanelReattachedAfterReconnect(t *testing.T) {
	p := newPhone(t, quietOptions())
	srv := p.start()
	dr := &droppable{dial: p.dial}
	d, url := startDaemon(t, dr.Dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	ls := openLines(t, url+"/v1/events?source=panel", nil)
	r := do(t, "PUT", url+"/v1/panels/deck", `{"title":"Deck","layout":{"type":"button","id":"go","label":"Go"}}`)
	if r.status != 200 {
		t.Fatalf("show: %s", r.raw)
	}
	// Drop only the connection: the phone keeps the panel, detached.
	dr.drop(t, d)
	if ev := ls.nextFrom(daemon.SourceDaemon); ev["type"] != daemon.TypeDisconnected {
		t.Fatalf("want disconnected, got %v", ev)
	}
	if ev := ls.nextFrom(daemon.SourceDaemon); ev["type"] != daemon.TypeConnected {
		t.Fatalf("want connected, got %v", ev)
	}
	waitFor(t, func() bool {
		ps := srv.User("").Panels()
		return len(ps) == 1 && ps[0].Attached
	})
	u := srv.User("")
	if err := u.Look("deck"); err != nil {
		t.Fatal(err)
	}
	if err := u.Press("deck", "go"); err != nil {
		t.Fatal(err)
	}
	for ev := ls.next(); ev["type"] != "press"; ev = ls.next() {
	}
}

func TestRequireToken(t *testing.T) {
	_, d, _ := connectedDaemon(t, quietOptions())
	hs := httptest.NewServer(daemon.RequireToken("s3cret-token", d.Handler()))
	defer hs.Close()

	get := func(path string, h http.Header, host string) *http.Response {
		req, _ := http.NewRequest("GET", hs.URL+path, nil)
		for k, v := range h {
			req.Header[k] = v
		}
		if host != "" {
			req.Host = host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get("/v1/status", nil, ""); res.StatusCode != 401 {
		t.Errorf("no token: %d", res.StatusCode)
	}
	if res := get("/v1/status", http.Header{"Authorization": {"Bearer wrong"}}, ""); res.StatusCode != 401 {
		t.Errorf("wrong token: %d", res.StatusCode)
	}
	if res := get("/v1/status", http.Header{"Authorization": {"Bearer s3cret-token"}}, ""); res.StatusCode != 200 {
		t.Errorf("bearer: %d", res.StatusCode)
	}
	if res := get("/v1/status?token=s3cret-token", nil, ""); res.StatusCode != 200 {
		t.Errorf("query token: %d", res.StatusCode)
	}
	if res := get("/v1/status", http.Header{"Authorization": {"Bearer s3cret-token"}}, "evil.example:8767"); res.StatusCode != 403 {
		t.Errorf("rebinding host: %d", res.StatusCode)
	}
	if res := get("/v1/status", http.Header{"Authorization": {"Bearer s3cret-token"}}, "localhost:8767"); res.StatusCode != 200 {
		t.Errorf("localhost: %d", res.StatusCode)
	}

	req, _ := http.NewRequest("OPTIONS", hs.URL+"/v1/notifications", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 || !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight: %d %v", res.StatusCode, res.Header)
	}
}

func TestSlowStreamOnChangeEnds(t *testing.T) {
	p := newPhone(t, quietOptions())
	srv := p.start()
	d := daemon.New(daemon.Options{Dial: p.dial, StreamBuffer: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(d.Handler())
	defer hs.Close()

	// A client that never reads: the server's writes back up, the stream's
	// queue fills, and the on_change source ends it rather than lose taps.
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/v1/events?source=ui", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitFor(t, func() bool {
		for range 2000 {
			srv.Tap()
		}
		return d.Status().Streams == 0
	})
}

// securePhone is a secure mock with this PC paired to it.
type securePhone struct {
	st   *mockphone.State
	srv  *mockphone.Server
	pcID string
	dial daemon.Dialer
}

func newSecurePhone(t *testing.T, tweak func(*mockphone.Options)) *securePhone {
	t.Helper()
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("", "Test Phone")
	if err != nil {
		t.Fatal(err)
	}
	opts := quietOptions()
	opts.Device = protocol.DeviceInfo{ID: id.ID, Name: id.Name}
	opts.Authenticate = st.Authenticate
	opts.ApprovalKey = func() *ecdsa.PrivateKey { k, _ := st.ApprovalKey(); return k }
	opts.DecryptKey = func() *rsa.PrivateKey { k, _ := st.DecryptKey(); return k }
	if tweak != nil {
		tweak(&opts)
	}
	srv := mockphone.New(opts)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = security.ServerTLSConfig(id.Cert)
	hs.Config.ErrorLog = log.New(io.Discard, "", 0)
	hs.StartTLS()
	t.Cleanup(func() { srv.Close(); hs.Close() })
	addr := strings.TrimPrefix(hs.URL, "https://")

	tok := security.NewToken()
	cfg := filepath.Join(t.TempDir(), "phonetopc")
	pc, err := store.Open(cfg).PC()
	if err != nil {
		t.Fatal(err)
	}
	st.PutPC(mockphone.PairedPC{ID: pc.ID, Name: "test PC", Token: tok})
	if err := store.Open(cfg).PutDevice(store.Device{ID: id.ID, Name: "phone", Pin: id.Pin, Token: tok, PairedAt: store.Now(time.Now())}); err != nil {
		t.Fatal(err)
	}
	return &securePhone{st: st, srv: srv, pcID: pc.ID, dial: func(ctx context.Context) (*client.Client, error) {
		return client.DialPaired(ctx, "", client.WithConfigDir(cfg), client.WithAddress(addr))
	}}
}

func TestApprovals(t *testing.T) {
	var (
		mu  sync.Mutex
		why protocol.Why // "": approve
	)
	sp := newSecurePhone(t, func(o *mockphone.Options) {
		o.Approve = func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
			mu.Lock()
			defer mu.Unlock()
			return why
		}
	})
	st := sp.st
	dr := &droppable{dial: sp.dial}
	d, url := startDaemon(t, dr.Dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}

	r := do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?"}`)
	if r.status != 200 || r.body["approved"] != true || r.body["why"] != "approved" {
		t.Fatalf("approved: %s", r.raw)
	}

	// The program's own nonce: it can check the proof without trusting the daemon.
	nonce := security.B64(security.RandomBytes(protocol.ApprovalNonceSize))
	r = do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?","nonce":"`+nonce+`"}`)
	var proof struct {
		Proof daemon.ApprovalProof `json:"proof"`
	}
	if err := json.Unmarshal([]byte(r.raw), &proof); err != nil || r.status != 200 {
		t.Fatalf("with a nonce: %s", r.raw)
	}
	p := proof.Proof
	key, err := security.ParseApprovalKey(p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if pin, _ := security.ApprovalKeyPin(p.Key); pin != p.KeyPin || p.Nonce != nonce || p.PCID != sp.pcID {
		t.Fatalf("proof %+v", p)
	}
	if err := security.VerifyApproval(key, p.Signature, security.ApprovalMessage(p.PhoneID, sp.pcID, nonce, "Deploy?")); err != nil {
		t.Fatalf("proof doesn't verify: %v", err)
	}
	if err := security.VerifyApproval(key, p.Signature, security.ApprovalMessage(p.PhoneID, sp.pcID, nonce, "Deploy!")); err == nil {
		t.Fatal("proof verifies for another reason")
	}
	wantStatus(t, do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?","nonce":"short"}`), 400, "invalid_request")

	mu.Lock()
	why = protocol.WhyDenied
	mu.Unlock()
	r = do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?","timeout_ms":5000}`)
	if r.status != 200 || r.body["approved"] != false || r.body["why"] != "denied" {
		t.Fatalf("denied: %s", r.raw)
	}
	wantStatus(t, do(t, "POST", url+"/v1/approvals", `{"reason":" padded "}`), 400, "invalid_request")
	wantStatus(t, do(t, "POST", url+"/v1/approvals", `{"reason":"x","timeout_ms":1}`), 400, "invalid_request")

	// A rotated key (fingerprints changed) is refused until accepted.
	if _, err := st.RotateApprovalKey(); err != nil {
		t.Fatal(err)
	}
	dr.drop(t, d)
	wantStatus(t, do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?"}`), 409, "approval_key_changed")
}

func TestDecryptions(t *testing.T) {
	var (
		mu  sync.Mutex
		why protocol.Why
		got []mockphone.ApprovalRequest
	)
	sp := newSecurePhone(t, func(o *mockphone.Options) {
		o.Approve = func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, req)
			return why
		}
	})
	dr := &droppable{dial: sp.dial}
	d, url := startDaemon(t, dr.Dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}

	r := do(t, "GET", url+"/v1/decryption-key", "")
	var key daemon.DecryptKeyResponse
	if err := json.Unmarshal([]byte(r.raw), &key); err != nil || r.status != 200 {
		t.Fatalf("key: %s", r.raw)
	}
	pub, _, err := security.ParseDecryptKey(key.Key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef")
	ct, _ := security.EncryptToPhone(pub, secret)
	body := func(pin string, ct []byte) string {
		return `{"reason":"Decrypt notes.age","key_pin":"` + pin + `","ciphertext":"` + security.B64(ct) + `"}`
	}

	r = do(t, "POST", url+"/v1/decryptions", body(key.Pin, ct))
	if r.status != 200 || r.body["decrypted"] != true || r.body["plaintext"] != security.B64(secret) {
		t.Fatalf("decrypt: %s", r.raw)
	}
	mu.Lock()
	if len(got) != 1 || !got[0].Decrypt || got[0].Reason != "Decrypt notes.age" {
		t.Errorf("the user saw %+v", got)
	}
	why = protocol.WhyDenied
	mu.Unlock()
	r = do(t, "POST", url+"/v1/decryptions", body(key.Pin, ct))
	if r.status != 200 || r.body["decrypted"] != false || r.body["why"] != "denied" || r.body["plaintext"] != nil {
		t.Fatalf("denied: %s", r.raw)
	}
	mu.Lock()
	why = ""
	mu.Unlock()

	// Encrypted to another key: the user confirms, the phone can't decrypt.
	other, _ := rsa.GenerateKey(crand.Reader, 2048)
	wrong, _ := security.EncryptToPhone(&other.PublicKey, secret)
	r = do(t, "POST", url+"/v1/decryptions", body(key.Pin, wrong))
	if r.status != 200 || r.body["why"] != "failed" {
		t.Fatalf("other key: %s", r.raw)
	}
	wantStatus(t, do(t, "POST", url+"/v1/decryptions", body("sha256/AAAA", ct)), 409, "decrypt_key_changed")
	wantStatus(t, do(t, "POST", url+"/v1/decryptions", body(key.Pin, ct[:10])), 400, "invalid_request")
	wantStatus(t, do(t, "POST", url+"/v1/decryptions", `{"reason":" x","key_pin":"p","ciphertext":"AAAA"}`), 400, "invalid_request")

	// Fingerprints changed: the key is new, and the old ciphertext is refused before anything is sent.
	if _, err := sp.st.RotateDecryptKey(); err != nil {
		t.Fatal(err)
	}
	dr.drop(t, d)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	wantStatus(t, do(t, "POST", url+"/v1/decryptions", body(key.Pin, ct)), 409, "decrypt_key_changed")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Error("the user was asked about a ciphertext for the old key")
	}
}
