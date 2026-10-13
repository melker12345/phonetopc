package mockphone

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Notifications and panels (spec/PANELS.md). They belong to the PC, not to
// a connection: in secure mode the PC is hello.pc.id, in dev mode every
// session is the one dev-mode PC. The phone's screen shows at most one
// panel; something has to look at it (the simulated user of user.go, or a
// test through User) for a panel to become visible.

// devPC is the key and name of the dev-mode PC (§1).
const (
	devPCKey  = "dev"
	devPCName = "Unverified PC (dev mode)"
)

// panelHub holds every PC's panels and notifications. Lock order: hub.mu,
// then Server.mu, then session.mu, then subscription.mu.
type panelHub struct {
	srv *Server

	mu          sync.Mutex
	pcs         map[string]*pcState
	allowNotify bool
	allowPanels bool
	screen      *panel   // the panel on the phone's screen, nil if none
	recent      []*panel // panels by the time they were last shown, newest last
}

// pcState is one PC's panels and notifications.
type pcState struct {
	key, name string
	panels    []*panel // creation order
	notifs    []*notif // posting order, oldest first
	nextNotif int
	posted    []time.Time // for the rate limit
}

type notif struct {
	id     string
	params protocol.NotifyParams
	posted time.Time
}

// panel is one panel. Its fields are guarded by panelHub.mu.
type panel struct {
	pc        *pcState
	id, title string
	raw       json.RawMessage // the layout as received
	layout    *protocol.Layout
	keepAwake bool
	orient    string

	owner    *session // nil while detached
	expiry   *time.Timer
	nextPseq int64
	canvases map[string]*canvas
	cursor   int // the simulated user's next widget
}

// canvas holds the stroke events of a canvas since its last clear (§7.3).
type canvas struct {
	kept       []protocol.PanelEvent
	points     int
	nextStroke int64
}

func newPanelHub(srv *Server) *panelHub {
	return &panelHub{
		srv:         srv,
		pcs:         map[string]*pcState{},
		allowNotify: !srv.opts.NoNotify,
		allowPanels: !srv.opts.NoPanels,
	}
}

// pcKey returns the key and name of the PC a session belongs to.
func (s *session) pcKey() (key, name string) {
	pc, _, ok := s.auth()
	if !ok {
		return devPCKey, devPCName
	}
	return "pc:" + pc.ID, pc.Name
}

// pc returns the state of the PC key, creating it. h.mu must be held.
func (h *panelHub) pc(key, name string) *pcState {
	pc := h.pcs[key]
	if pc == nil {
		pc = &pcState{key: key, name: name}
		h.pcs[key] = pc
	}
	return pc
}

func (h *panelHub) allowed(perm string) *protocol.Error {
	ok := h.allowPanels
	what := "panels"
	if perm == protocol.PermissionNotify {
		ok, what = h.allowNotify, "notifications"
	}
	if ok {
		return nil
	}
	e := protocol.NewError(protocol.CodeNotAllowed, "the phone's user hasn't allowed %s for this PC", what)
	e.Data.Permission = perm
	return e
}

func (pc *pcState) find(id string) (int, *panel) {
	i := slices.IndexFunc(pc.panels, func(p *panel) bool { return p.id == id })
	if i < 0 {
		return -1, nil
	}
	return i, pc.panels[i]
}

func unknownPanel(id string) *protocol.Error {
	return protocol.NewError(protocol.CodeUnknownPanel, "this PC has no panel %q (never shown, closed or expired)", id)
}

func invalid(format string, args ...any) *protocol.Error {
	return protocol.NewError(protocol.CodeInvalidParams, format, args...)
}

// -- events

// eventReading builds a reading of an event source with type typ.
func (s *Server) eventReading(source, typ string, value any) protocol.Reading {
	r, err := s.reading(source, value, time.Now())
	if err != nil {
		s.logf("mock: %v", err)
	}
	r.Type = typ
	return r
}

// deliver queues r on every subscription of the session to r's source.
func (s *session) deliver(r protocol.Reading) {
	s.mu.Lock()
	subs := slices.Collect(maps.Values(s.subs))
	s.mu.Unlock()
	for _, sub := range subs {
		if sub.info.Source == r.Source {
			sub.emit(r)
		}
	}
}

// emit numbers a panel event, keeps it if it's a stroke and sends it to the
// owner. h.mu must be held.
func (h *panelHub) emit(p *panel, typ string, ev protocol.PanelEvent) {
	ev.Panel = p.id
	ev.Pseq = p.nextPseq
	p.nextPseq++
	if typ == protocol.TypeStroke {
		c := p.canvases[ev.Widget]
		c.kept = append(c.kept, ev)
		c.points += len(ev.Points)
	}
	if p.owner != nil {
		p.owner.deliver(h.srv.eventReading(protocol.SourcePanel, typ, ev))
	}
}

// setScreen puts p (nil: nothing) on the screen, with hidden and visible
// events. h.mu must be held.
func (h *panelHub) setScreen(p *panel) {
	if h.screen == p {
		return
	}
	if old := h.screen; old != nil && h.live(old) {
		h.emit(old, protocol.TypeState, protocol.PanelEvent{State: protocol.StateHidden})
	}
	h.screen = p
	if p != nil {
		h.recent = append(slices.DeleteFunc(h.recent, func(x *panel) bool { return x == p }), p)
		h.emit(p, protocol.TypeState, protocol.PanelEvent{State: protocol.StateVisible})
	}
}

// live reports whether p is still one of its PC's panels. h.mu must be held.
func (h *panelHub) live(p *panel) bool {
	_, q := p.pc.find(p.id)
	return q == p
}

// remove drops p. h.mu must be held.
func (h *panelHub) remove(p *panel) {
	if i, q := p.pc.find(p.id); q == p {
		p.pc.panels = slices.Delete(p.pc.panels, i, i+1)
	}
	if p.expiry != nil {
		p.expiry.Stop()
	}
	h.recent = slices.DeleteFunc(h.recent, func(x *panel) bool { return x == p })
	if h.screen == p {
		h.screen = nil
	}
}

// sessionClosed detaches the panels the session owned (§4.1).
func (h *panelHub) sessionClosed(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, pc := range h.pcs {
		for _, p := range pc.panels {
			if p.owner == s {
				p.owner = nil
				h.startExpiry(p)
			}
		}
	}
}

// startExpiry removes p after Options.PanelExpiry unless someone attaches
// it first. h.mu must be held.
func (h *panelHub) startExpiry(p *panel) {
	if p.expiry != nil {
		p.expiry.Stop()
	}
	p.expiry = time.AfterFunc(h.srv.opts.PanelExpiry, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if p.owner == nil && h.live(p) {
			h.srv.logf("mock: panel %q of %s expired (detached for %v)", p.id, p.pc.name, h.srv.opts.PanelExpiry)
			h.remove(p)
		}
	})
}

// own makes s the owner of p, if p is still live. h.mu must be held.
func (h *panelHub) own(p *panel, s *session) bool {
	if !h.live(p) {
		return false
	}
	if p.expiry != nil {
		p.expiry.Stop()
		p.expiry = nil
	}
	p.owner = s
	return true
}

// -- permissions

// AllowNotify turns notifications on or off for every PC. Turning them off
// removes every PC's notifications (§1).
func (s *Server) AllowNotify(on bool) {
	h := s.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	h.allowNotify = on
	if !on {
		for _, pc := range h.pcs {
			pc.notifs = nil
		}
	}
}

// AllowPanels turns panels on or off for every PC. Turning them off closes
// every panel, with a state closed event (reason revoked) to each owner (§1).
func (s *Server) AllowPanels(on bool) {
	h := s.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	h.allowPanels = on
	if on {
		return
	}
	for _, pc := range h.pcs {
		for _, p := range slices.Clone(pc.panels) {
			h.emit(p, protocol.TypeState, protocol.PanelEvent{State: protocol.StateClosed, Reason: protocol.ReasonRevoked})
			h.remove(p)
		}
	}
}

// -- notify (§3)

func (s *session) notify(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.NotifyParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := protocol.CheckNotify(p); err != nil {
		return nil, invalid("params.%v", err)
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionNotify); err != nil {
		return nil, err
	}
	pc := h.pc(s.pcKey())
	now := time.Now()
	if limit := s.srv.opts.NotifyPerMinute; limit > 0 {
		pc.posted = slices.DeleteFunc(pc.posted, func(t time.Time) bool { return now.Sub(t) > time.Minute })
		if len(pc.posted) >= limit {
			return nil, protocol.NewError(protocol.CodeRateLimited, "more than %d notifications in a minute", limit)
		}
		pc.posted = append(pc.posted, now)
	}
	id := p.ID
	for id == "" || (p.ID == "" && slices.ContainsFunc(pc.notifs, func(n *notif) bool { return n.id == id })) {
		pc.nextNotif++
		id = "n-" + strconv.Itoa(pc.nextNotif)
	}
	pc.notifs = slices.DeleteFunc(pc.notifs, func(n *notif) bool { return n.id == id })
	pc.notifs = append(pc.notifs, &notif{id: id, params: p, posted: now})
	if len(pc.notifs) > protocol.MaxNotifications {
		pc.notifs = pc.notifs[len(pc.notifs)-protocol.MaxNotifications:]
	}
	s.srv.logf("mock: notification %q from %s: %q %q", id, pc.name, p.Title, p.Body)
	return protocol.NotifyResult{ID: id}, nil
}

func (s *session) notifyCancel(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.NotifyCancelParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := protocol.CheckID(p.ID); err != nil {
		return nil, invalid("params.id: %v", err)
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionNotify); err != nil {
		return nil, err
	}
	pc := h.pc(s.pcKey())
	pc.notifs = slices.DeleteFunc(pc.notifs, func(n *notif) bool { return n.id == p.ID })
	return protocol.EmptyResult{}, nil
}

// notifyEvent sends a notification event to every session of the PC.
// h.mu must be held.
func (h *panelHub) notifyEvent(pc *pcState, typ string, ev protocol.NotificationEvent) {
	r := h.srv.eventReading(protocol.SourceNotification, typ, ev)
	h.srv.mu.Lock()
	sessions := slices.Collect(maps.Keys(h.srv.sessions))
	h.srv.mu.Unlock()
	for _, sess := range sessions {
		if sess.helloedPC() == pc.key {
			sess.deliver(r)
		}
	}
}

// helloedPC returns the PC key of a session after hello, or "".
func (s *session) helloedPC() string {
	select {
	case <-s.helloDone:
		key, _ := s.pcKey()
		return key
	default:
		return ""
	}
}

// -- panels (§4)

func (s *session) panelShow(params json.RawMessage) (any, func(), *protocol.Error) {
	var p struct {
		Panel       string          `json:"panel"`
		Title       *string         `json:"title"`
		Layout      json.RawMessage `json:"layout"`
		KeepAwake   bool            `json:"keep_awake"`
		Orientation *string         `json:"orientation"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if err := protocol.CheckID(p.Panel); err != nil {
		return nil, nil, invalid("params.panel: %v", err)
	}
	if p.Title == nil {
		return nil, nil, invalid("params.title is required")
	}
	if err := protocol.CheckText(*p.Title, 1, 60, false); err != nil {
		return nil, nil, invalid("params.title %v", err)
	}
	layout, err := protocol.ParseLayout(p.Layout)
	if err != nil {
		return nil, nil, invalid("params.%v", err)
	}
	orient := protocol.OrientationAny
	if p.Orientation != nil {
		orient = *p.Orientation
		if !slices.Contains([]string{protocol.OrientationAny, protocol.OrientationPortrait, protocol.OrientationLandscape}, orient) {
			return nil, nil, invalid("params.orientation must be any, portrait or landscape")
		}
	}

	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, nil, err
	}
	pc := h.pc(s.pcKey())
	i, old := pc.find(p.Panel)
	if old == nil && len(pc.panels) >= protocol.MaxPanels {
		return nil, nil, protocol.NewError(protocol.CodeTooManyPanels, "this PC already has %d panels; close one first", protocol.MaxPanels)
	}
	np := &panel{
		pc: pc, id: p.Panel, title: *p.Title, raw: p.Layout, layout: layout,
		keepAwake: p.KeepAwake, orient: orient, canvases: map[string]*canvas{},
	}
	for id, n := range layout.ByID {
		if n.Type == protocol.WidgetCanvas {
			np.canvases[id] = &canvas{}
		}
	}
	// The phone switches to the new panel only if it shows a panel of the
	// same PC (§4.3); otherwise the user opens it from a notification.
	toScreen := h.screen != nil && h.screen.pc == pc
	if old != nil {
		if old.expiry != nil {
			old.expiry.Stop()
		}
		pc.panels[i] = np
		h.recent = slices.DeleteFunc(h.recent, func(x *panel) bool { return x == old })
		if h.screen == old {
			h.screen = nil // replaced in place: no hidden event for the old one
		}
	} else {
		pc.panels = append(pc.panels, np)
	}
	if !toScreen {
		s.srv.logf("mock: notification: %s wants to show %q", pc.name, np.title)
	}
	s.srv.logf("mock: %s shows panel %q %q (%d widgets)", pc.name, np.id, np.title, layout.Count)
	res := protocol.PanelShowResult{Panel: np.id, Visible: toScreen}
	// Ownership (and with it the events) starts after the response, so the
	// response precedes every event of the new panel.
	after := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.own(np, s) && toScreen && (h.screen == nil || h.screen.pc == pc) {
			h.setScreen(np)
		}
	}
	return res, after, nil
}

func (s *session) panelAttach(params json.RawMessage) (any, func(), *protocol.Error) {
	var p protocol.PanelAttachParams
	if err := decodeParams(params, &p); err != nil {
		return nil, nil, err
	}
	if err := protocol.CheckID(p.Panel); err != nil {
		return nil, nil, invalid("params.panel: %v", err)
	}
	after := int64(-1)
	if p.AfterPseq != nil {
		if after = *p.AfterPseq; after < -1 {
			return nil, nil, invalid("params.after_pseq must be >= -1")
		}
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, nil, err
	}
	_, pn := h.pc(s.pcKey()).find(p.Panel)
	if pn == nil {
		return nil, nil, unknownPanel(p.Panel)
	}
	values := map[string]any{}
	for id, n := range pn.layout.ByID {
		if v, ok := protocol.WidgetValue(n); ok {
			values[id] = v
		}
	}
	res := protocol.PanelAttachResult{
		Panel: pn.id, Title: pn.title, Visible: h.screen == pn, LastPseq: pn.nextPseq - 1, Values: values,
	}
	replay := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.own(pn, s) {
			return
		}
		// Replay in pseq order: merge the canvases' kept events.
		var evs []protocol.PanelEvent
		for _, c := range pn.canvases {
			for _, ev := range c.kept {
				if ev.Pseq > after {
					evs = append(evs, ev)
				}
			}
		}
		slices.SortFunc(evs, func(a, b protocol.PanelEvent) int { return int(a.Pseq - b.Pseq) })
		for _, ev := range evs {
			ev.Replayed = true
			s.deliver(s.srv.eventReading(protocol.SourcePanel, protocol.TypeStroke, ev))
		}
		if len(evs) > 0 {
			s.srv.logf("mock: replayed %d stroke event(s) of panel %q", len(evs), pn.id)
		}
	}
	return res, replay, nil
}

func (s *session) panelUpdate(params json.RawMessage) (any, *protocol.Error) {
	var p struct {
		Panel   string `json:"panel"`
		Updates []struct {
			Widget string          `json:"widget"`
			Set    json.RawMessage `json:"set"`
		} `json:"updates"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := protocol.CheckID(p.Panel); err != nil {
		return nil, invalid("params.panel: %v", err)
	}
	if len(p.Updates) == 0 || len(p.Updates) > protocol.MaxPanelUpdates {
		return nil, invalid("params.updates must have 1-%d items", protocol.MaxPanelUpdates)
	}
	sets := make([]map[string]any, len(p.Updates))
	for i, u := range p.Updates {
		if err := protocol.CheckID(u.Widget); err != nil {
			return nil, invalid("params.updates[%d].widget: %v", i, err)
		}
		set, err := protocol.DecodeProps(u.Set)
		if err != nil {
			return nil, invalid("params.updates[%d].set %v", i, err)
		}
		for _, k := range []string{"type", "id", "children"} {
			if _, ok := set[k]; ok {
				return nil, invalid("params.updates[%d].set: %s can't be changed; call panel_show to change the structure", i, k)
			}
		}
		sets[i] = set
	}

	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, err
	}
	_, pn := h.pc(s.pcKey()).find(p.Panel)
	if pn == nil {
		return nil, unknownPanel(p.Panel)
	}
	// Atomic: check everything on copies, then apply.
	merged := map[*protocol.Node]map[string]any{}
	for i, u := range p.Updates {
		n := pn.layout.ByID[u.Widget]
		if n == nil {
			return nil, invalid("params.updates[%d].widget: panel %q has no widget %q", i, pn.id, u.Widget)
		}
		m := merged[n]
		if m == nil {
			m = maps.Clone(n.Props)
			merged[n] = m
		}
		maps.Copy(m, sets[i])
		if err := protocol.CheckWidget(n.Type, m); err != nil {
			return nil, invalid("params.updates[%d].set.%v", i, err)
		}
	}
	for n, m := range merged {
		n.Props = m
	}
	return protocol.EmptyResult{}, nil
}

func (s *session) panelClose(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.PanelRef
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := protocol.CheckID(p.Panel); err != nil {
		return nil, invalid("params.panel: %v", err)
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, err
	}
	_, pn := h.pc(s.pcKey()).find(p.Panel)
	if pn == nil {
		return nil, unknownPanel(p.Panel)
	}
	h.remove(pn)
	s.srv.logf("mock: %s closed panel %q", pn.pc.name, pn.id)
	return protocol.EmptyResult{}, nil
}

func (s *session) panelList(params json.RawMessage) (any, *protocol.Error) {
	if err := decodeParams(params, &struct{}{}); err != nil {
		return nil, err
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, err
	}
	res := protocol.PanelListResult{Panels: []protocol.PanelInfo{}}
	for _, pn := range h.pc(s.pcKey()).panels {
		res.Panels = append(res.Panels, protocol.PanelInfo{Panel: pn.id, Title: pn.title, Attached: pn.owner != nil, Visible: h.screen == pn})
	}
	return res, nil
}

func (s *session) canvasClear(params json.RawMessage) (any, *protocol.Error) {
	var p protocol.CanvasClearParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := protocol.CheckID(p.Panel); err != nil {
		return nil, invalid("params.panel: %v", err)
	}
	if err := protocol.CheckID(p.Widget); err != nil {
		return nil, invalid("params.widget: %v", err)
	}
	h := s.srv.panels
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(protocol.PermissionPanels); err != nil {
		return nil, err
	}
	_, pn := h.pc(s.pcKey()).find(p.Panel)
	if pn == nil {
		return nil, unknownPanel(p.Panel)
	}
	c := pn.canvases[p.Widget]
	if c == nil {
		return nil, invalid("params.widget: panel %q has no canvas %q", pn.id, p.Widget)
	}
	c.kept, c.points = nil, 0
	return protocol.EmptyResult{}, nil
}
