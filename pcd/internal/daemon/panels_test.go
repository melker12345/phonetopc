package daemon_test

import (
	"errors"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

const padLayout = `{"type":"column","children":[{"type":"toggle","id":"mic","label":"Mic","value":false},{"type":"button","id":"go","label":"Go"},{"type":"canvas","id":"pad"}]}`

func showPad(t *testing.T, opts mockphone.Options) (*mockphone.User, string) {
	t.Helper()
	srv, _, url := connectedDaemon(t, opts)
	if r := do(t, "PUT", url+"/v1/panels/deck", `{"title":"Deck","layout":`+padLayout+`}`); r.status != 200 {
		t.Fatalf("show: %s", r.raw)
	}
	u := srv.User("")
	if err := u.Look("deck"); err != nil {
		t.Fatal(err)
	}
	return u, url
}

func TestPanelUpdateSetsWidgetOnPhone(t *testing.T) {
	u, url := showPad(t, quietOptions())
	if r := do(t, "PATCH", url+"/v1/panels/deck", `{"updates":[{"widget":"mic","set":{"value":true}}]}`); r.status != 200 || r.raw != "{}\n" {
		t.Fatalf("update: %d %s", r.status, r.raw)
	}
	// Toggling flips the value the update set.
	if on, err := u.Toggle("deck", "mic"); err != nil || on {
		t.Fatalf("toggle after update: %v, %v", on, err)
	}
}

func TestPanelUpdateIsAllOrNothing(t *testing.T) {
	u, url := showPad(t, quietOptions())
	r := do(t, "PATCH", url+"/v1/panels/deck", `{"updates":[{"widget":"mic","set":{"value":true}},{"widget":"nope","set":{"text":"x"}}]}`)
	wantStatus(t, r, 400, "invalid_params")
	if on, err := u.Toggle("deck", "mic"); err != nil || !on {
		t.Fatalf("the valid half was applied: toggle gave %v, %v", on, err)
	}
	wantStatus(t, do(t, "PATCH", url+"/v1/panels/deck", `{"updates":[],"extra":1}`), 400, "invalid_request")
}

func TestCanvasClearErasesInk(t *testing.T) {
	opts := quietOptions()
	opts.CanvasPoints = 4
	u, url := showPad(t, opts)
	stroke := mockphone.Stroke{Points: []protocol.Point{{0.1, 0.1, 1, 0}, {0.2, 0.2, 1, 10}, {0.3, 0.3, 1, 20}, {0.4, 0.4, 1, 30}}}
	if err := u.Draw("deck", "pad", stroke); err != nil {
		t.Fatal(err)
	}
	if err := u.Draw("deck", "pad", stroke); !errors.Is(err, mockphone.ErrCanvasFull) {
		t.Fatalf("second stroke on a full canvas: %v", err)
	}
	if r := do(t, "POST", url+"/v1/panels/deck/canvases/pad/clear", ""); r.status != 200 || r.raw != "{}\n" {
		t.Fatalf("clear: %d %s", r.status, r.raw)
	}
	if err := u.Draw("deck", "pad", stroke); err != nil {
		t.Fatalf("stroke after clear: %v", err)
	}

	wantStatus(t, do(t, "POST", url+"/v1/panels/deck/canvases/go/clear", ""), 400, "invalid_params")
	wantStatus(t, do(t, "POST", url+"/v1/panels/nope/canvases/pad/clear", ""), 404, "unknown_panel")
	wantStatus(t, do(t, "GET", url+"/v1/panels/deck/canvases/pad/clear", ""), 405, "method_not_allowed")
}

func TestPanelDeleteRemovesItFromPhone(t *testing.T) {
	u, url := showPad(t, quietOptions())
	if r := do(t, "DELETE", url+"/v1/panels/deck", ""); r.status != 200 || r.raw != "{}\n" {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	if ps := u.Panels(); len(ps) != 0 {
		t.Fatalf("phone still has %+v", ps)
	}
	if r := do(t, "GET", url+"/v1/panels", ""); r.raw != "{\"panels\":[]}\n" {
		t.Fatalf("list: %s", r.raw)
	}
	wantStatus(t, do(t, "DELETE", url+"/v1/panels/deck", ""), 404, "unknown_panel")
}
