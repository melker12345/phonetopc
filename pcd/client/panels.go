package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Notifications and panels (spec/PANELS.md): show things on the phone and
// get the user's input back. Input arrives as events of the sources
// "notification" and "panel"; subscribe to them before showing anything:
//
//	sub, _ := c.Subscribe(ctx, client.SourcePanel, client.SubscribeOptions{})
//	_, err := c.ShowPanel(ctx, "deck", "Stream deck", client.Grid(2,
//		client.Button("mute", "🔇 Mute"),
//		client.Toggle("mic", "Mic", true),
//	), client.PanelOptions{KeepAwake: true})
//	for ev := range sub.Events() {
//		p, _ := ev.Panel()
//		if ev.Type == client.TypePress && p.Widget == "mute" { ... }
//	}
//
// The user allows notifications and panels per PC on the phone; until then
// the calls fail with an error that IsNotAllowed recognizes.

// Re-exported types of PANELS.md.
type (
	Widget            = protocol.Widget
	WidgetUpdate      = protocol.WidgetUpdate
	NotifyAction      = protocol.NotifyAction
	PanelInfo         = protocol.PanelInfo
	PanelShowResult   = protocol.PanelShowResult
	PanelAttachResult = protocol.PanelAttachResult
	PanelEvent        = protocol.PanelEvent
	NotificationEvent = protocol.NotificationEvent
	Point             = protocol.Point
)

// Sources and event types of PANELS.md §2.
const (
	SourceNotification = protocol.SourceNotification
	SourcePanel        = protocol.SourcePanel

	TypeAction  = protocol.TypeAction  // notification: an action button
	TypeTap     = protocol.TypeTap     // notification: tapped
	TypeDismiss = protocol.TypeDismiss // notification: swiped away
	TypeState   = protocol.TypeState   // panel: visible, hidden, closed
	TypePress   = protocol.TypePress   // panel: button
	TypeChange  = protocol.TypeChange  // panel: toggle, slider
	TypeSubmit  = protocol.TypeSubmit  // panel: text field
	TypeStroke  = protocol.TypeStroke  // panel: canvas
)

// Panel error codes (PANELS.md §9).
const (
	CodeNotAllowed    = protocol.CodeNotAllowed
	CodeUnknownPanel  = protocol.CodeUnknownPanel
	CodeTooManyPanels = protocol.CodeTooManyPanels
	CodeRateLimited   = protocol.CodeRateLimited
)

// IsNotAllowed reports whether err is -32030: the phone's user hasn't
// allowed notifications or panels for this PC. They turn them on in the
// phone app.
func IsNotAllowed(err error) bool {
	code, ok := protocol.ErrorCodeOf(err)
	return ok && code == protocol.CodeNotAllowed
}

// NotifyOptions are the optional parts of a notification.
type NotifyOptions struct {
	// Body is shown under the title; newlines are allowed.
	Body string
	// ID names the notification; posting again with the same ID replaces
	// it. Empty: the phone picks one.
	ID string
	// Actions are up to 3 buttons. Pressing one sends a notification event
	// of type action.
	Actions []NotifyAction
	// High asks for a heads-up notification.
	High bool
}

// Notify posts a notification on the phone and returns its id (PANELS.md
// §3). The params are checked before sending.
func (c *Client) Notify(ctx context.Context, title string, opts NotifyOptions) (string, error) {
	p := protocol.NotifyParams{Title: title, Body: opts.Body, ID: opts.ID, Actions: opts.Actions}
	if opts.High {
		p.Priority = protocol.PriorityHigh
	}
	if err := protocol.CheckNotify(p); err != nil {
		return "", fmt.Errorf("phonetopc: notify: %w", err)
	}
	var res protocol.NotifyResult
	err := c.Call(ctx, protocol.MethodNotify, p, &res)
	return res.ID, err
}

// CancelNotification removes a notification this PC posted. It is not an
// error if it's gone already.
func (c *Client) CancelNotification(ctx context.Context, id string) error {
	return c.Call(ctx, protocol.MethodNotifyCancel, protocol.NotifyCancelParams{ID: id}, nil)
}

// PanelOptions are the optional parts of ShowPanel.
type PanelOptions struct {
	// KeepAwake keeps the phone's screen on while the panel is visible.
	KeepAwake bool
	// Orientation is "any" (default), "portrait" or "landscape".
	Orientation string
}

// ShowPanel shows a panel (PANELS.md §4.3), or replaces this PC's panel
// with the same id, and makes this connection its owner: the panel's
// events come here. Result.Visible says whether it's on screen already;
// otherwise the phone asks its user to open it. The layout is checked
// before sending.
func (c *Client) ShowPanel(ctx context.Context, id, title string, layout Widget, opts PanelOptions) (PanelShowResult, error) {
	b, err := json.Marshal(layout)
	if err != nil {
		return PanelShowResult{}, err
	}
	if _, err := protocol.ParseLayout(b); err != nil {
		return PanelShowResult{}, fmt.Errorf("phonetopc: panel %q: %w", id, err)
	}
	p := protocol.PanelShowParams{Panel: id, Title: title, Layout: &layout, KeepAwake: opts.KeepAwake, Orientation: opts.Orientation}
	var res PanelShowResult
	err = c.Call(ctx, protocol.MethodPanelShow, p, &res)
	return res, err
}

// AttachPanel makes this connection the owner of an existing panel, for
// example after a reconnect (PANELS.md §4.4). The phone then replays the
// canvas strokes with pseq > afterPseq (-1: all it keeps) as events with
// Replayed set.
func (c *Client) AttachPanel(ctx context.Context, id string, afterPseq int64) (PanelAttachResult, error) {
	var res PanelAttachResult
	err := c.Call(ctx, protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: id, AfterPseq: &afterPseq}, &res)
	return res, err
}

// UpdatePanel changes widget properties in place, all or nothing (PANELS.md
// §4.5):
//
//	c.UpdatePanel(ctx, "status", client.Set("msg", "text", "done", "tone", "good"))
func (c *Client) UpdatePanel(ctx context.Context, id string, updates ...WidgetUpdate) error {
	return c.Call(ctx, protocol.MethodPanelUpdate, protocol.PanelUpdateParams{Panel: id, Updates: updates}, nil)
}

// Set builds a WidgetUpdate from property name, value pairs.
func Set(widget string, kv ...any) WidgetUpdate {
	u := WidgetUpdate{Widget: widget, Set: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		u.Set[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return u
}

// ClosePanel removes a panel.
func (c *Client) ClosePanel(ctx context.Context, id string) error {
	return c.Call(ctx, protocol.MethodPanelClose, protocol.PanelRef{Panel: id}, nil)
}

// ListPanels lists this PC's panels.
func (c *Client) ListPanels(ctx context.Context) ([]PanelInfo, error) {
	var res protocol.PanelListResult
	err := c.Call(ctx, protocol.MethodPanelList, nil, &res)
	return res.Panels, err
}

// ClearCanvas erases a canvas and the strokes the phone keeps for it.
func (c *Client) ClearCanvas(ctx context.Context, panel, canvas string) error {
	return c.Call(ctx, protocol.MethodCanvasClear, protocol.CanvasClearParams{Panel: panel, Widget: canvas}, nil)
}

// Layout builders. Set other members on the returned Widget, e.g.
// w := client.Button("mute", "Mute"); w.Tone = "bad".

// Column stacks widgets top to bottom.
func Column(children ...Widget) Widget {
	return Widget{Type: protocol.WidgetColumn, Kids: children}
}

// Row puts widgets side by side.
func Row(children ...Widget) Widget { return Widget{Type: protocol.WidgetRow, Kids: children} }

// Grid lays widgets out in cols equal columns, filling its space.
func Grid(cols int, children ...Widget) Widget {
	return Widget{Type: protocol.WidgetGrid, Cols: cols, Kids: children}
}

// Text shows text. Give it an id to update it later.
func Text(id, text string) Widget { return Widget{Type: protocol.WidgetText, ID: id, Text: text} }

// Button sends a press event when pressed.
func Button(id, label string) Widget {
	return Widget{Type: protocol.WidgetButton, ID: id, Label: &label}
}

// Toggle sends a change event with its new value when flipped.
func Toggle(id, label string, on bool) Widget {
	return Widget{Type: protocol.WidgetToggle, ID: id, Label: &label, Value: on}
}

// Slider sends change events while dragged, the last with Final set.
func Slider(id, label string, lo, hi, value float64) Widget {
	return Widget{Type: protocol.WidgetSlider, ID: id, Label: &label, Min: &lo, Max: &hi, Value: value}
}

// Progress shows a progress bar; a value outside [0, 1] makes it
// indeterminate.
func Progress(id, label string, value float64) Widget {
	w := Widget{Type: protocol.WidgetProgress, ID: id, Label: &label}
	if value >= 0 && value <= 1 {
		w.Value = value
	}
	return w
}

// TextInput sends a submit event with the text when the user sends it.
func TextInput(id, label string) Widget {
	return Widget{Type: protocol.WidgetTextInput, ID: id, Label: &label}
}

// Canvas is a drawing area that sends stroke events; with ink off the
// phone draws nothing and it's a touchpad.
func Canvas(id string, ink bool) Widget {
	return Widget{Type: protocol.WidgetCanvas, ID: id, Ink: &ink}
}
