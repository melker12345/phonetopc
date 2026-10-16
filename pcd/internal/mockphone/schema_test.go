package mockphone_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

var specDir = filepath.Join("..", "..", "..", "spec")

// schemas holds the compiled message schema and the per-method result
// schemas. The top-level schema accepts any "result" in a response, so
// results are additionally checked against
// $defs/resultByMethod/properties/<method>, found by correlating the
// response id with the request that used it.
type schemas struct {
	message *jsonschema.Schema
	results map[string]*jsonschema.Schema // by method
}

var loadSchemasOnce = sync.OnceValues(func() (*schemas, error) {
	f, err := os.Open(filepath.Join(specDir, "schemas", "phonetopc-1.schema.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		return nil, err
	}
	const id = "https://github.com/melker12345/phonetopc/spec/schemas/phonetopc-1.schema.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(id, doc); err != nil {
		return nil, err
	}
	s := &schemas{results: map[string]*jsonschema.Schema{}}
	if s.message, err = c.Compile(id); err != nil {
		return nil, err
	}
	for _, method := range []string{
		protocol.MethodHello, protocol.MethodListSensors, protocol.MethodSubscribe,
		protocol.MethodUnsubscribe, protocol.MethodGet, protocol.MethodRequestApproval,
		protocol.MethodDecrypt,
		protocol.MethodNotify, protocol.MethodNotifyCancel, protocol.MethodPanelShow,
		protocol.MethodPanelAttach, protocol.MethodPanelUpdate, protocol.MethodPanelClose,
		protocol.MethodPanelList, protocol.MethodCanvasClear,
	} {
		if s.results[method], err = c.Compile(id + "#/$defs/resultByMethod/properties/" + method); err != nil {
			return nil, fmt.Errorf("result schema for %s: %w", method, err)
		}
	}
	return s, nil
})

func loadSchemas(t *testing.T) *schemas {
	t.Helper()
	s, err := loadSchemasOnce()
	if err != nil {
		t.Fatalf("loading schema: %v", err)
	}
	return s
}

func decodeInstance(frame []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(frame))
}

// frameValidator checks a stream of frames from one session against the
// schema. It remembers the method of each request id so it can check
// results against the method's result schema.
type frameValidator struct {
	t       *testing.T
	s       *schemas
	methods map[string]string // request id (raw JSON) -> method
	checked int
}

func newFrameValidator(t *testing.T) *frameValidator {
	return &frameValidator{t: t, s: loadSchemas(t), methods: map[string]string{}}
}

// check validates one frame. c2s frames listed in expectInvalid are not
// validated (they are deliberately malformed to provoke errors).
func (v *frameValidator) check(dir mockphone.Direction, frame []byte, expectInvalid bool) {
	v.t.Helper()
	var m protocol.Message
	_ = json.Unmarshal(frame, &m)
	if dir == mockphone.ClientToServer {
		if m.ID != nil && m.Method != "" {
			v.methods[string(m.ID)] = m.Method
		}
		if expectInvalid {
			return
		}
	}
	inst, err := decodeInstance(frame)
	if err != nil {
		v.t.Errorf("%s frame is not JSON: %s", dir, frame)
		return
	}
	if err := v.s.message.Validate(inst); err != nil {
		v.t.Errorf("%s frame violates schema: %s\n%v", dir, frame, err)
		return
	}
	v.checked++
	if dir == mockphone.ServerToClient && m.Kind() == protocol.KindResponse && m.Result != nil {
		method, ok := v.methods[string(m.ID)]
		rs := v.s.results[method]
		if !ok || rs == nil {
			v.t.Errorf("result for id %s does not answer a known request method (%q)", m.ID, method)
			return
		}
		res, _ := decodeInstance(m.Result)
		if err := rs.Validate(res); err != nil {
			v.t.Errorf("%s result violates schema: %s\n%v", method, m.Result, err)
		}
	}
}

// TestClientAndMockFramesMatchSchema drives every method and every
// schema-valid error through the Go client and validates every frame in
// both directions.
func TestClientAndMockFramesMatchSchema(t *testing.T) {
	type frame struct {
		dir  mockphone.Direction
		data []byte
	}
	var (
		mu     sync.Mutex
		frames []frame
	)
	opts := quietOptions()
	opts.Unavailable = []string{protocol.SourceProximity}
	opts.Tap = func(d mockphone.Direction, b []byte) {
		mu.Lock()
		frames = append(frames, frame{d, bytes.Clone(b)})
		mu.Unlock()
	}
	srv, url := start(t, opts)

	// The client's own view of what it sent must match what the server read.
	var clientSent [][]byte
	c := dial(t, url, client.WithTap(func(d client.Direction, b []byte) {
		if d == client.ClientToServer {
			mu.Lock()
			clientSent = append(clientSent, bytes.Clone(b))
			mu.Unlock()
		}
	}))
	ctx := testCtx(t)
	if _, err := c.ListSensors(ctx); err != nil {
		t.Fatal(err)
	}
	for _, info := range protocol.Catalog() {
		switch info.Source {
		case protocol.SourceProximity:
			continue
		case protocol.SourcePanel, protocol.SourceNotification:
			continue // driven by exercisePanels below
		}
		sub, err := c.Subscribe(ctx, info.Source, client.SubscribeOptions{Type: info.Types[0], RateHz: 200, MinDelta: 0.001})
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Source == protocol.SourceUI:
			for range 3 {
				srv.Tap()
				nextEvent(t, sub)
			}
		case info.Source == protocol.SourceNFC:
			for range 2 { // with and without NDEF
				srv.ScanTag()
				nextEvent(t, sub)
			}
		case info.Mode == protocol.ModeOnChange:
			nextEvent(t, sub) // the current value, sent on subscribe
		default:
			for range 3 {
				nextEvent(t, sub)
			}
		}
		if _, err := c.Get(ctx, info.Source); err != nil {
			t.Errorf("get %s: %v", info.Source, err)
		}
		if err := sub.Unsubscribe(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exercisePanels(t, c, srv)
	// Schema-valid requests that produce errors.
	_, _ = c.Subscribe(ctx, "nope", client.SubscribeOptions{})
	_, _ = c.Subscribe(ctx, "proximity", client.SubscribeOptions{})
	_, _ = c.Subscribe(ctx, "gyro", client.SubscribeOptions{Type: "bogus"})
	_, _ = c.Get(ctx, "nope")
	_ = c.Call(ctx, "unsubscribe", protocol.UnsubscribeParams{Subscription: "s-404"}, nil)
	_, _ = c.Hello(ctx)
	_ = c.Call(ctx, "hello", protocol.HelloParams{Protocol: "phonetopc/0"}, nil)
	c.Close()
	srv.Close()

	mu.Lock()
	defer mu.Unlock()
	v := newFrameValidator(t)
	var serverRead [][]byte
	for _, f := range frames {
		v.check(f.dir, f.data, false)
		if f.dir == mockphone.ClientToServer {
			serverRead = append(serverRead, f.data)
		}
	}
	if len(serverRead) != len(clientSent) {
		t.Fatalf("client sent %d frames, server read %d", len(clientSent), len(serverRead))
	}
	for i := range clientSent {
		if !bytes.Equal(clientSent[i], serverRead[i]) {
			t.Errorf("frame %d differs: %s vs %s", i, clientSent[i], serverRead[i])
		}
	}
	t.Logf("validated %d frames", v.checked)
	if v.checked < 40 {
		t.Errorf("only %d frames validated", v.checked)
	}
}

// exercisePanels calls every method of PANELS.md, with results and errors,
// and makes the user produce every panel and notification event type.
func exercisePanels(t *testing.T, c *client.Client, srv *mockphone.Server) {
	t.Helper()
	ctx := testCtx(t)
	call := func(method string, params any) {
		t.Helper()
		if err := c.Call(ctx, method, params, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	panels, err := c.Subscribe(ctx, protocol.SourcePanel, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := c.Subscribe(ctx, protocol.SourceNotification, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	user := srv.User("")

	call(protocol.MethodNotify, protocol.NotifyParams{Title: "Build done", Body: "All green.\nShip it?", ID: "build",
		Actions: []protocol.NotifyAction{{ID: "ship", Label: "Ship"}}, Priority: protocol.PriorityHigh})
	call(protocol.MethodNotify, protocol.NotifyParams{Title: "Plain"})
	call(protocol.MethodNotify, protocol.NotifyParams{Title: "Swipe me", ID: "swipe"})
	for _, f := range []func() error{
		func() error { return user.Act("build", "ship") },
		func() error { return user.TapNotification("n-1") },
		func() error { return user.Dismiss("swipe") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
		nextEvent(t, notes)
	}
	call(protocol.MethodNotifyCancel, protocol.NotifyCancelParams{ID: "gone"})

	layout := json.RawMessage(`{"type":"column","children":[
		{"type":"text","id":"msg","text":"Hi","size":"title","tone":"accent"},
		{"type":"grid","cols":2,"children":[{"type":"button","id":"b","label":"Go"},{"type":"toggle","id":"t","label":"On"}]},
		{"type":"slider","id":"s","min":0,"max":10,"step":1},
		{"type":"progress","value":null},
		{"type":"text_input","id":"in","label":"Say"},
		{"type":"canvas","id":"c","weight":2}]}`)
	call(protocol.MethodPanelShow, map[string]any{"panel": "p", "title": "Everything", "layout": layout, "keep_awake": true})
	call(protocol.MethodPanelUpdate, protocol.PanelUpdateParams{Panel: "p", Updates: []protocol.WidgetUpdate{{Widget: "msg", Set: map[string]any{"text": "Bye"}}}})
	call(protocol.MethodPanelList, nil)
	for _, f := range []func() error{
		func() error { return user.Look("p") },
		func() error { return user.Press("p", "b") },
		func() error { _, err := user.Toggle("p", "t"); return err },
		func() error { _, err := user.Slide("p", "s", 3.4, true); return err },
		func() error { return user.Submit("p", "in", "hello") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
		nextEvent(t, panels)
	}
	stroke := mockphone.Stroke{Points: []protocol.Point{{0.1, 0.1, 0.5, 0}, {0.2, 0.2, 0.5, 16}, {0.3, 0.2, 0.6, 32}}, Chunk: 2}
	if err := user.Draw("p", "c", stroke); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, panels) // start
	nextEvent(t, panels) // end
	call(protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "p"})
	nextEvent(t, panels) // replayed start
	nextEvent(t, panels) // replayed end
	call(protocol.MethodCanvasClear, protocol.CanvasClearParams{Panel: "p", Widget: "c"})
	user.LookAway()
	nextEvent(t, panels) // hidden
	call(protocol.MethodPanelClose, protocol.PanelRef{Panel: "p"})
	call(protocol.MethodPanelShow, map[string]any{"panel": "q", "title": "Bye", "layout": json.RawMessage(`{"type":"text","text":""}`)})
	if err := user.Close("q"); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, panels) // closed

	// Schema-valid requests that produce errors.
	_ = c.Call(ctx, protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "q"}, nil)
	srv.AllowPanels(false)
	_ = c.Call(ctx, protocol.MethodPanelList, nil, nil)
	srv.AllowPanels(true)
	for _, ref := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		_ = c.Call(ctx, protocol.MethodPanelShow, map[string]any{"panel": ref, "title": ref, "layout": json.RawMessage(`{"type":"text","text":""}`)}, nil)
	}
	for _, s := range []*client.Subscription{panels, notes} {
		if err := s.Unsubscribe(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResultSchemasRejectBadResults makes sure the per-method result schemas
// are actually applied, so the frame checks above can't pass vacuously.
func TestResultSchemasRejectBadResults(t *testing.T) {
	s := loadSchemas(t)
	for _, c := range []struct {
		method, result string
		ok             bool
	}{
		{protocol.MethodSubscribe, `{"subscription":"s-1","source":"gyro","types":["rotation"],"mode":"continuous","rate_hz":50}`, true},
		{protocol.MethodSubscribe, `{"subscription":"s-1","source":"gyro","types":["rotation"],"rate_hz":50}`, false},
		{protocol.MethodSubscribe, `{"subscription":"s-1","source":"gyro","types":["rotation"],"mode":"sometimes"}`, false},
		{protocol.MethodUnsubscribe, `{}`, true},
		{protocol.MethodUnsubscribe, `{"ok":true}`, false},
		{protocol.MethodGet, `{"source":"battery","type":"state","value":{"level":50,"charging":false},"unit":"%","ts":1,"ts_mono_ns":2,"device":"d"}`, true},
		{protocol.MethodGet, `{"source":"battery","type":"state","value":50,"unit":"%","ts":1,"ts_mono_ns":2,"device":"d"}`, false},
		{protocol.MethodListSensors, `{}`, false},
		{protocol.MethodHello, `{"protocol":"phonetopc/2"}`, false},
	} {
		inst, err := decodeInstance([]byte(c.result))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.results[c.method].Validate(inst); (err == nil) != c.ok {
			t.Errorf("%s result %s: valid = %v, want %v (%v)", c.method, c.result, err == nil, c.ok, err)
		}
	}
}
