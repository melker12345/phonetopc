package mockphone_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Behaviour of notifications and panels (PANELS.md) beyond the single
// requests of panel-server-cases.json.

const padLayout = `{"type":"column","children":[{"type":"toggle","id":"t","label":"T"},{"type":"button","id":"b","label":"B"},{"type":"canvas","id":"c"}]}`

func showPanel(t *testing.T, c *client.Client, id, layout string) protocol.PanelShowResult {
	t.Helper()
	var res protocol.PanelShowResult
	err := c.Call(testCtx(t), protocol.MethodPanelShow, map[string]any{"panel": id, "title": "Pad", "layout": json.RawMessage(layout)}, &res)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func subscribePanel(t *testing.T, c *client.Client) *client.Subscription {
	t.Helper()
	sub, err := c.Subscribe(testCtx(t), protocol.SourcePanel, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func nextPanelEvent(t *testing.T, sub *client.Subscription, typ string) protocol.PanelEvent {
	t.Helper()
	ev := nextEvent(t, sub)
	if ev.Type != typ {
		t.Fatalf("event %s/%s %s, want type %s", ev.Source, ev.Type, ev.Value, typ)
	}
	pe, err := ev.Panel()
	if err != nil {
		t.Fatal(err)
	}
	return pe
}

func noEvent(t *testing.T, sub *client.Subscription) {
	t.Helper()
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event %s/%s %s", ev.Source, ev.Type, ev.Value)
	case <-time.After(100 * time.Millisecond):
	}
}

func dot(x, y float64) mockphone.Stroke {
	return mockphone.Stroke{Points: []protocol.Point{{x, y, 1, 0}}}
}

func TestPanelEventsGoToTheOwnerOnly(t *testing.T) {
	srv, url := start(t, quietOptions())
	a, b := dial(t, url), dial(t, url) // both the dev-mode PC
	subA, subB := subscribePanel(t, a), subscribePanel(t, b)
	if res := showPanel(t, a, "pad", padLayout); res.Visible {
		t.Error("visible without anybody looking")
	}
	user := srv.User("")
	if err := user.Press("pad", "b"); err != nil {
		t.Fatal(err)
	}
	if ev := nextPanelEvent(t, subA, protocol.TypeState); ev.State != protocol.StateVisible || ev.Pseq != 0 {
		t.Errorf("first event %+v", ev)
	}
	if ev := nextPanelEvent(t, subA, protocol.TypePress); ev.Widget != "b" || ev.Pseq != 1 {
		t.Errorf("press %+v", ev)
	}
	noEvent(t, subB)

	// b takes the panel over; a hears nothing more.
	var att protocol.PanelAttachResult
	if err := b.Call(testCtx(t), protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "pad"}, &att); err != nil {
		t.Fatal(err)
	}
	if att.LastPseq != 1 || !att.Visible || att.Values["t"] != false {
		t.Errorf("attach %+v", att)
	}
	if v, err := user.Toggle("pad", "t"); err != nil || !v {
		t.Fatal(v, err)
	}
	if ev := nextPanelEvent(t, subB, protocol.TypeChange); string(ev.Value) != "true" || ev.Pseq != 2 {
		t.Errorf("change %+v", ev)
	}
	noEvent(t, subA)
}

func TestDetachedPanelKeepsInkAndReplays(t *testing.T) {
	srv, url := start(t, quietOptions())
	a := dial(t, url)
	subA := subscribePanel(t, a)
	showPanel(t, a, "pad", padLayout)
	user := srv.User("")
	if err := user.Draw("pad", "c", dot(0.1, 0.1)); err != nil {
		t.Fatal(err)
	}
	nextPanelEvent(t, subA, protocol.TypeState) // visible, pseq 0
	first := nextPanelEvent(t, subA, protocol.TypeStroke)
	if first.Phase != protocol.PhaseStart || *first.Stroke != 0 || len(first.Size) != 2 {
		t.Errorf("start %+v", first)
	}
	if end := nextPanelEvent(t, subA, protocol.TypeStroke); end.Phase != protocol.PhaseEnd || end.Pseq != 2 {
		t.Errorf("end %+v", end)
	}
	a.Close()
	waitFor(t, func() bool { p := user.Panels(); return len(p) == 1 && !p[0].Attached })

	// Detached: buttons are disabled, the canvas takes ink.
	if err := user.Press("pad", "b"); err == nil {
		t.Error("pressed a button of a detached panel")
	}
	pts := []protocol.Point{{0.5, 0.5, 1, 0}, {0.6, 0.5, 1, 16}, {0.7, 0.5, 1, 32}}
	if err := user.Draw("pad", "c", mockphone.Stroke{Points: pts, Chunk: 2}); err != nil {
		t.Fatal(err)
	}

	b := dial(t, url)
	subB := subscribePanel(t, b)
	var att protocol.PanelAttachResult
	if err := b.Call(testCtx(t), protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "pad", AfterPseq: ptr(int64(2))}, &att); err != nil {
		t.Fatal(err)
	}
	if att.LastPseq != 4 {
		t.Errorf("last_pseq %d, want 4", att.LastPseq)
	}
	for _, want := range []struct {
		pseq  int64
		phase string
		n     int
	}{{3, protocol.PhaseStart, 2}, {4, protocol.PhaseEnd, 1}} {
		ev := nextPanelEvent(t, subB, protocol.TypeStroke)
		if ev.Pseq != want.pseq || ev.Phase != want.phase || len(ev.Points) != want.n || !ev.Replayed || *ev.Stroke != 1 {
			t.Errorf("replayed %+v, want pseq %d %s", ev, want.pseq, want.phase)
		}
	}
	noEvent(t, subB)

	// From scratch, after a clear, nothing is replayed.
	if err := b.Call(testCtx(t), protocol.MethodCanvasClear, protocol.CanvasClearParams{Panel: "pad", Widget: "c"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Call(testCtx(t), protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "pad"}, nil); err != nil {
		t.Fatal(err)
	}
	noEvent(t, subB)
	// Live events continue the pseq.
	if err := user.Press("pad", "b"); err != nil {
		t.Fatal(err)
	}
	if ev := nextPanelEvent(t, subB, protocol.TypePress); ev.Pseq != 5 || ev.Replayed {
		t.Errorf("press %+v", ev)
	}
}

func ptr[T any](v T) *T { return &v }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDetachedPanelExpires(t *testing.T) {
	opts := quietOptions()
	opts.PanelExpiry = 50 * time.Millisecond
	srv, url := start(t, opts)
	a := dial(t, url)
	showPanel(t, a, "pad", padLayout)
	a.Close()
	waitFor(t, func() bool { return len(srv.User("").Panels()) == 0 })
	b := dial(t, url)
	err := b.Call(testCtx(t), protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "pad"}, nil)
	wantCode(t, err, protocol.CodeUnknownPanel)
}

func TestPanelShowSwitchesScreenForSamePC(t *testing.T) {
	srv, url := start(t, quietOptions())
	c := dial(t, url)
	sub := subscribePanel(t, c)
	showPanel(t, c, "one", padLayout)
	if err := srv.User("").Look("one"); err != nil {
		t.Fatal(err)
	}
	nextPanelEvent(t, sub, protocol.TypeState) // one visible
	if res := showPanel(t, c, "two", padLayout); !res.Visible {
		t.Error("a panel of the PC on screen: the new panel should be shown")
	}
	if ev := nextPanelEvent(t, sub, protocol.TypeState); ev.Panel != "one" || ev.State != protocol.StateHidden {
		t.Errorf("%+v", ev)
	}
	if ev := nextPanelEvent(t, sub, protocol.TypeState); ev.Panel != "two" || ev.State != protocol.StateVisible || ev.Pseq != 0 {
		t.Errorf("%+v", ev)
	}
	// The user closes it.
	if err := srv.User("").Close("two"); err != nil {
		t.Fatal(err)
	}
	if ev := nextPanelEvent(t, sub, protocol.TypeState); ev.State != protocol.StateClosed || ev.Reason != protocol.ReasonUser {
		t.Errorf("%+v", ev)
	}
}

func TestRevokingPanelsClosesThem(t *testing.T) {
	srv, url := start(t, quietOptions())
	c := dial(t, url)
	sub := subscribePanel(t, c)
	showPanel(t, c, "pad", padLayout)
	srv.AllowPanels(false)
	if ev := nextPanelEvent(t, sub, protocol.TypeState); ev.State != protocol.StateClosed || ev.Reason != protocol.ReasonRevoked {
		t.Errorf("%+v", ev)
	}
	err := c.Call(testCtx(t), protocol.MethodPanelList, nil, nil)
	wantCode(t, err, protocol.CodeNotAllowed)
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Data.Permission != protocol.PermissionPanels {
		t.Errorf("error %+v", err)
	}
	sensors, _ := c.ListSensors(testCtx(t))
	for _, s := range sensors {
		if s.Source == protocol.SourcePanel && s.Available {
			t.Error("panel still available")
		}
	}
}

func TestCanvasFull(t *testing.T) {
	opts := quietOptions()
	opts.CanvasPoints = 3
	srv, url := start(t, opts)
	c := dial(t, url)
	showPanel(t, c, "pad", padLayout)
	user := srv.User("")
	if err := user.Draw("pad", "c", mockphone.Stroke{Points: []protocol.Point{{0, 0, 1, 0}, {1, 1, 1, 10}}}); err != nil {
		t.Fatal(err)
	}
	if err := user.Draw("pad", "c", mockphone.Stroke{Points: []protocol.Point{{0, 0, 1, 0}, {1, 1, 1, 10}}}); !errors.Is(err, mockphone.ErrCanvasFull) {
		t.Fatalf("want canvas full, got %v", err)
	}
	if err := c.Call(testCtx(t), protocol.MethodCanvasClear, protocol.CanvasClearParams{Panel: "pad", Widget: "c"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := user.Draw("pad", "c", dot(0.5, 0.5)); err != nil {
		t.Errorf("after clearing: %v", err)
	}
}

func TestNotificationsLimitAndRate(t *testing.T) {
	opts := quietOptions()
	opts.NotifyPerMinute = 20
	srv, url := start(t, opts)
	c := dial(t, url)
	for i := range 20 {
		if err := c.Call(testCtx(t), protocol.MethodNotify, protocol.NotifyParams{Title: "x"}, nil); err != nil {
			t.Fatal(i, err)
		}
	}
	notes := srv.User("").Notifications()
	if len(notes) != protocol.MaxNotifications || notes[0].ID != "n-5" {
		t.Errorf("%d notifications, oldest %q", len(notes), notes[0].ID)
	}
	err := c.Call(testCtx(t), protocol.MethodNotify, protocol.NotifyParams{Title: "x"}, nil)
	wantCode(t, err, protocol.CodeRateLimited)
}

func TestNotificationEventsStayWithTheirPC(t *testing.T) {
	key, _ := security.GenerateKey()
	cert, _ := security.SelfSigned(key, "panels")
	pin, _ := security.CertPin(cert)
	opts := quietOptions()
	opts.Authenticate = func(token string) (protocol.PCInfo, bool) {
		switch token {
		case "tok-1":
			return protocol.PCInfo{ID: "pc-1", Name: "one"}, true
		case "tok-2":
			return protocol.PCInfo{ID: "pc-2", Name: "two"}, true
		}
		return protocol.PCInfo{}, false
	}
	srv, addr := serveTLS(t, opts, cert)
	dialPC := func(token string) *client.Client {
		c, err := client.Dial(testCtx(t), client.SecureURL(addr), client.WithPin(pin), client.WithToken(token))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	one, two := dialPC("tok-1"), dialPC("tok-2")
	subOne, err := one.Subscribe(testCtx(t), protocol.SourceNotification, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	subTwo, err := two.Subscribe(testCtx(t), protocol.SourceNotification, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*client.Client{one, two} {
		if err := c.Call(testCtx(t), protocol.MethodNotify, protocol.NotifyParams{Title: "hi", ID: "same"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Panels of one PC are invisible to the other.
	showPanel(t, one, "pad", padLayout)
	err = two.Call(testCtx(t), protocol.MethodPanelAttach, protocol.PanelAttachParams{Panel: "pad"}, nil)
	wantCode(t, err, protocol.CodeUnknownPanel)

	if err := srv.User("pc-2").Dismiss("same"); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, subTwo)
	if n, _ := ev.Notification(); ev.Type != protocol.TypeDismiss || n.Notification != "same" {
		t.Errorf("%s %s", ev.Type, ev.Value)
	}
	noEvent(t, subOne)
	if len(srv.User("pc-1").Notifications()) != 1 {
		t.Error("pc-1's notification went away")
	}
}
