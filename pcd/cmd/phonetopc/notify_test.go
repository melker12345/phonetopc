package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func TestNotify(t *testing.T) {
	srv, addr := startMock(t)
	ctx := context.Background()
	code, out, errs := runCLI(t, ctx, nil, "--insecure-dev", "--addr", addr, "notify", "Claude Code is done", "--body", `3 files\nchanged`, "--id", "claude")
	if code != 0 || strings.TrimSpace(out) != "claude" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errs)
	}
	n := srv.User("").Notifications()
	if len(n) != 1 || n[0].Body != "3 files\nchanged" {
		t.Errorf("notifications %+v", n)
	}

	// --wait prints the action the user pressed.
	go func() {
		for range 500 {
			for _, n := range srv.User("").Notifications() {
				if n.Title == "Deploy?" {
					srv.User("").Act(n.ID, "ship_it")
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	code, out, errs = runCLI(t, ctx, nil, "--insecure-dev", "--addr", addr, "notify", "Deploy?", "--action", "Ship it", "--action", "no:Not now", "--wait")
	if code != 0 || strings.TrimSpace(out) != "ship_it" {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errs)
	}

	code, _, errs = runCLI(t, ctx, nil, "--insecure-dev", "--addr", addr, "notify", "a\nb")
	if code != 2 || !strings.Contains(errs, "title") {
		t.Errorf("newline in title: code %d, %s", code, errs)
	}
	srv.AllowNotify(false)
	code, _, errs = runCLI(t, ctx, nil, "--insecure-dev", "--addr", addr, "notify", "hi")
	if code != 1 || !strings.Contains(errs, "turn them on") {
		t.Errorf("not allowed: code %d, %s", code, errs)
	}
}

func TestFormatPanelEvents(t *testing.T) {
	reading := func(source, typ, value string) client.Reading {
		return client.Reading{Source: source, Type: typ, Value: json.RawMessage(value)}
	}
	for _, c := range []struct {
		r    client.Reading
		want string
	}{
		{reading("panel", "press", `{"panel":"deck","pseq":3,"widget":"mute"}`), "deck p3 mute"},
		{reading("panel", "change", `{"panel":"deck","pseq":4,"widget":"vol","value":42,"final":true}`), "deck p4 vol = 42 (final)"},
		{reading("panel", "state", `{"panel":"deck","pseq":0,"state":"closed","reason":"user"}`), "deck p0 closed (user)"},
		{reading("panel", "stroke", `{"panel":"pad","pseq":7,"widget":"ink","stroke":12,"phase":"move","tool":"finger","points":[[0.412,0.2331,1,0],[0.4153,0.2402,1,16]]}`),
			"pad p7 ink stroke 12 move finger 2 pt (0.4120,0.2331)…(0.4153,0.2402)"},
		{reading("notification", "action", `{"notification":"claude","action":"open"}`), "claude open"},
		{reading("notification", "tap", `{"notification":"claude"}`), "claude"},
	} {
		if got, ok := formatEvent(c.r); !ok || got != c.want {
			t.Errorf("formatEvent(%s) = %q, want %q", c.r.Value, got, c.want)
		}
	}
	if _, ok := formatEvent(reading(protocol.SourceBattery, "state", `{"level":1}`)); ok {
		t.Error("battery is not an event source")
	}
}
