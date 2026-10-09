package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorCodeNames(t *testing.T) {
	want := map[ErrorCode]string{
		CodeUnknownSource:       "unknown_source",
		CodeSourceUnavailable:   "source_unavailable",
		CodeHelloRequired:       "hello_required",
		CodeUnsupportedProtocol: "unsupported_protocol",
		CodeUnknownSubscription: "unknown_subscription",
		CodeNoValue:             "no_value",
		CodeParseError:          "parse_error",
		CodeInvalidRequest:      "invalid_request",
		CodeMethodNotFound:      "method_not_found",
		CodeInvalidParams:       "invalid_params",
		CodeInternalError:       "internal_error",
	}
	for code, name := range want {
		if got := code.Name(); got != name {
			t.Errorf("%d.Name() = %q, want %q", code, got, name)
		}
	}
	if ErrorCode(-1).Name() != "" {
		t.Error("unknown code should have empty name")
	}
}

func TestErrorJSONAndIs(t *testing.T) {
	e := NewError(CodeUnknownSource, "unknown source %q", "nope")
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"code":-32001,"message":"unknown source \"nope\"","data":{"name":"unknown_source"}}`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
	wrapped := fmt.Errorf("ctx: %w", e)
	if !errors.Is(wrapped, &Error{Code: CodeUnknownSource}) {
		t.Error("errors.Is by code failed")
	}
	if errors.Is(wrapped, &Error{Code: CodeNoValue}) {
		t.Error("errors.Is matched a different code")
	}
	if c, ok := ErrorCodeOf(wrapped); !ok || c != CodeUnknownSource {
		t.Errorf("ErrorCodeOf = %v, %v", c, ok)
	}
	if !strings.Contains(e.Error(), "unknown_source") {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestResponseEncoding(t *testing.T) {
	cases := []struct {
		resp *Response
		want string
	}{
		{NewResult(IntID(4), UnsubscribeResult{}), `{"jsonrpc":"2.0","id":4,"result":{}}`},
		{NewErrorResponse(nil, NewError(CodeParseError, "bad")), `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"bad","data":{"name":"parse_error"}}}`},
		{NewResult(json.RawMessage(`"abc"`), SubscribeResult{Subscription: "s-1", Source: "ui", Types: []string{"tile_tap"}, Mode: ModeOnChange}),
			`{"jsonrpc":"2.0","id":"abc","result":{"subscription":"s-1","source":"ui","types":["tile_tap"],"mode":"on_change"}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.resp)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("got  %s\nwant %s", b, c.want)
		}
	}
}

func TestEventEncodingRoundTrip(t *testing.T) {
	ev := &Event{Subscription: "s-7f3a", Seq: 1042, Reading: Reading{
		Source: "gyro", Type: "rotation", Unit: "rad/s", TS: 1760000000123, TSMonoNS: 81234567890123, Device: "a1b2c3d4",
	}}
	if err := ev.SetValue([]float64{0.01, -0.32, 0.05}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(NewEventNotification(ev))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"jsonrpc":"2.0","method":"event","params":{"subscription":"s-7f3a","seq":1042,"source":"gyro","type":"rotation","value":[0.01,-0.32,0.05],"unit":"rad/s","ts":1760000000123,"ts_mono_ns":81234567890123,"device":"a1b2c3d4"}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Kind() != KindNotification {
		t.Fatalf("kind = %v", m.Kind())
	}
	var back Event
	if err := json.Unmarshal(m.Params, &back); err != nil {
		t.Fatal(err)
	}
	v, err := back.Vec3()
	if err != nil || v != [3]float64{0.01, -0.32, 0.05} || back.Seq != 1042 || back.Subscription != "s-7f3a" {
		t.Errorf("round trip mismatch: %+v %v %v", back, v, err)
	}
	if _, err := back.Quat(); err == nil {
		t.Error("Quat() on a vec3 should fail")
	}
}

func TestMessageKind(t *testing.T) {
	cases := map[string]Kind{
		`{"jsonrpc":"2.0","id":1,"method":"hello"}`:                            KindRequest,
		`{"jsonrpc":"2.0","method":"event","params":{}}`:                       KindNotification,
		`{"jsonrpc":"2.0","id":1,"result":{}}`:                                 KindResponse,
		`{"jsonrpc":"2.0","id":null,"result":null}`:                            KindResponse,
		`{"jsonrpc":"2.0","id":null,"error":{"code":1,"message":""}}`:          KindResponse,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":""}}`: KindInvalid,
		`{"jsonrpc":"2.0"}`: KindInvalid,
	}
	for in, want := range cases {
		var m Message
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatal(err)
		}
		if got := m.Kind(); got != want {
			t.Errorf("%s: kind %v, want %v", in, got, want)
		}
	}
}

func TestValidRequestID(t *testing.T) {
	for in, want := range map[string]bool{
		`1`: true, `-5`: true, `0`: true, `"abc"`: true, `""`: true,
		`9007199254740991`: true, `-9007199254740991`: true,
		`9007199254740992`: false, `-9007199254740992`: false, `99999999999999999999`: false,
		`1.5`: false, `1.0`: false, `1e3`: false, `null`: false, `true`: false, `[]`: false, `{}`: false, ``: false,
	} {
		if got := ValidRequestID(json.RawMessage(in)); got != want {
			t.Errorf("ValidRequestID(%s) = %v, want %v", in, got, want)
		}
	}
	if n, ok := ParseIntID(IntID(42)); !ok || n != 42 {
		t.Errorf("ParseIntID(IntID(42)) = %d, %v", n, ok)
	}
	if _, ok := ParseIntID(json.RawMessage(`"42"`)); ok {
		t.Error("string id parsed as int")
	}
}

func TestCatalog(t *testing.T) {
	cat := Catalog()
	if len(cat) != 7 {
		t.Fatalf("catalog has %d entries", len(cat))
	}
	for _, s := range cat {
		if len(s.Types) == 0 || !s.Available {
			t.Errorf("bad entry %+v", s)
		}
		cont := s.Mode == ModeContinuous
		hasRates := s.MinRateHz > 0 && s.MaxRateHz > 0 && s.DefaultRateHz > 0
		if cont != hasRates {
			t.Errorf("%s: rate fields must be present iff continuous", s.Source)
		}
	}
	cat[0].Source = "mutated"
	if _, ok := LookupSensor(SourceAccelerometer); !ok {
		t.Error("Catalog() must return a copy")
	}
	g, ok := LookupSensor(SourceGyro)
	if !ok || g.Unit != "rad/s" || g.Shape != ShapeVec3 || !g.HasType(TypeRotation) || g.HasType("x") {
		t.Errorf("gyro entry: %+v", g)
	}
	if _, ok := LookupSensor("nope"); ok {
		t.Error("lookup of unknown source succeeded")
	}
}

func TestClampRate(t *testing.T) {
	g, _ := LookupSensor(SourceGyro)
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		in   *float64
		want float64
	}{{nil, 50}, {f(0.1), 1}, {f(100), 100}, {f(1000), 200}, {f(12.5), 12.5}} {
		if got := g.ClampRate(c.in); got != c.want {
			t.Errorf("ClampRate(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	p, _ := LookupSensor(SourceProximity)
	if p.ClampRate(f(10)) != 0 {
		t.Error("on_change sensors have no rate")
	}
}

func TestValueDistance(t *testing.T) {
	cases := []struct {
		shape Shape
		a, b  string
		want  float64
	}{
		{ShapeNumber, `3`, `5.5`, 2.5},
		{ShapeVec3, `[0,0,0]`, `[3,4,0]`, 5},
		{ShapeQuat, `[0,0,0,1]`, `[0,0,0,1]`, 0},
	}
	for _, c := range cases {
		got, err := ValueDistance(c.shape, json.RawMessage(c.a), json.RawMessage(c.b))
		if err != nil || got != c.want {
			t.Errorf("ValueDistance(%s, %s, %s) = %v, %v; want %v", c.shape, c.a, c.b, got, err, c.want)
		}
	}
	if _, err := ValueDistance(ShapeVec3, json.RawMessage(`[1,2]`), json.RawMessage(`[1,2,3]`)); err == nil {
		t.Error("length mismatch should fail")
	}
	for _, sh := range []Shape{ShapeBool, ShapeObject} {
		if _, err := ValueDistance(sh, json.RawMessage(`true`), json.RawMessage(`true`)); !errors.Is(err, ErrNoDistance) {
			t.Errorf("%s: err = %v, want ErrNoDistance", sh, err)
		}
	}
}
