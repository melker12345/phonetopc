package mockphone_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Conformance transcripts (spec/conformance/*.jsonl) are recorded from real
// sessions against the mock:
//
//	go test ./internal/mockphone -run TestTranscripts -record
//
// Without -record, the test replays every scenario and checks that the
// committed transcripts still match (ignoring timestamps, sensor values and
// event counts), and that every frame conforms to the JSON Schema.
var record = flag.Bool("record", false, "rewrite spec/conformance transcripts")

// transcriptLine is one line of a transcript. Exactly one of Msg, Raw,
// Binary and Close is set.
type transcriptLine struct {
	Dir    mockphone.Direction `json:"dir"`
	Msg    json.RawMessage     `json:"msg,omitempty"`
	Raw    *string             `json:"raw,omitempty"`    // text frame that is not valid JSON
	Binary string              `json:"binary,omitempty"` // base64 of a binary frame
	Close  *closeInfo          `json:"close,omitempty"`  // close frame
}

type closeInfo struct {
	Code   int    `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// step is one client action in a scenario.
type step struct {
	send    string // text frame; the runner then waits for a response
	invalid bool   // send is deliberately not schema-valid
	events  int    // wait until this many events have arrived
	tap     bool   // trigger a ui/tile_tap on the mock
	binary  []byte // send a binary frame; the runner then waits for the close
}

type scenario struct {
	name  string
	about string
	opts  func(*mockphone.Options)
	steps []step
}

func req(id any, method string, params string) string {
	idJSON, _ := json.Marshal(id)
	if params == "" {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q}`, idJSON, method)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, idJSON, method, params)
}

const helloParams = `{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}`

var scenarios = []scenario{
	{
		name:  "happy_path",
		about: "hello, list_sensors, subscribe to gyro, a few events, get, unsubscribe",
		steps: []step{
			{send: req(1, "hello", helloParams)},
			{send: req(2, "list_sensors", "")},
			{send: req(3, "subscribe", `{"source":"gyro","type":"rotation","rate_hz":10}`)},
			{events: 3},
			{send: req(4, "get", `{"source":"battery"}`)},
			{send: req(5, "unsubscribe", `{"subscription":"s-1"}`)},
		},
	},
	{
		name:  "on_change",
		about: "on_change sources: current value on subscribe, tile taps, get, string ids",
		steps: []step{
			{send: req("a", "hello", helloParams)},
			{send: req("b", "subscribe", `{"source":"light"}`)},
			{events: 1},
			{send: req("c", "subscribe", `{"source":"ui","type":"tile_tap"}`)},
			{tap: true},
			{events: 2},
			{send: req("d", "get", `{"source":"ui"}`)},
			{send: req("e", "unsubscribe", `{"subscription":"s-1"}`)},
			{send: req("f", "unsubscribe", `{"subscription":"s-2"}`)},
		},
	},
	{
		name:  "rate_and_min_delta",
		about: "rate_hz clamping and a min_delta filter that only lets the first event through",
		steps: []step{
			{send: req(1, "hello", helloParams)},
			{send: req(2, "subscribe", `{"source":"game_rotation_vector","rate_hz":0.5}`)},
			{send: req(3, "unsubscribe", `{"subscription":"s-1"}`)},
			{send: req(4, "subscribe", `{"source":"accelerometer","rate_hz":1000,"min_delta":100}`)},
			{events: 1},
			{send: req(5, "unsubscribe", `{"subscription":"s-2"}`)},
		},
	},
	{
		name:  "error_hello",
		about: "hello_required (-32003) and unsupported_protocol (-32004)",
		steps: []step{
			{send: req(1, "list_sensors", "")},
			{send: req(2, "hello", `{"protocol":"phonetopc/2"}`)},
			{send: req(3, "hello", helloParams)},
		},
	},
	{
		name:  "error_methods",
		about: "method and params errors: -32601, -32001, -32002, -32602, -32005, -32006 (proximity is unavailable)",
		opts:  func(o *mockphone.Options) { o.Unavailable = []string{protocol.SourceProximity} },
		steps: []step{
			{send: req(1, "hello", helloParams)},
			{send: req(2, "no_such_method", ""), invalid: true},
			{send: req(3, "subscribe", `{"source":"nope"}`)},
			{send: req(4, "subscribe", `{"source":"proximity"}`)},
			{send: req(5, "get", `{"source":"proximity"}`)},
			{send: req(6, "subscribe", `{"source":"gyro","type":"bogus"}`)},
			{send: req(7, "subscribe", `{"source":"gyro","rate_hz":0}`), invalid: true},
			{send: req(8, "subscribe", `{}`), invalid: true},
			{send: req(9, "unsubscribe", `{"subscription":"s-99"}`)},
			{send: req(10, "get", `{"source":"ui"}`)},
			{send: req(11, "get", `{"source":"nope"}`)},
		},
	},
	{
		name:  "error_framing",
		about: "parse error (-32700), batch and malformed requests (-32600), binary frame (close 1003)",
		steps: []step{
			{send: `{"jsonrpc":"2.0","id":1,"method":`, invalid: true},
			{send: `[{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1"}}]`, invalid: true},
			{send: `{"jsonrpc":"2.0","method":"hello","params":{"protocol":"phonetopc/1"}}`, invalid: true},
			{send: `{"jsonrpc":"1.0","id":2,"method":"hello","params":{"protocol":"phonetopc/1"}}`, invalid: true},
			{send: req(3, "hello", helloParams)},
			{binary: []byte{0x01, 0x02, 0x03}},
		},
	},
}

// runScenario plays sc against a fresh mock and returns the transcript.
// It also validates every frame against the schema.
func runScenario(t *testing.T, sc scenario) []transcriptLine {
	t.Helper()
	var (
		mu    sync.Mutex
		lines []transcriptLine
	)
	add := func(l transcriptLine) {
		mu.Lock()
		lines = append(lines, l)
		mu.Unlock()
	}
	invalid := map[string]bool{}
	for _, s := range sc.steps {
		if s.invalid {
			invalid[s.send] = true
		}
	}
	v := newFrameValidator(t)

	opts := quietOptions()
	if sc.opts != nil {
		sc.opts(&opts)
	}
	opts.Tap = func(dir mockphone.Direction, frame []byte) {
		mu.Lock()
		v.check(dir, frame, dir == mockphone.ClientToServer && invalid[string(frame)])
		mu.Unlock()
		l := transcriptLine{Dir: dir}
		if json.Valid(frame) {
			var buf bytes.Buffer
			_ = json.Compact(&buf, frame)
			l.Msg = buf.Bytes()
		} else {
			s := string(frame)
			l.Raw = &s
		}
		add(l)
	}
	srv, url := start(t, opts)
	r := dialRaw(t, url, nil)
	ctx := testCtx(t)

	events := 0
	readOne := func() (*protocol.Message, error) {
		m, err := r.read(ctx)
		if err == nil && m.Kind() == protocol.KindNotification {
			events++
		}
		return m, err
	}
	for _, s := range sc.steps {
		switch {
		case s.send != "":
			r.sendRaw(s.send)
			for {
				m, err := readOne()
				if err != nil {
					t.Fatalf("waiting for response to %s: %v", s.send, err)
				}
				if m.Kind() == protocol.KindResponse {
					break
				}
			}
		case s.events > 0:
			for events < s.events {
				if _, err := readOne(); err != nil {
					t.Fatalf("waiting for events: %v", err)
				}
			}
		case s.tap:
			srv.Tap()
		case s.binary != nil:
			add(transcriptLine{Dir: mockphone.ClientToServer, Binary: base64.StdEncoding.EncodeToString(s.binary)})
			if err := r.conn.Write(ctx, websocket.MessageBinary, s.binary); err != nil {
				t.Fatal(err)
			}
			var err error
			for err == nil {
				_, err = readOne()
			}
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("want close frame, got %v", err)
			}
			add(transcriptLine{Dir: mockphone.ServerToClient, Close: &closeInfo{Code: int(ce.Code), Reason: ce.Reason}})
		}
	}
	r.conn.Close(websocket.StatusNormalClosure, "")
	srv.Close()
	mu.Lock()
	defer mu.Unlock()
	return lines
}

func transcriptPath(name string) string {
	return filepath.Join(specDir, "conformance", name+".jsonl")
}

func TestTranscripts(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			got := runScenario(t, sc)
			if *record {
				writeTranscript(t, transcriptPath(sc.name), got)
				return
			}
			want := readTranscript(t, transcriptPath(sc.name))
			g, w := normalize(t, got), normalize(t, want)
			if !reflect.DeepEqual(g, w) {
				t.Errorf("transcript %s no longer matches the mock (re-record with -record if intended)\n got: %s\nwant: %s",
					sc.name, mustJSON(g), mustJSON(w))
			}
		})
	}
}

func writeTranscript(t *testing.T, path string, lines []transcriptLine) {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d lines)", path, len(lines))
}

func readTranscript(t *testing.T, path string) []transcriptLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("%v (record transcripts with -record)", err)
	}
	defer f.Close()
	var lines []transcriptLine
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		var l transcriptLine
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("%s:%d: %v", path, n, err)
		}
		if l.Dir != mockphone.ClientToServer && l.Dir != mockphone.ServerToClient {
			t.Fatalf("%s:%d: bad dir %q", path, n, l.Dir)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// normalize drops event notifications (their number depends on timing) and
// blanks the fields that legitimately vary between runs: ts, ts_mono_ns and
// value.
func normalize(t *testing.T, lines []transcriptLine) []any {
	t.Helper()
	var out []any
	for _, l := range lines {
		if l.Msg == nil {
			out = append(out, map[string]any{"dir": string(l.Dir), "raw": l.Raw, "binary": l.Binary, "close": l.Close})
			continue
		}
		var m any // usually an object; a batch array in error_framing
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		if obj, ok := m.(map[string]any); ok && obj["method"] == protocol.MethodEvent {
			continue
		}
		blank(m)
		out = append(out, map[string]any{"dir": string(l.Dir), "msg": m})
	}
	return out
}

func blank(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			switch k {
			case "ts", "ts_mono_ns", "value":
				v[k] = "<varies>"
			default:
				blank(x)
			}
		}
	case []any:
		for _, x := range v {
			blank(x)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// TestTranscriptFilesMatchSchema checks the committed files themselves: every
// "msg" from the server must be schema-valid, as must every client message
// that the scenario doesn't mark as deliberately invalid.
func TestTranscriptFilesMatchSchema(t *testing.T) {
	if *record {
		t.Skip("recording")
	}
	for _, sc := range scenarios {
		invalid := map[string]bool{}
		for _, s := range sc.steps {
			if s.invalid {
				var buf bytes.Buffer
				if json.Compact(&buf, []byte(s.send)) == nil {
					invalid[buf.String()] = true
				}
			}
		}
		v := newFrameValidator(t)
		for _, l := range readTranscript(t, transcriptPath(sc.name)) {
			if l.Msg != nil {
				v.check(l.Dir, l.Msg, l.Dir == mockphone.ClientToServer && invalid[string(l.Msg)])
			}
		}
	}
}
