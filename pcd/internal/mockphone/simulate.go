package mockphone

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// simulate plays a phone user who keeps an eye on the PCs' panels and
// notifications, so clients see events without a phone. Every tick it does
// one thing per PC's notifications and one on the screen:
//
//   - answers the oldest notification older than a tick: its first
//     action, or a tap when it has none;
//   - with no panel on screen, opens the newest panel (as if from the
//     "wants to show" notification);
//   - otherwise uses the next widget of the panel on screen, in layout
//     order: presses a button, flips a toggle, drags a slider, submits text
//     or draws a circle on a canvas, in per-frame chunks.
func (h *panelHub) simulate(ctx context.Context, every time.Duration) {
	rng := h.srv.newRand()
	t := time.NewTicker(every)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.answerNotifications(every)
			h.useScreen(ctx, rng, n)
		}
	}
}

func (h *panelHub) answerNotifications(age time.Duration) {
	type answer struct{ key, id, action string }
	var todo []answer
	h.mu.Lock()
	for _, key := range slices.Sorted(maps.Keys(h.pcs)) {
		pc := h.pcs[key]
		if len(pc.notifs) == 0 || time.Since(pc.notifs[0].posted) < age {
			continue
		}
		n := pc.notifs[0]
		a := answer{key: key, id: n.id}
		if len(n.params.Actions) > 0 {
			a.action = n.params.Actions[0].ID
		}
		todo = append(todo, a)
	}
	h.mu.Unlock()
	for _, a := range todo {
		u := &User{srv: h.srv, key: a.key}
		var err error
		if a.action != "" {
			err = u.Act(a.id, a.action)
			h.srv.logf("mock: user pressed %q on notification %q", a.action, a.id)
		} else {
			err = u.TapNotification(a.id)
			h.srv.logf("mock: user tapped notification %q", a.id)
		}
		if err != nil {
			h.srv.logf("mock: user: %v", err)
		}
	}
}

func (h *panelHub) useScreen(ctx context.Context, rng *rand.Rand, n int) {
	h.mu.Lock()
	p := h.screen
	if p == nil || !h.live(p) {
		h.screen = nil
		if next := h.newestPanel(); next != nil {
			h.srv.logf("mock: user opens panel %q of %s", next.id, next.pc.name)
			h.setScreen(next)
		}
		h.mu.Unlock()
		return
	}
	var widgets []*protocol.Node
	var walk func(*protocol.Node)
	walk = func(w *protocol.Node) {
		usable := w.Props["enabled"] != false && (p.owner != nil || w.Type == protocol.WidgetCanvas)
		if interactiveType(w.Type) && usable {
			widgets = append(widgets, w)
		}
		for _, c := range w.Children {
			walk(c)
		}
	}
	walk(p.layout.Root)
	if len(widgets) == 0 {
		h.mu.Unlock()
		return
	}
	w := widgets[p.cursor%len(widgets)]
	p.cursor++
	u := &User{srv: h.srv, key: p.pc.key}
	panelID := p.id
	h.mu.Unlock()

	var err error
	switch w.Type {
	case protocol.WidgetButton:
		err = u.Press(panelID, w.ID)
	case protocol.WidgetToggle:
		_, err = u.Toggle(panelID, w.ID)
	case protocol.WidgetSlider:
		lo, hi := propNumber(w, "min", 0), propNumber(w, "max", 1)
		target := lo + rng.Float64()*(hi-lo)
		for i := 1; i <= 3 && err == nil; i++ {
			_, err = u.Slide(panelID, w.ID, lo+(target-lo)*float64(i)/3, i == 3)
		}
	case protocol.WidgetTextInput:
		err = u.Submit(panelID, w.ID, fmt.Sprintf("hello from phonetopc-mock #%d", n))
	case protocol.WidgetCanvas:
		st := Stroke{Points: circle(rng), Chunk: 2, Frame: 32 * time.Millisecond}
		h.srv.wg.Go(func() {
			if err := u.Draw(panelID, w.ID, st); err != nil && ctx.Err() == nil {
				h.srv.logf("mock: user: %v", err)
			}
		})
	}
	if err != nil {
		h.srv.logf("mock: user: %v", err)
	} else {
		h.srv.logf("mock: user used %s %q on panel %q", w.Type, w.ID, panelID)
	}
}

// newestPanel returns the panel shown most recently, else the newest
// panel of any PC. h.mu must be held.
func (h *panelHub) newestPanel() *panel {
	for i := len(h.recent) - 1; i >= 0; i-- {
		if h.live(h.recent[i]) {
			return h.recent[i]
		}
	}
	var newest *panel
	for _, key := range slices.Sorted(maps.Keys(h.pcs)) {
		if ps := h.pcs[key].panels; len(ps) > 0 {
			newest = ps[len(ps)-1]
		}
	}
	return newest
}

func interactiveType(typ string) bool {
	switch typ {
	case protocol.WidgetButton, protocol.WidgetToggle, protocol.WidgetSlider, protocol.WidgetTextInput, protocol.WidgetCanvas:
		return true
	}
	return false
}

// circle returns a slightly wobbly hand-drawn circle of 48 points, one per
// 16 ms frame.
func circle(rng *rand.Rand) []protocol.Point {
	cx, cy, r := 0.35+rng.Float64()*0.3, 0.35+rng.Float64()*0.3, 0.1+rng.Float64()*0.15
	pts := make([]protocol.Point, 48)
	for i := range pts {
		a := 2 * math.Pi * float64(i) / float64(len(pts)-1)
		rr := r * (1 + 0.05*math.Sin(5*a))
		pts[i] = protocol.Point{cx + rr*math.Cos(a), cy + rr*math.Sin(a), round4(0.6 + 0.2*math.Sin(a)), float64(16 * i)}
	}
	return pts
}
