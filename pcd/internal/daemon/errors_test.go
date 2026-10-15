package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// refusing puts a proxy between the daemon and p that answers the given
// methods itself with an error, as a phone that lacks them would.
func refusing(t *testing.T, p *phone, errs map[string]*protocol.Error) daemon.Dialer {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		up, _, err := websocket.Dial(ctx, client.URL(p.addr), nil)
		if err != nil {
			return
		}
		defer up.CloseNow()
		down.SetReadLimit(-1)
		up.SetReadLimit(-1)
		var mu sync.Mutex
		send := func(b []byte) error {
			mu.Lock()
			defer mu.Unlock()
			return down.Write(ctx, websocket.MessageText, b)
		}
		go func() {
			defer down.CloseNow()
			for {
				_, b, err := up.Read(ctx)
				if err != nil || send(b) != nil {
					return
				}
			}
		}()
		for {
			_, b, err := down.Read(ctx)
			if err != nil {
				return
			}
			var req protocol.Request
			json.Unmarshal(b, &req)
			if e, ok := errs[req.Method]; ok {
				res, _ := json.Marshal(protocol.NewErrorResponse(req.ID, e))
				err = send(res)
			} else {
				err = up.Write(ctx, websocket.MessageText, b)
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(hs.Close)
	addr := strings.TrimPrefix(hs.URL, "http://")
	return func(ctx context.Context) (*client.Client, error) { return client.Dial(ctx, client.URL(addr)) }
}

type call struct{ method, path, body string }

func devDaemon(tweak func(*mockphone.Options)) func(t *testing.T) string {
	return func(t *testing.T) string {
		opts := quietOptions()
		if tweak != nil {
			tweak(&opts)
		}
		_, _, url := connectedDaemon(t, opts)
		return url
	}
}

func refusingDaemon(errs map[string]*protocol.Error) func(t *testing.T) string {
	return func(t *testing.T) string {
		p := newPhone(t, quietOptions())
		p.start()
		d, url := startDaemon(t, refusing(t, p, errs))
		if err := d.WaitConnected(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		return url
	}
}

func secureDaemon(tweak func(*mockphone.Options)) func(t *testing.T) string {
	return func(t *testing.T) string {
		d, url := startDaemon(t, newSecurePhone(t, tweak).dial)
		if err := d.WaitConnected(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		return url
	}
}

func revokedDaemon(t *testing.T) string {
	sp := newSecurePhone(t, nil)
	d, url := startDaemon(t, sp.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.st.RemovePC("pc-test"); err != nil {
		t.Fatal(err)
	}
	sp.srv.Recheck()
	waitFor(t, func() bool {
		s := d.Status()
		return !s.Connected && strings.Contains(s.Error, "unknown or revoked token")
	})
	return url
}

func offlineDaemon(t *testing.T) string {
	_, url := startDaemon(t, newPhone(t, quietOptions()).dial)
	return url
}

func tokenDaemon(t *testing.T) string {
	_, d, _ := connectedDaemon(t, quietOptions())
	hs := httptest.NewServer(daemon.RequireToken("s3cret-token", d.Handler()))
	t.Cleanup(hs.Close)
	return hs.URL
}

const buttonLayout = `{"type":"button","id":"go","label":"Go"}`

func panelCalls(n int) []call {
	var calls []call
	for i := range n {
		calls = append(calls, call{"PUT", "/v1/panels/p" + string(rune('a'+i)), `{"title":"P","layout":` + buttonLayout + `}`})
	}
	return calls
}

// Every row of LOCAL-API.md §2 the daemon can be made to answer. Not
// here: 409 approval_key_changed (TestApprovals), 500 internal_error (a
// bug) and 504 no_answer (takes the approval timeout plus slack, 10 s).
func TestErrorTable(t *testing.T) {
	fiveSecs := `{"reason":"Deploy?","timeout_ms":5000}`
	for _, tc := range []struct {
		name    string
		daemon  func(t *testing.T) string
		before  []call
		req     call
		host    string
		status  int
		code    string
		rpcCode protocol.ErrorCode
	}{
		{name: "bad JSON", daemon: devDaemon(nil), req: call{"POST", "/v1/notifications", `{"title":`}, status: 400, code: "invalid_request"},
		{name: "unknown member", daemon: devDaemon(nil), req: call{"PUT", "/v1/panels/x", `{"title":"X","layout":` + buttonLayout + `,"colour":"red"}`}, status: 400, code: "invalid_request"},
		{name: "two JSON values", daemon: devDaemon(nil), req: call{"POST", "/v1/notifications", `{"title":"a"}{"title":"b"}`}, status: 400, code: "invalid_request"},
		{name: "body over 1 MiB", daemon: devDaemon(nil), req: call{"POST", "/v1/notifications", `{"title":"` + strings.Repeat("x", 1<<20) + `"}`}, status: 400, code: "invalid_request"},
		{name: "bad query", daemon: devDaemon(nil), req: call{"GET", "/v1/events?source=gyro&rate=-1", ""}, status: 400, code: "invalid_request"},
		{name: "phone finds a param invalid", daemon: devDaemon(nil), req: call{"GET", "/v1/sensors/battery?type=nope", ""}, status: 400, code: "invalid_params", rpcCode: protocol.CodeInvalidParams},
		{name: "TCP without token", daemon: tokenDaemon, req: call{"GET", "/v1/status", ""}, status: 401, code: "unauthorized"},
		{name: "TCP with a foreign Host", daemon: tokenDaemon, req: call{"GET", "/v1/status?token=s3cret-token", ""}, host: "evil.example", status: 403, code: "forbidden"},
		{name: "notifications not allowed", daemon: devDaemon(func(o *mockphone.Options) { o.NoNotify = true }), req: call{"POST", "/v1/notifications", `{"title":"hi"}`}, status: 403, code: "not_allowed", rpcCode: protocol.CodeNotAllowed},
		{name: "panels not allowed", daemon: devDaemon(func(o *mockphone.Options) { o.NoPanels = true }), req: call{"PUT", "/v1/panels/x", `{"title":"X","layout":` + buttonLayout + `}`}, status: 403, code: "not_allowed", rpcCode: protocol.CodeNotAllowed},
		{name: "unknown path", daemon: devDaemon(nil), req: call{"GET", "/v2/status", ""}, status: 404, code: "not_found"},
		{name: "unknown source", daemon: devDaemon(nil), req: call{"GET", "/v1/sensors/nope", ""}, status: 404, code: "unknown_source", rpcCode: protocol.CodeUnknownSource},
		{name: "unknown panel", daemon: devDaemon(nil), req: call{"DELETE", "/v1/panels/nope", ""}, status: 404, code: "unknown_panel", rpcCode: protocol.CodeUnknownPanel},
		{name: "wrong method", daemon: devDaemon(nil), req: call{"GET", "/v1/notifications", ""}, status: 405, code: "method_not_allowed"},
		{name: "source unavailable", daemon: devDaemon(func(o *mockphone.Options) { o.Unavailable = []string{"light"} }), req: call{"GET", "/v1/sensors/light", ""}, status: 409, code: "source_unavailable", rpcCode: protocol.CodeSourceUnavailable},
		{name: "no value yet", daemon: devDaemon(nil), req: call{"GET", "/v1/sensors/nfc", ""}, status: 409, code: "no_value", rpcCode: protocol.CodeNoValue},
		{name: "too many panels", daemon: devDaemon(nil), before: panelCalls(protocol.MaxPanels), req: panelCalls(protocol.MaxPanels + 1)[protocol.MaxPanels], status: 409, code: "too_many_panels", rpcCode: protocol.CodeTooManyPanels},
		{name: "approval in dev mode", daemon: devDaemon(nil), req: call{"POST", "/v1/approvals", fiveSecs}, status: 409, code: "approval_unavailable"},
		{name: "too many notifications", daemon: devDaemon(func(o *mockphone.Options) { o.NotifyPerMinute = 1 }), before: []call{{"POST", "/v1/notifications", `{"title":"a"}`}}, req: call{"POST", "/v1/notifications", `{"title":"b"}`}, status: 429, code: "rate_limited", rpcCode: protocol.CodeRateLimited},
		{name: "phone lacks the method", daemon: refusingDaemon(map[string]*protocol.Error{protocol.MethodGet: protocol.NewError(protocol.CodeMethodNotFound, "no get")}), req: call{"GET", "/v1/sensors/battery", ""}, status: 501, code: "method_not_found", rpcCode: protocol.CodeMethodNotFound},
		{name: "forged approval", daemon: secureDaemon(func(o *mockphone.Options) { o.TamperApprovals = true }), req: call{"POST", "/v1/approvals", fiveSecs}, status: 502, code: "approval_invalid"},
		{name: "phone error with a name", daemon: refusingDaemon(map[string]*protocol.Error{protocol.MethodGet: protocol.NewError(protocol.CodeInternalError, "boom")}), req: call{"GET", "/v1/sensors/battery", ""}, status: 502, code: "internal_error", rpcCode: protocol.CodeInternalError},
		{name: "phone error without a name", daemon: refusingDaemon(map[string]*protocol.Error{protocol.MethodGet: {Code: -32099, Message: "odd"}}), req: call{"GET", "/v1/sensors/battery", ""}, status: 502, code: "phone_error", rpcCode: -32099},
		{name: "phone not connected", daemon: offlineDaemon, req: call{"GET", "/v1/sensors", ""}, status: 503, code: "phone_unavailable"},
		{name: "PC revoked", daemon: revokedDaemon, req: call{"GET", "/v1/sensors/battery", ""}, status: 503, code: "unauthorized", rpcCode: protocol.CodeUnauthorized},
		{name: "PC revoked mid-session", daemon: refusingDaemon(map[string]*protocol.Error{protocol.MethodGet: protocol.NewError(protocol.CodeUnauthorized, "revoked")}), req: call{"GET", "/v1/sensors/battery", ""}, status: 503, code: "unauthorized", rpcCode: protocol.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := tc.daemon(t)
			for _, c := range tc.before {
				if r := do(t, c.method, url+c.path, c.body); r.status != 200 {
					t.Fatalf("%s %s: %s", c.method, c.path, r.raw)
				}
			}
			req, _ := http.NewRequestWithContext(testCtx(t), tc.req.method, url+tc.req.path, strings.NewReader(tc.req.body))
			if tc.host != "" {
				req.Host = tc.host
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var body struct {
				Error struct {
					Code    string             `json:"code"`
					Message string             `json:"message"`
					RPCCode protocol.ErrorCode `json:"rpc_code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			e := body.Error
			if res.StatusCode != tc.status || e.Code != tc.code || e.RPCCode != tc.rpcCode || e.Message == "" {
				t.Fatalf("got %d %+v, want %d %q rpc_code %d", res.StatusCode, e, tc.status, tc.code, tc.rpcCode)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q", ct)
			}
			if tc.status == 405 && res.Header.Get("Allow") == "" {
				t.Error("405 without Allow")
			}
		})
	}
}
