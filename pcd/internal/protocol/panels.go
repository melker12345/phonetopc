package protocol

import (
	"encoding/json"
)

// Notifications and panels (spec/PANELS.md): the PC shows things on the
// phone and gets the user's input back as events.

// Methods of PANELS.md.
const (
	MethodNotify       = "notify"        // §3.1
	MethodNotifyCancel = "notify_cancel" // §3.3
	MethodPanelShow    = "panel_show"    // §4.3
	MethodPanelAttach  = "panel_attach"  // §4.4
	MethodPanelUpdate  = "panel_update"  // §4.5
	MethodPanelClose   = "panel_close"   // §4.6
	MethodPanelList    = "panel_list"    // §4.6
	MethodCanvasClear  = "canvas_clear"  // §4.6
)

// Capabilities of PANELS.md §1. They say a method exists, not that this PC
// may use it.
const (
	CapabilityNotify = "notify"
	CapabilityPanels = "panels"
)

// Permissions, as in error.data.permission of not_allowed (§1).
const (
	PermissionNotify = "notify"
	PermissionPanels = "panels"
)

// Event sources of PANELS.md §2.
const (
	SourceNotification = "notification"
	SourcePanel        = "panel"
)

// Event types of the notification (§3.2) and panel (§6) sources. The panel
// source's "state" type is TypeState.
const (
	TypeAction  = "action"
	TypeTap     = "tap"
	TypeDismiss = "dismiss"
	TypePress   = "press"
	TypeChange  = "change"
	TypeSubmit  = "submit"
	TypeStroke  = "stroke"
)

// Limits of PANELS.md.
const (
	MaxNotifications     = 16 // per PC, §3.1
	MaxNotifyActions     = 3
	MaxPanels            = 8 // per PC, §4
	MaxPanelUpdates      = 64
	MaxWidgets           = 256 // per layout, §5
	MaxLayoutDepth       = 8
	MaxChildren          = 64
	MaxStrokePoints      = 128     // per stroke event, §7.1
	MinCanvasPoints      = 100_000 // a phone keeps at least this many per canvas, §7.3
	DetachedPanelTimeout = 10 * 60 // seconds a detached panel is kept, §4.1
)

// Priorities of a notification (§3.1).
const (
	PriorityDefault = "default"
	PriorityHigh    = "high"
)

// NotifyAction is one action button of a notification.
type NotifyAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// NotifyParams are the params of notify (§3.1).
type NotifyParams struct {
	Title    string         `json:"title"`
	Body     string         `json:"body,omitempty"`
	ID       string         `json:"id,omitempty"`
	Actions  []NotifyAction `json:"actions,omitempty"`
	Priority string         `json:"priority,omitempty"`
}

// NotifyResult is the result of notify.
type NotifyResult struct {
	ID string `json:"id"`
}

// NotifyCancelParams are the params of notify_cancel (§3.3).
type NotifyCancelParams struct {
	ID string `json:"id"`
}

// Orientations of a panel (§4.3).
const (
	OrientationAny       = "any"
	OrientationPortrait  = "portrait"
	OrientationLandscape = "landscape"
)

// PanelShowParams are the params of panel_show (§4.3).
type PanelShowParams struct {
	Panel       string  `json:"panel"`
	Title       string  `json:"title"`
	Layout      *Widget `json:"layout"`
	KeepAwake   bool    `json:"keep_awake,omitempty"`
	Orientation string  `json:"orientation,omitempty"`
}

// PanelShowResult is the result of panel_show.
type PanelShowResult struct {
	Panel   string `json:"panel"`
	Visible bool   `json:"visible"`
}

// PanelRef names a panel: the params of panel_close.
type PanelRef struct {
	Panel string `json:"panel"`
}

// PanelAttachParams are the params of panel_attach (§4.4). AfterPseq nil
// means -1: replay every kept stroke.
type PanelAttachParams struct {
	Panel     string `json:"panel"`
	AfterPseq *int64 `json:"after_pseq,omitempty"`
}

// PanelAttachResult is the result of panel_attach. Values holds the
// current value of every toggle (bool) and slider (number) by widget id.
type PanelAttachResult struct {
	Panel    string         `json:"panel"`
	Title    string         `json:"title"`
	Visible  bool           `json:"visible"`
	LastPseq int64          `json:"last_pseq"`
	Values   map[string]any `json:"values"`
}

// WidgetUpdate is one item of panel_update: properties to set on a widget.
type WidgetUpdate struct {
	Widget string         `json:"widget"`
	Set    map[string]any `json:"set"`
}

// PanelUpdateParams are the params of panel_update (§4.5).
type PanelUpdateParams struct {
	Panel   string         `json:"panel"`
	Updates []WidgetUpdate `json:"updates"`
}

// PanelInfo is one entry of panel_list.
type PanelInfo struct {
	Panel    string `json:"panel"`
	Title    string `json:"title"`
	Attached bool   `json:"attached"`
	Visible  bool   `json:"visible"`
}

// PanelListResult is the result of panel_list.
type PanelListResult struct {
	Panels []PanelInfo `json:"panels"`
}

// CanvasClearParams are the params of canvas_clear.
type CanvasClearParams struct {
	Panel  string `json:"panel"`
	Widget string `json:"widget"`
}

// EmptyResult is the result {} of notify_cancel, panel_update, panel_close
// and canvas_clear.
type EmptyResult struct{}

// Widget types (§5).
const (
	WidgetColumn    = "column"
	WidgetRow       = "row"
	WidgetGrid      = "grid"
	WidgetText      = "text"
	WidgetButton    = "button"
	WidgetToggle    = "toggle"
	WidgetSlider    = "slider"
	WidgetProgress  = "progress"
	WidgetTextInput = "text_input"
	WidgetCanvas    = "canvas"
)

// Tones (§5).
const (
	ToneDefault = "default"
	ToneAccent  = "accent"
	ToneGood    = "good"
	ToneWarn    = "warn"
	ToneBad     = "bad"
)

// Widget is one node of a panel layout (§5), for building layouts in Go.
// Which members apply depends on Type; unset members are omitted, so the
// phone's defaults apply. Value is a bool (toggle), a number (slider,
// progress) or a string (text_input).
type Widget struct {
	Type    string   `json:"type"`
	ID      string   `json:"id,omitempty"`
	Weight  float64  `json:"weight,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	Kids    []Widget `json:"children,omitempty"`

	Text  string `json:"text,omitempty"`
	Size  string `json:"size,omitempty"`
	Align string `json:"align,omitempty"`

	Label       *string  `json:"label,omitempty"`
	Value       any      `json:"value,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Step        float64  `json:"step,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Multiline   bool     `json:"multiline,omitempty"`
	MaxLength   int      `json:"max_length,omitempty"`
	// ClearOnSubmit is a pointer because the default is true.
	ClearOnSubmit *bool `json:"clear_on_submit,omitempty"`

	Ink        *bool   `json:"ink,omitempty"`
	Color      string  `json:"color,omitempty"`
	Width      float64 `json:"width,omitempty"`
	Background string  `json:"background,omitempty"`
}

// MarshalJSON writes a text widget's text even when empty: it is required.
func (w Widget) MarshalJSON() ([]byte, error) {
	type plain Widget
	b, err := json.Marshal(plain(w))
	if err != nil || w.Type != WidgetText || w.Text != "" {
		return b, err
	}
	return append(b[:len(b)-1], `,"text":""}`...), nil
}

// Panel states (§6.1) and the reasons of "closed".
const (
	StateVisible = "visible"
	StateHidden  = "hidden"
	StateClosed  = "closed"

	ReasonUser    = "user"
	ReasonRevoked = "revoked"
)

// Stroke phases and tools (§7.1).
const (
	PhaseStart  = "start"
	PhaseMove   = "move"
	PhaseEnd    = "end"
	PhaseCancel = "cancel"

	ToolFinger  = "finger"
	ToolStylus  = "stylus"
	ToolEraser  = "eraser"
	ToolMouse   = "mouse"
	ToolUnknown = "unknown"
)

// NotificationEvent is the value of a notification event (§3.2). Action is
// set for type "action".
type NotificationEvent struct {
	Notification string `json:"notification"`
	Action       string `json:"action,omitempty"`
}

// Point is one stroke point: x and y in [0, 1] relative to the canvas,
// pressure in [0, 1], and t, milliseconds since the stroke's first point.
type Point [4]float64

// X, Y, Pressure and T name the components.
func (p Point) X() float64        { return p[0] }
func (p Point) Y() float64        { return p[1] }
func (p Point) Pressure() float64 { return p[2] }
func (p Point) T() int64          { return int64(p[3]) }

// MarshalJSON writes t as an integer.
func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{p[0], p[1], p[2], int64(p[3])})
}

// PanelEvent is the value of a panel event (§6, §7). Which members are set
// depends on the event type:
//
//   - state: State, and Reason for "closed"
//   - press: Widget
//   - change: Widget, Value (bool or number), Final for sliders
//   - submit: Widget, Value (string)
//   - stroke: Widget, Stroke, Phase, Tool, Points, Size (start only),
//     Replayed when replayed by panel_attach
type PanelEvent struct {
	Panel    string          `json:"panel"`
	Pseq     int64           `json:"pseq"`
	Widget   string          `json:"widget,omitempty"`
	State    string          `json:"state,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Final    bool            `json:"final,omitempty"`
	Replayed bool            `json:"replayed,omitempty"`
	Stroke   *int64          `json:"stroke,omitempty"`
	Phase    string          `json:"phase,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Size     []float64       `json:"size,omitempty"`
	Points   []Point         `json:"points,omitempty"`
}

// BoolValue decodes Value of a toggle change.
func (e PanelEvent) BoolValue() (bool, error) {
	var b bool
	return b, json.Unmarshal(e.Value, &b)
}

// NumberValue decodes Value of a slider change.
func (e PanelEvent) NumberValue() (float64, error) {
	var f float64
	return f, json.Unmarshal(e.Value, &f)
}

// TextValue decodes Value of a submit.
func (e PanelEvent) TextValue() (string, error) {
	var s string
	return s, json.Unmarshal(e.Value, &s)
}

// Notification decodes a notification event's value.
func (r Reading) Notification() (NotificationEvent, error) {
	var n NotificationEvent
	return n, r.decode(&n)
}

// Panel decodes a panel event's value.
func (r Reading) Panel() (PanelEvent, error) {
	var p PanelEvent
	return p, r.decode(&p)
}
