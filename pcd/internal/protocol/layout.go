package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Value rules (PANELS.md §8) and layout checks (§5). Everything here can be
// checked from a request alone, so a server answers violations with -32602
// at step 7 of the order of checks; clients may check before sending.

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// CheckID applies to panel, widget, notification and action ids.
func CheckID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%q is not a valid id (1-64 of A-Z a-z 0-9 _ . -)", id)
	}
	return nil
}

// CheckText checks display text: min to max code points, no control
// characters (except newline when newlines is true) and nothing IsHidden.
func CheckText(s string, minLen, maxLen int, newlines bool) error {
	if !utf8.ValidString(s) {
		return errors.New("is not valid UTF-8")
	}
	n := utf8.RuneCountInString(s)
	switch {
	case n < minLen && minLen == 1:
		return errors.New("must not be empty")
	case n < minLen || n > maxLen:
		return fmt.Errorf("has %d characters, %d-%d are allowed", n, minLen, maxLen)
	}
	for _, r := range s {
		if r == '\n' && newlines {
			continue
		}
		if unicode.Is(unicode.Cc, r) {
			return fmt.Errorf("must not contain control characters (%U)", r)
		}
		if IsHidden(r) {
			return fmt.Errorf("must not contain bidirectional controls, invisible characters or line separators (%U)", r)
		}
	}
	return nil
}

var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func CheckColor(s string) error {
	if !colorPattern.MatchString(s) {
		return fmt.Errorf("%q is not a #rrggbb color", s)
	}
	return nil
}

// §3.1.
func CheckNotify(p NotifyParams) error {
	if err := CheckText(p.Title, 1, 100, false); err != nil {
		return fmt.Errorf("title %w", err)
	}
	if err := CheckText(p.Body, 0, 1000, true); err != nil {
		return fmt.Errorf("body %w", err)
	}
	if p.ID != "" {
		if err := CheckID(p.ID); err != nil {
			return fmt.Errorf("id: %w", err)
		}
	}
	if len(p.Actions) > MaxNotifyActions {
		return fmt.Errorf("actions: at most %d are allowed", MaxNotifyActions)
	}
	seen := map[string]bool{}
	for i, a := range p.Actions {
		if err := CheckID(a.ID); err != nil {
			return fmt.Errorf("actions[%d].id: %w", i, err)
		}
		if seen[a.ID] {
			return fmt.Errorf("actions[%d].id %q is used twice", i, a.ID)
		}
		seen[a.ID] = true
		if err := CheckText(a.Label, 1, 30, false); err != nil {
			return fmt.Errorf("actions[%d].label %w", i, err)
		}
	}
	switch p.Priority {
	case "", PriorityDefault, PriorityHigh:
	default:
		return fmt.Errorf("priority must be %q or %q", PriorityDefault, PriorityHigh)
	}
	return nil
}

// Node is one widget of a checked layout. Props holds all of its members
// as decoded JSON (numbers as json.Number), except children.
type Node struct {
	Type     string
	ID       string
	Props    map[string]any
	Children []*Node
}

type Layout struct {
	Root *Node
	// Only widgets with an id.
	ByID  map[string]*Node
	Count int
}

// ParseLayout decodes and checks a layout (§5): every widget's members, the
// required ids, unique ids, at most 256 widgets and 8 levels.
func ParseLayout(raw json.RawMessage) (*Layout, error) {
	v, err := decodeAny(raw)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	l := &Layout{ByID: map[string]*Node{}}
	if l.Root, err = l.node(v, "layout", 1); err != nil {
		return nil, err
	}
	return l, nil
}

// decodeAny decodes JSON keeping numbers exact.
func decodeAny(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("is required")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("has data after the JSON value")
	}
	return v, nil
}

// DecodeProps decodes the set member of a panel_update item: an object.
func DecodeProps(raw json.RawMessage) (map[string]any, error) {
	v, err := decodeAny(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("must be an object")
	}
	return m, nil
}

func (l *Layout) node(v any, path string, depth int) (*Node, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a widget object", path)
	}
	if depth > MaxLayoutDepth {
		return nil, fmt.Errorf("%s: the layout is more than %d levels deep", path, MaxLayoutDepth)
	}
	l.Count++
	if l.Count > MaxWidgets {
		return nil, fmt.Errorf("the layout has more than %d widgets", MaxWidgets)
	}
	typ, _ := m["type"].(string)
	if _, ok := m["type"]; !ok {
		return nil, fmt.Errorf("%s.type is required", path)
	}
	n := &Node{Type: typ, Props: map[string]any{}}
	for k, x := range m {
		if k != "children" {
			n.Props[k] = x
		}
	}
	if err := CheckWidget(typ, n.Props); err != nil {
		return nil, fmt.Errorf("%s.%w", path, err)
	}
	if id, ok := m["id"].(string); ok {
		if l.ByID[id] != nil {
			return nil, fmt.Errorf("%s.id %q is used twice", path, id)
		}
		n.ID = id
		l.ByID[id] = n
	}
	if isContainer(typ) {
		kids, ok := m["children"].([]any)
		if !ok {
			return nil, fmt.Errorf("%s.children must be an array", path)
		}
		if len(kids) == 0 || len(kids) > MaxChildren {
			return nil, fmt.Errorf("%s.children must have 1-%d widgets", path, MaxChildren)
		}
		for i, k := range kids {
			c, err := l.node(k, fmt.Sprintf("%s.children[%d]", path, i), depth+1)
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, c)
		}
	}
	return n, nil
}

func isContainer(typ string) bool {
	return typ == WidgetColumn || typ == WidgetRow || typ == WidgetGrid
}

// Interactive widgets produce events and need an id.
func interactive(typ string) bool {
	switch typ {
	case WidgetButton, WidgetToggle, WidgetSlider, WidgetTextInput, WidgetCanvas:
		return true
	}
	return false
}

// CheckWidget checks the members of one widget of type typ (not its
// children). props are decoded with json.Number numbers. Unknown members
// are ignored. The error starts with the member's name.
func CheckWidget(typ string, props map[string]any) error {
	switch typ {
	case WidgetColumn, WidgetRow, WidgetGrid, WidgetText, WidgetButton, WidgetToggle,
		WidgetSlider, WidgetProgress, WidgetTextInput, WidgetCanvas:
	default:
		return fmt.Errorf("type %q is not a widget type", typ)
	}
	c := checker{props: props}
	if id, ok := props["id"]; ok {
		s, isStr := id.(string)
		if !isStr {
			return errors.New("id must be a string")
		}
		if err := CheckID(s); err != nil {
			return fmt.Errorf("id: %w", err)
		}
	} else if interactive(typ) {
		return fmt.Errorf("id is required for a %s", typ)
	}
	if w, ok := c.number("weight"); ok && !(w > 0) {
		c.fail("weight must be > 0")
	}
	c.boolean("enabled")
	c.enum("tone", ToneDefault, ToneAccent, ToneGood, ToneWarn, ToneBad)

	switch typ {
	case WidgetGrid:
		if _, ok := props["cols"]; !ok {
			c.fail("cols is required")
		} else if n, ok := c.integer("cols"); ok && (n < 1 || n > 8) {
			c.fail("cols must be 1-8")
		}
	case WidgetText:
		c.text("text", 0, 4000, true, true)
		c.enum("size", "small", "body", "title", "display")
		c.enum("align", "start", "center", "end")
	case WidgetButton:
		c.text("label", 1, 100, false, true)
	case WidgetToggle:
		c.text("label", 1, 100, false, true)
		c.boolean("value")
	case WidgetSlider:
		c.text("label", 0, 100, false, false)
		lo, hasLo := c.number("min")
		hi, hasHi := c.number("max")
		if !hasLo {
			lo = 0
		}
		if !hasHi {
			hi = 1
		}
		if c.err == nil && !(lo < hi) {
			c.fail("min must be less than max")
		}
		if s, ok := c.number("step"); ok && !(s > 0) {
			c.fail("step must be > 0")
		}
		c.number("value")
	case WidgetProgress:
		c.text("label", 0, 100, false, false)
		if v, ok := props["value"]; ok && v != nil {
			if f, ok := c.number("value"); ok && (f < 0 || f > 1) {
				c.fail("value must be in [0, 1] or null")
			}
		}
	case WidgetTextInput:
		c.text("label", 0, 100, false, false)
		c.text("placeholder", 0, 100, false, false)
		maxLen := int64(1000)
		if n, ok := c.integer("max_length"); ok {
			if n < 1 || n > 4000 {
				c.fail("max_length must be 1-4000")
			}
			maxLen = n
		}
		if c.err == nil {
			c.text("value", 0, int(maxLen), true, false)
		}
		c.boolean("multiline")
		c.boolean("clear_on_submit")
	case WidgetCanvas:
		c.boolean("ink")
		c.color("color")
		c.color("background")
		if w, ok := c.number("width"); ok && (w < 0.5 || w > 64) {
			c.fail("width must be 0.5-64")
		}
	}
	return c.err
}

// checker checks members of a decoded object, keeping the first error.
type checker struct {
	props map[string]any
	err   error
}

func (c *checker) fail(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

func (c *checker) number(k string) (float64, bool) {
	v, ok := c.props[k]
	if !ok {
		return 0, false
	}
	n, isNum := v.(json.Number)
	f, err := n.Float64()
	if !isNum || err != nil || math.IsInf(f, 0) {
		c.fail("%s must be a number", k)
		return 0, false
	}
	return f, true
}

func (c *checker) integer(k string) (int64, bool) {
	v, ok := c.props[k]
	if !ok {
		return 0, false
	}
	n, isNum := v.(json.Number)
	i, err := strconv.ParseInt(string(n), 10, 64)
	if !isNum || err != nil {
		c.fail("%s must be an integer", k)
		return 0, false
	}
	return i, true
}

func (c *checker) boolean(k string) {
	if v, ok := c.props[k]; ok {
		if _, isBool := v.(bool); !isBool {
			c.fail("%s must be true or false", k)
		}
	}
}

func (c *checker) enum(k string, values ...string) {
	if v, ok := c.props[k]; ok {
		if s, isStr := v.(string); !isStr || !slices.Contains(values, s) {
			c.fail("%s must be one of %q", k, values)
		}
	}
}

func (c *checker) text(k string, minLen, maxLen int, newlines, required bool) {
	v, ok := c.props[k]
	if !ok {
		if required {
			c.fail("%s is required", k)
		}
		return
	}
	s, isStr := v.(string)
	if !isStr {
		c.fail("%s must be a string", k)
		return
	}
	if err := CheckText(s, minLen, maxLen, newlines); err != nil {
		c.fail("%s %v", k, err)
	}
}

func (c *checker) color(k string) {
	if v, ok := c.props[k]; ok {
		s, _ := v.(string)
		if err := CheckColor(s); err != nil {
			c.fail("%s: %v", k, err)
		}
	}
}

// WidgetValue returns the current value of a toggle (bool) or slider
// (number, clamped to [min, max]) as panel_attach reports it, and false for
// other widgets.
func WidgetValue(n *Node) (any, bool) {
	num := func(k string, def float64) float64 {
		if x, ok := n.Props[k].(json.Number); ok {
			if f, err := x.Float64(); err == nil {
				return f
			}
		}
		return def
	}
	switch n.Type {
	case WidgetToggle:
		b, _ := n.Props["value"].(bool)
		return b, true
	case WidgetSlider:
		lo, hi := num("min", 0), num("max", 1)
		return min(max(num("value", lo), lo), hi), true
	}
	return nil, false
}
