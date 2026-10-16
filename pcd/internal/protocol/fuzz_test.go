package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/melker12345/phonetopc/pcd/internal/conformance"
)

func seedFrames(f *testing.F) [][]byte {
	frames, err := conformance.Frames("../../../spec/conformance")
	if err != nil {
		f.Fatal(err)
	}
	if len(frames) == 0 {
		f.Fatal("no conformance frames")
	}
	return frames
}

// sameJSON reports whether a and b hold the same JSON value, numbers
// compared as written.
func sameJSON(t *testing.T, a, b []byte) bool {
	return reflect.DeepEqual(jsonValue(t, a), jsonValue(t, b))
}

func jsonValue(t *testing.T, b []byte) any {
	if len(b) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	return v
}

func FuzzParseRequest(f *testing.F) {
	for _, fr := range seedFrames(f) {
		f.Add(fr)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		id, method, params, errResp := ParseRequest(data)
		if errResp != nil {
			if errResp.Error == nil || errResp.Result != nil {
				t.Fatalf("error response without exactly an error: %+v", errResp)
			}
			if errResp.ID != nil && !ValidRequestID(errResp.ID) {
				t.Fatalf("error response echoes invalid id %q", errResp.ID)
			}
			if _, err := json.Marshal(errResp); err != nil {
				t.Fatalf("error response doesn't encode: %v", err)
			}
			return
		}
		if !json.Valid(data) {
			t.Fatalf("accepted invalid JSON %q", data)
		}
		if id == nil {
			return
		}
		if !ValidRequestID(id) {
			t.Fatalf("accepted invalid id %q", id)
		}
		var p any
		if params != nil {
			p = params
		}
		b, err := json.Marshal(NewRequest(id, method, p))
		if err != nil {
			t.Fatal(err)
		}
		id2, method2, params2, errResp2 := ParseRequest(b)
		if errResp2 != nil {
			t.Fatalf("re-encoded request %s rejected: %s", b, errResp2.Error.Message)
		}
		if !sameJSON(t, id, id2) || method != method2 || !sameJSON(t, params, params2) {
			t.Fatalf("round trip changed the request: %q %q %q -> %q %q %q", id, method, params, id2, method2, params2)
		}
	})
}

func FuzzValidRequestID(f *testing.F) {
	for _, s := range []string{`1`, `-1`, `0`, `"x"`, `9007199254740991`, `9007199254740992`, `1.0`, `1e3`, `null`, `[]`, `"ä"`, ` 7 `} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, id []byte) {
		if !ValidRequestID(id) || !json.Valid(id) {
			return
		}
		frame := []byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"method":"m"}`)
		got, _, _, errResp := ParseRequest(frame)
		if errResp != nil {
			t.Fatalf("valid id %q rejected in %s: %s", id, frame, errResp.Error.Message)
		}
		if !sameJSON(t, got, id) {
			t.Fatalf("id %q came back as %q", id, got)
		}
		if n, ok := ParseIntID(id); ok {
			if n < -MaxSafeID || n > MaxSafeID {
				t.Fatalf("id %q is out of range but valid", id)
			}
			if m, ok := ParseIntID(IntID(n)); !ok || m != n {
				t.Fatalf("IntID(%d) doesn't parse back", n)
			}
		}
	})
}

// paramTypes are the params a phone decodes from a PC, and a PC from the
// local API.
func paramTypes() []any {
	return []any{
		&HelloParams{}, &SubscribeParams{}, &GetParams{}, &UnsubscribeParams{},
		&NotifyParams{}, &NotifyCancelParams{}, &PanelShowParams{}, &PanelUpdateParams{}, &PanelAttachParams{}, &PanelRef{},
		&RequestApprovalParams{}, &DecryptParams{},
	}
}

func FuzzDecodeParams(f *testing.F) {
	for _, fr := range seedFrames(f) {
		var m Message
		if json.Unmarshal(fr, &m) == nil && m.Params != nil {
			f.Add([]byte(m.Params))
		}
	}
	f.Add([]byte(`null`))
	f.Add([]byte(` `))
	f.Fuzz(func(t *testing.T, params []byte) {
		for _, v := range paramTypes() {
			if err := DecodeParams(params, v); err != nil {
				if err.Code != CodeInvalidParams {
					t.Fatalf("%T: error %d, want %d", v, err.Code, CodeInvalidParams)
				}
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("%T %+v doesn't encode: %v", v, v, err)
			}
			again := reflect.New(reflect.TypeOf(v).Elem()).Interface()
			if err := DecodeParams(b, again); err != nil {
				t.Fatalf("%T: re-encoded %s rejected: %v", v, b, err)
			}
			if !reflect.DeepEqual(normalize(t, v), normalize(t, again)) {
				t.Fatalf("%T: round trip changed %s", v, b)
			}
		}
	})
}

// normalize encodes v once more, so that values that only differ in ways
// JSON can't express (an empty slice and nil, for omitempty) compare equal.
func normalize(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// FuzzMessage covers what a PC does with a frame from a phone.
func FuzzMessage(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"x","data":{"name":"method_not_found"}}}`,
		`{"jsonrpc":"2.0","method":"event","params":{"subscription":"s1","seq":1,"source":"gyro","type":"rotation","value":[1,2,3],"unit":"rad/s","ts":1,"ts_mono_ns":2,"device":"d"}}`,
		`{"jsonrpc":"2.0","method":"event","params":{"subscription":"s1","seq":1,"source":"panel","type":"stroke","value":{"panel":"p","pseq":3,"widget":"c","stroke":1,"phase":"start","tool":"stylus","size":[100,200],"points":[[0.5,0.5,1,0]]}}}`,
		`{"jsonrpc":"2.0","method":"event","params":{"source":"battery","type":"state","value":{"level":0.5,"charging":true}}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var m Message
		if json.Unmarshal(data, &m) != nil {
			return
		}
		switch m.Kind() {
		case KindResponse:
			if (m.Result == nil) == (m.Error == nil) {
				t.Fatal("response with both or neither of result and error")
			}
			if n, ok := ParseIntID(m.ID); ok {
				if got, _ := ParseIntID(IntID(n)); got != n {
					t.Fatalf("id %d doesn't round-trip", n)
				}
			}
			if m.Error != nil {
				_ = m.Error.Error()
				_, _ = ErrorCodeOf(m.Error)
			}
		case KindNotification:
			var ev Event
			if json.Unmarshal(m.Params, &ev) != nil {
				return
			}
			r := ev.Reading
			_, _ = r.Number()
			_, _ = r.Bool()
			_, _ = r.Vec3()
			_, _ = r.Quat()
			_, _ = r.Battery()
			_, _ = r.Position()
			if tag, err := r.Tag(); err == nil {
				for _, rec := range tag.NDEF {
					_, _ = rec.PayloadBytes()
				}
			}
			_, _ = r.Notification()
			if p, err := r.Panel(); err == nil {
				_, _ = p.BoolValue()
				_, _ = p.NumberValue()
				_, _ = p.TextValue()
				for _, pt := range p.Points {
					_, _, _, _ = pt.X(), pt.Y(), pt.Pressure(), pt.T()
				}
			}
			if info, ok := LookupSensor(r.Source); ok {
				_, _ = ValueDistance(info.Shape, r.Value, r.Value)
			}
		}
	})
}

func FuzzValueDistance(f *testing.F) {
	f.Add(string(ShapeVec3), []byte(`[1,2,3]`), []byte(`[1,2,4]`))
	f.Add(string(ShapeQuat), []byte(`[0,0,0,1]`), []byte(`[0,0,1,0]`))
	f.Add(string(ShapeNumber), []byte(`1.5`), []byte(`-2`))
	f.Add(string(ShapeBool), []byte(`true`), []byte(`false`))
	f.Fuzz(func(t *testing.T, shape string, a, b []byte) {
		d, err := ValueDistance(Shape(shape), a, b)
		if err == nil && d < 0 {
			t.Fatalf("negative distance %v", d)
		}
	})
}

func FuzzCheckText(f *testing.F) {
	for _, s := range []string{"", "x", "Deploy?", "two\nlines", "tab\t", "rlo\u202Etxt", "zw\u200Bj", "می\u200Cخواهم", "👨\u200D👩\u200D👧", "\xff", strings.Repeat("ä", 120)} {
		f.Add(s, 0, 100, true)
		f.Add(s, 1, 100, false)
	}
	f.Fuzz(func(t *testing.T, s string, minLen, maxLen int, newlines bool) {
		minLen, maxLen = clampLen(minLen), clampLen(maxLen)
		if err := CheckText(s, minLen, maxLen, newlines); err != nil {
			return
		}
		if !utf8.ValidString(s) {
			t.Fatalf("accepted invalid UTF-8 %q", s)
		}
		if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
			t.Fatalf("accepted %d characters, want %d-%d", n, minLen, maxLen)
		}
		for _, r := range s {
			if IsHidden(r) || (unicode.Is(unicode.Cc, r) && !(r == '\n' && newlines)) {
				t.Fatalf("accepted %U in %q", r, s)
			}
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back string
		if err := json.Unmarshal(b, &back); err != nil || back != s {
			t.Fatalf("%q doesn't survive JSON: %q, %v", s, back, err)
		}
	})
}

func clampLen(n int) int {
	if n < 0 {
		n = -n
	}
	return n % 4097
}

func FuzzCheckReason(f *testing.F) {
	for _, s := range []string{"Deploy?", "x", " padded", "nbsp\u00A0", "rlo\u202Etxt.exe", "lrm\u200E", "tag\U000E0041", "ls\u2028x", strings.Repeat("ä", 200), strings.Repeat("x", 201), "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if CheckReason(s) != nil {
			return
		}
		if err := CheckText(s, 1, MaxReasonLen, false); err != nil {
			t.Fatalf("reason %q accepted, but not as display text: %v", s, err)
		}
		first, _ := utf8.DecodeRuneInString(s)
		last, _ := utf8.DecodeLastRuneInString(s)
		if unicode.IsSpace(first) || unicode.IsSpace(last) {
			t.Fatalf("reason %q starts or ends with whitespace", s)
		}
		b, _ := json.Marshal(RequestApprovalParams{Reason: s})
		var p RequestApprovalParams
		if err := DecodeParams(b, &p); err != nil || p.Reason != s {
			t.Fatalf("reason %q doesn't survive JSON: %q, %v", s, p.Reason, err)
		}
	})
}

// Hidden characters are format characters, line or paragraph separators,
// or unassigned tag characters, and never ZWNJ or ZWJ.
func FuzzIsHidden(f *testing.F) {
	for _, r := range []rune{'a', 0x200B, 0x200C, 0x200D, 0x202E, 0x2028, 0xE0000, 0xE0041, 0xFEFF, 0x2065} {
		f.Add(r)
	}
	f.Fuzz(func(t *testing.T, r rune) {
		if !IsHidden(r) {
			return
		}
		if r == 0x200C || r == 0x200D {
			t.Fatalf("%U is hidden", r)
		}
		tags := r >= 0xE0000 && r <= 0xE007F
		if !unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) && !tags {
			t.Fatalf("%U is hidden but not a format character or separator", r)
		}
		if CheckText(string(r), 0, 1, true) == nil || CheckReason("a"+string(r)+"b") == nil {
			t.Fatalf("%U passes a check", r)
		}
	})
}

func FuzzCheckNotify(f *testing.F) {
	f.Add("Build done", "All tests pass", "build", "open", "Open", "high", uint8(1))
	f.Add("Hi", "", "", "a", "A", "", uint8(0))
	f.Add("x\u202E", "two\nlines", "bad id", "a", "", "low", uint8(4))
	f.Fuzz(func(t *testing.T, title, body, id, actionID, label, priority string, n uint8) {
		p := NotifyParams{Title: title, Body: body, ID: id, Priority: priority}
		for i := range int(n % 5) {
			a := NotifyAction{ID: actionID, Label: label}
			if i%2 == 1 {
				a.ID = fmt.Sprintf("%s%d", actionID, i)
			}
			p.Actions = append(p.Actions, a)
		}
		if CheckNotify(p) != nil {
			return
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var back NotifyParams
		if err := DecodeParams(b, &back); err != nil {
			t.Fatal(err)
		}
		if err := CheckNotify(back); err != nil {
			t.Fatalf("%s accepted, then rejected after JSON: %v", b, err)
		}
		if normalize(t, p) != normalize(t, back) {
			t.Fatalf("round trip changed %s", b)
		}
	})
}

func FuzzParseLayout(f *testing.F) {
	for _, s := range []string{
		`{"type":"text","text":"hi"}`,
		`{"type":"column","children":[{"type":"button","id":"b","label":"Go"},{"type":"toggle","id":"t","label":"On","value":true}]}`,
		`{"type":"grid","cols":2,"children":[{"type":"slider","id":"s","min":-1,"max":1,"step":0.5,"value":3},{"type":"progress","value":null,"tone":"good"}]}`,
		`{"type":"row","weight":2,"children":[{"type":"text_input","id":"in","max_length":10,"value":"abc","multiline":true},{"type":"canvas","id":"c","ink":true,"color":"#ff0000","width":2}]}`,
	} {
		f.Add([]byte(s))
	}
	for _, fr := range seedFrames(f) {
		var m struct {
			Params struct {
				Layout json.RawMessage `json:"layout"`
			} `json:"params"`
		}
		if json.Unmarshal(fr, &m) == nil && m.Params.Layout != nil {
			f.Add([]byte(m.Params.Layout))
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		l, err := ParseLayout(raw)
		if err != nil {
			return
		}
		if !json.Valid(raw) {
			t.Fatalf("accepted invalid JSON %q", raw)
		}
		checkLayout(t, l)

		b, err := json.Marshal(nodeJSON(l.Root))
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseLayout(b)
		if err != nil {
			t.Fatalf("re-encoded layout %s rejected: %v", b, err)
		}
		if again.Count != l.Count || len(again.ByID) != len(l.ByID) {
			t.Fatalf("re-encoded layout differs: %d/%d widgets", again.Count, l.Count)
		}

		// The local API and the client check a Widget's encoding, then send
		// the Widget: both must be the same.
		var w Widget
		if json.Unmarshal(raw, &w) != nil {
			return
		}
		if b, err = json.Marshal(w); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseLayout(b); err != nil {
			return
		}
		var w2 Widget
		if err := json.Unmarshal(b, &w2); err != nil {
			t.Fatalf("Widget %s doesn't decode: %v", b, err)
		}
		if b2, err := json.Marshal(w2); err != nil || string(b2) != string(b) {
			t.Fatalf("Widget %s encodes as %s after decoding", b, b2)
		}
	})
}

func checkLayout(t *testing.T, l *Layout) {
	if l.Count > MaxWidgets {
		t.Fatalf("%d widgets", l.Count)
	}
	ids := map[string]bool{}
	count := 0
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		count++
		if depth > MaxLayoutDepth {
			t.Fatalf("depth %d", depth)
		}
		if err := CheckWidget(n.Type, n.Props); err != nil {
			t.Fatalf("accepted widget fails CheckWidget: %v", err)
		}
		if n.ID != "" {
			if ids[n.ID] || l.ByID[n.ID] != n {
				t.Fatalf("id %q is not unique or not indexed", n.ID)
			}
			ids[n.ID] = true
		}
		if isContainer(n.Type) && (len(n.Children) == 0 || len(n.Children) > MaxChildren) {
			t.Fatalf("container with %d children", len(n.Children))
		}
		if !isContainer(n.Type) && len(n.Children) > 0 {
			t.Fatalf("%s with children", n.Type)
		}
		if v, ok := WidgetValue(n); ok && n.Type == WidgetSlider {
			lo, hi := propNumber(n, "min", 0), propNumber(n, "max", 1)
			if f := v.(float64); !(f >= lo && f <= hi) {
				t.Fatalf("slider value %v outside [%v, %v]", f, lo, hi)
			}
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(l.Root, 1)
	if count != l.Count || len(ids) != len(l.ByID) {
		t.Fatalf("Count %d, walked %d; ByID %d, ids %d", l.Count, count, len(l.ByID), len(ids))
	}
}

func propNumber(n *Node, k string, def float64) float64 {
	if x, ok := n.Props[k].(json.Number); ok {
		if f, err := x.Float64(); err == nil {
			return f
		}
	}
	return def
}

func nodeJSON(n *Node) map[string]any {
	m := map[string]any{}
	for k, v := range n.Props {
		m[k] = v
	}
	if isContainer(n.Type) {
		kids := make([]any, len(n.Children))
		for i, c := range n.Children {
			kids[i] = nodeJSON(c)
		}
		m["children"] = kids
	}
	return m
}

func FuzzPanelUpdate(f *testing.F) {
	f.Add(WidgetText, []byte(`{"text":"done","tone":"good"}`))
	f.Add(WidgetSlider, []byte(`{"value":0.5,"min":0,"max":2}`))
	f.Add(WidgetTextInput, []byte(`{"max_length":3,"value":"abcd"}`))
	f.Add(WidgetCanvas, []byte(`{"color":"#00ff00","width":100}`))
	f.Fuzz(func(t *testing.T, typ string, set []byte) {
		props, err := DecodeProps(set)
		if err != nil {
			return
		}
		if CheckWidget(typ, props) != nil {
			return
		}
		b, err := json.Marshal(props)
		if err != nil {
			t.Fatal(err)
		}
		again, err := DecodeProps(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckWidget(typ, again); err != nil {
			t.Fatalf("%s accepted for %s, then rejected after JSON: %v", set, typ, err)
		}
	})
}
