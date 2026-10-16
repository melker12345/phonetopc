package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckText(t *testing.T) {
	for _, c := range []struct {
		s        string
		min, max int
		newlines bool
		ok       bool
	}{
		{"hi", 1, 100, false, true},
		{"", 1, 100, false, false},
		{"", 0, 100, false, true},
		{strings.Repeat("🚀", 100), 1, 100, false, true}, // code points, not UTF-16 units
		{strings.Repeat("a", 101), 1, 100, false, false},
		{"a\nb", 1, 100, false, false},
		{"a\nb", 1, 100, true, true},
		{"a\tb", 1, 100, true, false},
		{"a\u202Eb", 1, 100, true, false},
		{"a\u2028b", 1, 100, true, false},
		{"a\u200Bb", 1, 100, true, false},
		{"a\u200Db", 1, 100, true, true},
		{"\xff", 0, 100, true, false},
	} {
		if err := CheckText(c.s, c.min, c.max, c.newlines); (err == nil) != c.ok {
			t.Errorf("CheckText(%q, %d, %d, %v) = %v, want ok %v", c.s, c.min, c.max, c.newlines, err, c.ok)
		}
	}
}

func TestCheckIDAndColor(t *testing.T) {
	for id, ok := range map[string]bool{"deck": true, "a.b-c_D9": true, "": false, "a b": false, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false} {
		if (CheckID(id) == nil) != ok {
			t.Errorf("CheckID(%q) ok = %v", id, !ok)
		}
	}
	for c, ok := range map[string]bool{"#00ff8A": true, "#fff": false, "red": false, "#0000000": false} {
		if (CheckColor(c) == nil) != ok {
			t.Errorf("CheckColor(%q) ok = %v", c, !ok)
		}
	}
}

func TestCheckNotify(t *testing.T) {
	ok := NotifyParams{Title: "Done", Body: "a\nb", ID: "n", Actions: []NotifyAction{{"a", "A"}, {"b", "B"}}, Priority: PriorityHigh}
	if err := CheckNotify(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []NotifyParams{
		{},
		{Title: "a\nb"},
		{Title: "t", Body: strings.Repeat("x", 1001)},
		{Title: "t", ID: "a/b"},
		{Title: "t", Actions: []NotifyAction{{"a", "x"}, {"a", "y"}}},
		{Title: "t", Actions: []NotifyAction{{"a", "x"}, {"b", "y"}, {"c", "z"}, {"d", "w"}}},
		{Title: "t", Actions: []NotifyAction{{"a", strings.Repeat("x", 31)}}},
		{Title: "t", Priority: "urgent"},
	} {
		if CheckNotify(bad) == nil {
			t.Errorf("CheckNotify(%+v) accepted", bad)
		}
	}
}

func TestParseLayout(t *testing.T) {
	l, err := ParseLayout(json.RawMessage(`{"type":"column","children":[
		{"type":"text","id":"t","text":"hi"},
		{"type":"grid","cols":2,"children":[{"type":"button","id":"b","label":"Go","weight":2},{"type":"toggle","id":"tg","label":"On","value":true}]},
		{"type":"slider","id":"s","min":0,"max":10,"value":42},
		{"type":"canvas","id":"c","icon":"ignored"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if l.Count != 7 || len(l.ByID) != 5 || l.ByID["c"].Type != WidgetCanvas {
		t.Fatalf("layout: count %d, ids %v", l.Count, l.ByID)
	}
	if v, _ := WidgetValue(l.ByID["s"]); v != 10.0 {
		t.Errorf("slider value %v, want clamped 10", v)
	}
	if v, _ := WidgetValue(l.ByID["tg"]); v != true {
		t.Errorf("toggle value %v", v)
	}
	if _, ok := WidgetValue(l.ByID["b"]); ok {
		t.Error("a button has no value")
	}

	for _, bad := range []string{
		``, `null`, `[]`, `{"id":"x"}`, `{"type":"image","id":"i"}`,
		`{"type":"button","label":"x"}`,
		`{"type":"column","children":[{"type":"button","id":"x","label":"a"},{"type":"text","id":"x","text":"b"}]}`,
		`{"type":"column","children":[]}`,
		`{"type":"grid","cols":2.5,"children":[{"type":"text","text":""}]}`,
		`{"type":"grid","cols":"2","children":[{"type":"text","text":""}]}`,
		`{"type":"slider","id":"s","min":1,"max":1}`,
		`{"type":"slider","id":"s","step":0}`,
		`{"type":"progress","value":1.5}`,
		`{"type":"text_input","id":"in","value":"abcdef","max_length":5}`,
		`{"type":"canvas","id":"c","color":"#fff"}`,
		`{"type":"button","id":"b","label":"x","tone":"red"}`,
		`{"type":"text"}`,
	} {
		if _, err := ParseLayout(json.RawMessage(bad)); err == nil {
			t.Errorf("ParseLayout(%s) accepted", bad)
		}
	}

	deep := `{"type":"text","text":"x"}`
	for range 7 {
		deep = `{"type":"column","children":[` + deep + `]}`
	}
	if _, err := ParseLayout(json.RawMessage(deep)); err != nil {
		t.Errorf("8 levels: %v", err)
	}
	if _, err := ParseLayout(json.RawMessage(`{"type":"column","children":[` + deep + `]}`)); err == nil {
		t.Error("9 levels accepted")
	}
}

func TestWidgetJSON(t *testing.T) {
	label := "Mute"
	w := Widget{Type: WidgetColumn, Kids: []Widget{
		{Type: WidgetText},
		{Type: WidgetButton, ID: "mute", Label: &label},
		{Type: WidgetToggle, ID: "t", Label: &label, Value: false},
	}}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"column","children":[{"type":"text","text":""},{"type":"button","id":"mute","label":"Mute"},{"type":"toggle","id":"t","label":"Mute","value":false}]}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	if _, err := ParseLayout(b); err != nil {
		t.Error(err)
	}
}

func TestPanelEventValues(t *testing.T) {
	r := Reading{Source: SourcePanel, Type: TypeStroke, Value: json.RawMessage(
		`{"panel":"p","widget":"c","pseq":3,"stroke":0,"phase":"start","tool":"finger","size":[360,640],"points":[[0.5,0.25,1,0],[0.5,0.3,0.9,16]]}`)}
	e, err := r.Panel()
	if err != nil {
		t.Fatal(err)
	}
	if e.Pseq != 3 || e.Stroke == nil || *e.Stroke != 0 || len(e.Points) != 2 || e.Points[1].T() != 16 || e.Points[1].Y() != 0.3 {
		t.Errorf("%+v", e)
	}
	b, _ := json.Marshal(e.Points[1])
	if string(b) != `[0.5,0.3,0.9,16]` {
		t.Errorf("point JSON %s", b)
	}
	c := PanelEvent{Value: json.RawMessage(`0.25`)}
	if f, err := c.NumberValue(); err != nil || f != 0.25 {
		t.Errorf("%v %v", f, err)
	}
	n, err := Reading{Value: json.RawMessage(`{"notification":"n","action":"ok"}`)}.Notification()
	if err != nil || n.Action != "ok" {
		t.Errorf("%+v %v", n, err)
	}
}
