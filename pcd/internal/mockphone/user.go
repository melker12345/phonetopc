package mockphone

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// User stands in for the phone's user on one PC's panels and notifications,
// as Tap and ScanTag do for the tile and NFC. Each method does what the user
// would do on the phone and sends the events a phone would send.
type User struct {
	srv *Server
	key string
}

// User returns the stand-in for the user of the PC with id pcID's panels
// and notifications; "" is the dev-mode PC.
func (s *Server) User(pcID string) *User {
	key := devPCKey
	if pcID != "" {
		key = "pc:" + pcID
	}
	return &User{srv: s, key: key}
}

// CanvasSize is the size in dp the mock reports for every canvas (§7.1).
var CanvasSize = []float64{360, 480}

// pcState is nil if the PC has no state yet. h.mu must be held.
func (u *User) pcState() *pcState { return u.srv.panels.pcs[u.key] }

func (u *User) Panels() []protocol.PanelInfo {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []protocol.PanelInfo
	if pc := u.pcState(); pc != nil {
		for _, p := range pc.panels {
			out = append(out, protocol.PanelInfo{Panel: p.id, Title: p.title, Attached: p.owner != nil, Visible: h.screen == p})
		}
	}
	return out
}

// Notifications are oldest first, with their ids.
func (u *User) Notifications() []protocol.NotifyParams {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []protocol.NotifyParams
	if pc := u.pcState(); pc != nil {
		for _, n := range pc.notifs {
			p := n.params
			p.ID = n.id
			out = append(out, p)
		}
	}
	return out
}

// h.mu must be held.
func (u *User) panel(id string) (*panel, error) {
	if pc := u.pcState(); pc != nil {
		if _, p := pc.find(id); p != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no panel %q", id)
}

// Look opens the panel: it comes on screen, with a visible event (and a
// hidden event for the panel shown before).
func (u *User) Look(panel string) error {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := u.panel(panel)
	if err != nil {
		return err
	}
	h.setScreen(p)
	return nil
}

// LookAway leaves the panel screen (switches app, or turns the screen off):
// the panel shown gets a hidden event.
func (u *User) LookAway() {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setScreen(nil)
}

// Close presses the close button in the panel's frame: the owner gets a
// state closed event (reason user) and the panel is gone.
func (u *User) Close(panel string) error {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := u.panel(panel)
	if err != nil {
		return err
	}
	h.emit(p, protocol.TypeState, protocol.PanelEvent{State: protocol.StateClosed, Reason: protocol.ReasonUser})
	h.remove(p)
	return nil
}

// widget finds an enabled widget of type typ and brings its panel on
// screen. Only canvases take input while the panel is detached (§4.1).
// h.mu must be held.
func (u *User) widget(panel, widget, typ string) (*panel, *protocol.Node, error) {
	p, err := u.panel(panel)
	if err != nil {
		return nil, nil, err
	}
	n := p.layout.ByID[widget]
	switch {
	case n == nil || n.Type != typ:
		return nil, nil, fmt.Errorf("panel %q has no %s %q", panel, typ, widget)
	case n.Props["enabled"] == false:
		return nil, nil, fmt.Errorf("%s %q is disabled", typ, widget)
	case p.owner == nil && typ != protocol.WidgetCanvas:
		return nil, nil, fmt.Errorf("panel %q is detached: its widgets are disabled", panel)
	}
	u.srv.panels.setScreen(p)
	return p, n, nil
}

func (u *User) Press(panel, button string) error {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, _, err := u.widget(panel, button, protocol.WidgetButton)
	if err != nil {
		return err
	}
	h.emit(p, protocol.TypePress, protocol.PanelEvent{Widget: button})
	return nil
}

func (u *User) Toggle(panel, toggle string) (bool, error) {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, n, err := u.widget(panel, toggle, protocol.WidgetToggle)
	if err != nil {
		return false, err
	}
	v := n.Props["value"] != true
	n.Props["value"] = v
	h.emit(p, protocol.TypeChange, protocol.PanelEvent{Widget: toggle, Value: mustJSON(v)})
	return v, nil
}

// Slide moves a slider to value (snapped to its step, within [min, max]),
// with final set when the finger lifts. It returns the value sent.
func (u *User) Slide(panel, slider string, value float64, final bool) (float64, error) {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, n, err := u.widget(panel, slider, protocol.WidgetSlider)
	if err != nil {
		return 0, err
	}
	lo, hi := propNumber(n, "min", 0), propNumber(n, "max", 1)
	if step := propNumber(n, "step", 0); step > 0 {
		value = lo + math.Round((value-lo)/step)*step
	}
	value = min(max(value, lo), hi)
	n.Props["value"] = json.Number(strconv.FormatFloat(value, 'f', -1, 64))
	h.emit(p, protocol.TypeChange, protocol.PanelEvent{Widget: slider, Value: mustJSON(value), Final: final})
	return value, nil
}

func (u *User) Submit(panel, field, text string) error {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	p, n, err := u.widget(panel, field, protocol.WidgetTextInput)
	if err != nil {
		return err
	}
	maxLen := int(propNumber(n, "max_length", 1000))
	if err := protocol.CheckText(text, 0, maxLen, n.Props["multiline"] == true); err != nil {
		return fmt.Errorf("text %w", err)
	}
	if n.Props["clear_on_submit"] == false {
		n.Props["value"] = text
	} else {
		n.Props["value"] = ""
	}
	h.emit(p, protocol.TypeSubmit, protocol.PanelEvent{Widget: field, Value: mustJSON(text)})
	return nil
}

// Stroke is a stroke for User.Draw.
type Stroke struct {
	// Tool defaults to finger.
	Tool string
	// Points are [x, y, pressure, t]; x and y are clamped to [0, 1].
	Points []protocol.Point
	// Chunk is the number of points per stroke event; default 4.
	Chunk int
	// Frame is the time between events; 0 sends them all at once.
	Frame time.Duration
	// Cancel ends the stroke with cancel instead of end.
	Cancel bool
}

// ErrCanvasFull is returned by Draw when the canvas keeps as many points as
// it can (§7.3).
var ErrCanvasFull = errors.New("canvas full")

// Draw draws a stroke on a canvas, sending it in chunks as a phone does
// (§7). It works on detached panels too: their canvases keep taking ink.
func (u *User) Draw(panel, canvasID string, st Stroke) error {
	if len(st.Points) == 0 {
		return errors.New("a stroke needs at least one point")
	}
	if st.Tool == "" {
		st.Tool = protocol.ToolFinger
	}
	chunk := st.Chunk
	if chunk <= 0 {
		chunk = 4
	}
	chunk = min(chunk, protocol.MaxStrokePoints)
	pts := make([]protocol.Point, len(st.Points))
	for i, pt := range st.Points {
		pts[i] = protocol.Point{round4(min(max(pt[0], 0), 1)), round4(min(max(pt[1], 0), 1)), min(max(pt[2], 0), 1), math.Max(0, math.Round(pt[3]))}
	}

	h := u.srv.panels
	h.mu.Lock()
	p, _, err := u.widget(panel, canvasID, protocol.WidgetCanvas)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	c := p.canvases[canvasID]
	if c.points+len(pts) > u.srv.opts.CanvasPoints {
		h.mu.Unlock()
		return ErrCanvasFull
	}
	id := c.nextStroke
	c.nextStroke++
	h.mu.Unlock()

	end := protocol.PhaseEnd
	if st.Cancel {
		end = protocol.PhaseCancel
	}
	for i := 0; i < len(pts) || i == 0; i += chunk {
		if i > 0 && st.Frame > 0 {
			time.Sleep(st.Frame)
		}
		part := pts[i:min(i+chunk, len(pts))]
		ev := protocol.PanelEvent{Widget: canvasID, Stroke: &id, Tool: st.Tool, Points: part, Phase: protocol.PhaseMove}
		switch {
		case i == 0:
			ev.Phase, ev.Size = protocol.PhaseStart, CanvasSize
		case i+chunk >= len(pts):
			ev.Phase = end
		}
		h.mu.Lock()
		if !h.live(p) || p.canvases[canvasID] != c {
			h.mu.Unlock()
			return fmt.Errorf("panel %q went away while drawing", panel)
		}
		h.emit(p, protocol.TypeStroke, ev)
		if i == 0 && len(pts) <= chunk { // a dot: start, then an empty end
			h.emit(p, protocol.TypeStroke, protocol.PanelEvent{Widget: canvasID, Stroke: &id, Tool: st.Tool, Phase: end})
		}
		h.mu.Unlock()
	}
	return nil
}

// Act presses a notification's action button.
func (u *User) Act(notification, action string) error {
	return u.onNotification(notification, func(n *notif) (string, protocol.NotificationEvent, error) {
		if !slices.ContainsFunc(n.params.Actions, func(a protocol.NotifyAction) bool { return a.ID == action }) {
			return "", protocol.NotificationEvent{}, fmt.Errorf("notification %q has no action %q", notification, action)
		}
		return protocol.TypeAction, protocol.NotificationEvent{Notification: n.id, Action: action}, nil
	})
}

// TapNotification taps a notification itself.
func (u *User) TapNotification(notification string) error {
	return u.onNotification(notification, func(n *notif) (string, protocol.NotificationEvent, error) {
		return protocol.TypeTap, protocol.NotificationEvent{Notification: n.id}, nil
	})
}

// Dismiss swipes a notification away.
func (u *User) Dismiss(notification string) error {
	return u.onNotification(notification, func(n *notif) (string, protocol.NotificationEvent, error) {
		return protocol.TypeDismiss, protocol.NotificationEvent{Notification: n.id}, nil
	})
}

// onNotification removes a notification and sends the event f returns.
func (u *User) onNotification(id string, f func(*notif) (string, protocol.NotificationEvent, error)) error {
	h := u.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	pc := u.pcState()
	var i int
	if pc != nil {
		i = slices.IndexFunc(pc.notifs, func(n *notif) bool { return n.id == id })
	}
	if pc == nil || i < 0 {
		return fmt.Errorf("no notification %q", id)
	}
	typ, ev, err := f(pc.notifs[i])
	if err != nil {
		return err
	}
	pc.notifs = slices.Delete(pc.notifs, i, i+1)
	h.notifyEvent(pc, typ, ev)
	return nil
}

func propNumber(n *protocol.Node, k string, def float64) float64 {
	if x, ok := n.Props[k].(json.Number); ok {
		if f, err := x.Float64(); err == nil {
			return f
		}
	}
	return def
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }
