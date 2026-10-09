package mockphone_test

import (
	"bytes"
	"encoding/json"
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
// results are additionally checked against the matching $defs entry.
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
	for method, def := range map[string]string{
		protocol.MethodHello:       "helloResult",
		protocol.MethodListSensors: "listSensorsResult",
		protocol.MethodSubscribe:   "subscribeResult",
		protocol.MethodGet:         "reading",
	} {
		if s.results[method], err = c.Compile(id + "#/$defs/" + def); err != nil {
			return nil, err
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
		method := v.methods[string(m.ID)]
		if rs := v.s.results[method]; rs != nil {
			res, _ := decodeInstance(m.Result)
			if err := rs.Validate(res); err != nil {
				v.t.Errorf("%s result violates schema: %s\n%v", method, m.Result, err)
			}
		} else if method == protocol.MethodUnsubscribe && string(m.Result) != "{}" {
			v.t.Errorf("unsubscribe result %s, want {}", m.Result)
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
		if info.Source == protocol.SourceProximity {
			continue
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
