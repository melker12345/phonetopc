package client_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func startMock(t *testing.T, opts mockphone.Options) (*mockphone.Server, *client.Client, context.Context) {
	t.Helper()
	opts.TapEvery, opts.NFCEvery, opts.UserEvery = -1, -1, -1
	srv := mockphone.New(opts)
	hs := httptest.NewServer(srv.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	c, err := client.Dial(ctx, client.URL(strings.TrimPrefix(hs.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close(); hs.Close(); cancel() })
	return srv, c, ctx
}

func next(t *testing.T, sub *client.Subscription) client.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	panic("unreachable")
}

func TestPanelRoundTrip(t *testing.T) {
	srv, c, ctx := startMock(t, mockphone.Options{})
	sub, err := c.Subscribe(ctx, client.SourcePanel, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mute := client.Button("mute", "🔇 Mute")
	mute.Tone = "bad"
	layout := client.Column(
		client.Text("status", "Claude: working…"),
		client.Grid(2, mute, client.Toggle("mic", "Mic", true)),
		client.Slider("vol", "Volume", 0, 100, 40),
		client.Progress("bar", "Build", -1),
		client.TextInput("say", "Say"),
		client.Canvas("pad", false),
	)
	res, err := c.ShowPanel(ctx, "deck", "Stream deck", layout, client.PanelOptions{KeepAwake: true})
	if err != nil || res.Panel != "deck" || res.Visible {
		t.Fatal(res, err)
	}
	if err := c.UpdatePanel(ctx, "deck", client.Set("status", "text", "Claude: done ✅", "tone", "good"), client.Set("bar", "value", 1)); err != nil {
		t.Fatal(err)
	}
	user := srv.User("")
	if err := user.Press("deck", "mute"); err != nil {
		t.Fatal(err)
	}
	next(t, sub) // visible
	ev := next(t, sub)
	p, err := ev.Panel()
	if err != nil || ev.Type != client.TypePress || p.Widget != "mute" {
		t.Errorf("%s %+v %v", ev.Type, p, err)
	}
	if err := user.Draw("deck", "pad", mockphone.Stroke{Points: []client.Point{{0.25, 0.5, 1, 0}, {0.3, 0.5, 1, 16}}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := next(t, sub).Panel(); p.Phase != protocol.PhaseStart || p.Points[1].X() != 0.3 {
		t.Errorf("%+v", p)
	}
	next(t, sub) // end

	att, err := c.AttachPanel(ctx, "deck", 1)
	if err != nil || att.LastPseq != 3 || att.Values["vol"] != 40.0 || att.Values["mic"] != true {
		t.Fatalf("%+v %v", att, err)
	}
	for range 2 {
		if p, _ := next(t, sub).Panel(); !p.Replayed {
			t.Errorf("not replayed: %+v", p)
		}
	}
	if err := c.ClearCanvas(ctx, "deck", "pad"); err != nil {
		t.Fatal(err)
	}
	if list, err := c.ListPanels(ctx); err != nil || len(list) != 1 || !list[0].Visible {
		t.Errorf("%+v %v", list, err)
	}
	if err := c.ClosePanel(ctx, "deck"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AttachPanel(ctx, "deck", -1); err == nil {
		t.Error("attached a closed panel")
	}

	// Checked before sending.
	if _, err := c.ShowPanel(ctx, "bad", "Bad", client.Button("", "no id"), client.PanelOptions{}); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("want a local layout error, got %v", err)
	}
}

func TestNotify(t *testing.T) {
	srv, c, ctx := startMock(t, mockphone.Options{})
	sub, err := c.Subscribe(ctx, client.SourceNotification, client.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Notify(ctx, "Claude Code is done", client.NotifyOptions{Body: "3 files changed", High: true,
		Actions: []client.NotifyAction{{ID: "open", Label: "Open"}}})
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if err := srv.User("").Act(id, "open"); err != nil {
		t.Fatal(err)
	}
	ev := next(t, sub)
	if n, _ := ev.Notification(); ev.Type != client.TypeAction || n.Notification != id || n.Action != "open" {
		t.Errorf("%s %s", ev.Type, ev.Value)
	}
	if err := c.CancelNotification(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Notify(ctx, "bad\u202E", client.NotifyOptions{}); err == nil {
		t.Error("bidi control accepted")
	}

	srv.AllowNotify(false)
	if _, err := c.Notify(ctx, "hi", client.NotifyOptions{}); !client.IsNotAllowed(err) {
		t.Errorf("want not allowed, got %v", err)
	}
}
