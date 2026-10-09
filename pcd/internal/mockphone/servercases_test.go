package mockphone_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Runs the language-neutral server suite spec/conformance/server-cases.json
// (format: spec/conformance/README.md) against the mock.

type serverCase struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Hello       bool       `json:"hello"`
	Setup       []caseStep `json:"setup"`
	caseStep
}

type caseStep struct {
	Send   json.RawMessage `json:"send"`
	Expect caseExpect      `json:"expect"`
}

type caseExpect struct {
	Error   *int                       `json:"error"`
	Name    string                     `json:"name"`
	ID      json.RawMessage            `json:"id"`
	Data    map[string]json.RawMessage `json:"data"`
	Result  string                     `json:"result"`
	NoReply bool                       `json:"no_reply"`
	Close   *int                       `json:"close"`
}

const (
	caseHello = `{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}}`
	caseProbe = `{"jsonrpc":"2.0","id":"conformance-probe","method":"list_sensors"}`
)

func loadServerCases(t *testing.T) []serverCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(specDir, "conformance", "server-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []serverCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no server cases")
	}
	return cases
}

func TestServerCases(t *testing.T) {
	opts := quietOptions()
	// The fixture the suite assumes: proximity unavailable, battery has a
	// value, ui has none.
	opts.Unavailable = []string{protocol.SourceProximity}
	_, url := start(t, opts)

	seen := map[string]bool{}
	for _, c := range loadServerCases(t) {
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			r := dialRaw(t, url, nil)
			if c.Hello {
				r.sendRaw(caseHello)
				m := r.response()
				if m.Error != nil || string(m.ID) != `"conformance-hello"` {
					t.Fatalf("hello failed: %+v", m)
				}
			}
			for i, s := range c.Setup {
				runCaseStep(t, r, fmt.Sprintf("setup[%d]", i), s)
			}
			runCaseStep(t, r, "send", c.caseStep)
		})
	}
}

func runCaseStep(t *testing.T, r *rawConn, where string, s caseStep) {
	t.Helper()
	typ, payload := caseFrame(t, s.Send)
	if err := r.conn.Write(testCtx(t), typ, payload); err != nil {
		t.Fatalf("%s: write: %v", where, err)
	}
	ex := s.Expect
	switch {
	case ex.Close != nil:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, data, err := r.conn.Read(ctx)
			if err != nil {
				if got := websocket.CloseStatus(err); int(got) != *ex.Close {
					t.Fatalf("%s: want close %d, got %v", where, *ex.Close, err)
				}
				return
			}
			if bytes.Contains(data, []byte(`"id"`)) {
				t.Fatalf("%s: want close %d, got response %s", where, *ex.Close, data)
			}
		}
	case ex.NoReply:
		r.sendRaw(caseProbe)
		if m := r.response(); string(m.ID) != `"conformance-probe"` {
			t.Fatalf("%s: want no reply, got %+v (error %+v)", where, m, m.Error)
		}
	case ex.Result != "":
		m := r.response()
		if m.Error != nil || m.Result == nil {
			t.Fatalf("%s: want a result, got error %+v", where, m.Error)
		}
		wantSameJSON(t, where+": id", m.ID, ex.ID)
	case ex.Error != nil:
		m := r.response()
		if m.Error == nil {
			t.Fatalf("%s: want error %d, got result %s", where, *ex.Error, m.Result)
		}
		if int(m.Error.Code) != *ex.Error {
			t.Errorf("%s: error code %d (%s), want %d", where, m.Error.Code, m.Error.Message, *ex.Error)
		}
		if m.Error.Data == nil || m.Error.Data.Name != ex.Name {
			t.Errorf("%s: error data %+v, want name %q", where, m.Error.Data, ex.Name)
		}
		wantSameJSON(t, where+": id", m.ID, ex.ID)
		if len(ex.Data) > 0 {
			raw, err := json.Marshal(m.Error.Data)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			for k, want := range ex.Data {
				wantSameJSON(t, where+": data."+k, got[k], want)
			}
		}
	default:
		t.Fatalf("%s: unknown expect form", where)
	}
}

// caseFrame turns a case's send member into a WebSocket frame.
func caseFrame(t *testing.T, send json.RawMessage) (websocket.MessageType, []byte) {
	t.Helper()
	var special map[string]json.RawMessage
	if bytes.HasPrefix(bytes.TrimSpace(send), []byte("{")) && json.Unmarshal(send, &special) == nil && len(special) == 1 {
		if v, ok := special["raw"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				t.Fatal(err)
			}
			return websocket.MessageText, []byte(s)
		}
		if v, ok := special["binary"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				t.Fatal(err)
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				t.Fatal(err)
			}
			return websocket.MessageBinary, b
		}
	}
	return websocket.MessageText, send
}

// wantSameJSON compares two JSON values, keeping numbers exact.
func wantSameJSON(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	decode := func(b json.RawMessage) any {
		if len(b) == 0 {
			return "<absent>"
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
	if g, w := decode(got), decode(want); !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}
