package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

type route struct {
	method, path string
	h            http.HandlerFunc
}

func (d *Daemon) routes() []route {
	return []route{
		{"GET", "/v1/status", d.handleStatus},
		{"GET", "/v1/sensors", d.handleSensors},
		{"GET", "/v1/sensors/{source}", d.handleGet},
		{"GET", "/v1/events", d.handleEvents},
		{"POST", "/v1/notifications", d.handleNotify},
		{"DELETE", "/v1/notifications/{id}", d.handleNotifyCancel},
		{"POST", "/v1/approvals", d.handleApproval},
		{"GET", "/v1/decryption-key", d.handleDecryptKey},
		{"POST", "/v1/decryptions", d.handleDecrypt},
		{"GET", "/v1/panels", d.handlePanelList},
		{"PUT", "/v1/panels/{id}", d.handlePanelShow},
		{"PATCH", "/v1/panels/{id}", d.handlePanelUpdate},
		{"DELETE", "/v1/panels/{id}", d.handlePanelClose},
		{"POST", "/v1/panels/{id}/canvases/{widget}/clear", d.handleCanvasClear},
	}
}

// Handler returns the local API (spec/LOCAL-API.md). It does no
// authentication; wrap it in RequireToken where others could reach it.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	allowed := map[string][]string{}
	var paths []string
	for _, rt := range d.routes() {
		mux.HandleFunc(rt.method+" "+rt.path, rt.h)
		if allowed[rt.path] == nil {
			paths = append(paths, rt.path)
		}
		allowed[rt.path] = append(allowed[rt.path], rt.method)
	}
	// The same paths without a method catch the other methods (the
	// patterns with one take precedence).
	for _, path := range paths {
		allow := strings.Join(allowed[path], ", ")
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.URL.Path+" takes "+allow)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path+"; see spec/LOCAL-API.md")
	})
	return mux
}

// RequireToken guards h for a TCP listener on the loopback address. Programs
// of this user (see ConnContext) get in without a token, unless the request
// looks like it comes from a web page; the rest need the token, as
// "Authorization: Bearer <token>" or, for browsers' EventSource and
// WebSocket, which can't set headers, as "?token=<token>". Requests whose
// Host isn't a loopback name are refused, against DNS rebinding. CORS
// preflights are answered, so a page in a browser can use the API once it
// has the token.
func RequireToken(token string, h http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden", "Host must be localhost, 127.0.0.1 or [::1]")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if sameUser(r) && !fromBrowser(r) {
			h.ServeHTTP(w, r)
			return
		}
		got := r.URL.Query().Get("token")
		if auth := r.Header.Get("Authorization"); auth != "" {
			got, _ = strings.CutPrefix(auth, "Bearer ")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"missing or wrong token: requests from web pages and from other users send \"Authorization: Bearer <token>\" with the token from phonetopcd's token file")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// apiError is the body of every error response.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		RPCCode int    `json:"rpc_code,omitempty"` // the phone's error code, if the phone refused
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeRPCError(w, status, code, msg, 0)
}

func writeRPCError(w http.ResponseWriter, status int, code, msg string, rpc protocol.ErrorCode) {
	var e apiError
	e.Error.Code, e.Error.Message, e.Error.RPCCode = code, msg, int(rpc)
	b, _ := json.Marshal(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// badRequest is a request the daemon refuses before asking the phone.
type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func badRequestf(format string, args ...any) error { return badRequest{fmt.Sprintf(format, args...)} }

var statusOf = map[protocol.ErrorCode]int{
	protocol.CodeInvalidParams:     http.StatusBadRequest,
	protocol.CodeMethodNotFound:    http.StatusNotImplemented, // a phone app older than this daemon
	protocol.CodeUnknownSource:     http.StatusNotFound,
	protocol.CodeSourceUnavailable: http.StatusConflict,
	protocol.CodeNoValue:           http.StatusConflict,
	protocol.CodeUnauthorized:      http.StatusServiceUnavailable,
	protocol.CodeNotAllowed:        http.StatusForbidden,
	protocol.CodeUnknownPanel:      http.StatusNotFound,
	protocol.CodeTooManyPanels:     http.StatusConflict,
	protocol.CodeRateLimited:       http.StatusTooManyRequests,
}

// hintOf adds what to do to the phone's message where that isn't obvious.
var hintOf = map[protocol.ErrorCode]string{
	protocol.CodeNotAllowed:    "; allow notifications and panels for this PC in the phone app",
	protocol.CodeUnknownSource: "; GET /v1/sensors lists the sources",
}

func (d *Daemon) fail(w http.ResponseWriter, err error) {
	var (
		br  badRequest
		ue  *unavailableError
		pe  *protocol.Error
		kce *client.ApprovalKeyChangedError
		kae *client.KeyAttestationError
	)
	switch {
	case errors.As(err, &br):
		writeError(w, http.StatusBadRequest, "invalid_request", br.msg)
	case errors.As(err, &ue) && client.IsUnauthorized(ue.err):
		writeRPCError(w, http.StatusServiceUnavailable, "unauthorized", ue.Error(), protocol.CodeUnauthorized)
	case errors.As(err, &ue):
		writeError(w, http.StatusServiceUnavailable, "phone_unavailable", ue.Error())
	case errors.As(err, &pe):
		status, ok := statusOf[pe.Code]
		if !ok {
			status = http.StatusBadGateway
		}
		name := pe.Code.Name()
		if name == "" {
			name = "phone_error"
		}
		writeRPCError(w, status, name, pe.Message+hintOf[pe.Code], pe.Code)
	case errors.As(err, &kae) && kae.Key == "approval":
		writeError(w, http.StatusConflict, "approval_key_unattested", kae.Error()+
			". Approvals are refused; to accept the key anyway, run `phonetopc approval-key --accept --unattested`. Nothing was sent")
	case errors.As(err, &kae):
		writeError(w, http.StatusConflict, "decrypt_key_unattested", kae.Error()+
			". To use the key anyway, ask for /v1/decryption-key?unattested=true")
	case errors.As(err, &kce):
		writeError(w, http.StatusConflict, "approval_key_changed", kce.Error()+
			". Typically fingerprints were added or removed on the phone; if so, accept the new key with `phonetopc approval-key --accept`. Nothing was sent")
	case errors.Is(err, client.ErrApprovalInvalid):
		writeError(w, http.StatusBadGateway, "approval_invalid", strings.TrimPrefix(err.Error(), "phonetopc: ")+". This is NOT an approval")
	case errors.Is(err, client.ErrApprovalNoAnswer):
		writeError(w, http.StatusGatewayTimeout, "no_answer", strings.TrimPrefix(err.Error(), "phonetopc: "))
	case errors.Is(err, client.ErrDecryptKeyChanged):
		writeError(w, http.StatusConflict, "decrypt_key_changed", strings.TrimPrefix(err.Error(), "phonetopc: ")+
			". What was encrypted to the old key needs your recovery key. Nothing was sent")
	case errors.Is(err, client.ErrDecryptUnavailable):
		writeError(w, http.StatusConflict, "decrypt_unavailable", strings.TrimPrefix(err.Error(), "phonetopc: "))
	case errors.Is(err, client.ErrApprovalNeedsSecureMode):
		writeError(w, http.StatusConflict, "approval_unavailable", strings.TrimPrefix(err.Error(), "phonetopc: "))
	default:
		writeError(w, http.StatusBadGateway, "phone_error", strings.TrimPrefix(err.Error(), "phonetopc: "))
	}
}

// Unknown members are refused so a typo doesn't go unnoticed.
func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil {
		return badRequestf("reading the body: %v", err)
	}
	if len(body) > 1<<20 {
		return badRequestf("the body is larger than 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequestf("the body is not the expected JSON: %v", err)
	}
	if dec.More() {
		return badRequestf("the body has more than one JSON value")
	}
	return nil
}

func (d *Daemon) call(w http.ResponseWriter, r *http.Request, f func(cl *client.Client) (any, error)) {
	cl, err := d.phone(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	res, err := f(cl)
	if err != nil {
		d.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.Status())
}

func (d *Daemon) handleSensors(w http.ResponseWriter, r *http.Request) {
	d.call(w, r, func(cl *client.Client) (any, error) {
		s, err := cl.ListSensors(r.Context())
		return protocol.ListSensorsResult{Sensors: s}, err
	})
}

func (d *Daemon) handleGet(w http.ResponseWriter, r *http.Request) {
	d.call(w, r, func(cl *client.Client) (any, error) {
		var res protocol.Reading
		p := protocol.GetParams{Source: r.PathValue("source"), Type: r.URL.Query().Get("type")}
		err := cl.Call(r.Context(), protocol.MethodGet, p, &res)
		return res, err
	})
}

func (d *Daemon) handleNotify(w http.ResponseWriter, r *http.Request) {
	var p protocol.NotifyParams
	if err := decode(r, &p); err != nil {
		d.fail(w, err)
		return
	}
	if err := protocol.CheckNotify(p); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		var res protocol.NotifyResult
		err := cl.Call(r.Context(), protocol.MethodNotify, p, &res)
		return res, err
	})
}

func (d *Daemon) handleNotifyCancel(w http.ResponseWriter, r *http.Request) {
	d.call(w, r, func(cl *client.Client) (any, error) {
		return struct{}{}, cl.CancelNotification(r.Context(), r.PathValue("id"))
	})
}

// ApprovalRequest is the body of POST /v1/approvals.
type ApprovalRequest struct {
	Reason    string `json:"reason"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"` // 0: 30 s
	Nonce     string `json:"nonce,omitempty"`      // "": the daemon makes one
}

// ApprovalResponse is the answer to POST /v1/approvals.
type ApprovalResponse struct {
	Approved bool           `json:"approved"`
	Why      string         `json:"why"`
	Proof    *ApprovalProof `json:"proof,omitempty"`
}

// ApprovalProof lets a program verify an approval itself (LOCAL-API.md §5).
type ApprovalProof struct {
	Signature string `json:"signature"`
	Key       string `json:"key"`
	KeyPin    string `json:"key_pin"`
	Nonce     string `json:"nonce"`
	PhoneID   string `json:"phone_id"`
	PCID      string `json:"pc_id"`
}

func (d *Daemon) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req ApprovalRequest
	if err := decode(r, &req); err != nil {
		d.fail(w, err)
		return
	}
	if err := protocol.CheckReason(req.Reason); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if req.TimeoutMS != 0 && (timeout < protocol.MinApprovalTimeout || timeout > protocol.MaxApprovalTimeout) {
		d.fail(w, badRequestf("timeout_ms must be between %d and %d", protocol.MinApprovalTimeout.Milliseconds(), protocol.MaxApprovalTimeout.Milliseconds()))
		return
	}
	if req.Nonce != "" {
		if n, err := security.DecodeB64(req.Nonce); err != nil || len(n) != protocol.ApprovalNonceSize {
			d.fail(w, badRequestf("nonce must be base64url of %d bytes", protocol.ApprovalNonceSize))
			return
		}
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		var res client.ApprovalResult
		var err error
		if req.Nonce != "" {
			res, err = cl.RequestApprovalNonce(r.Context(), req.Reason, req.Nonce, timeout)
		} else {
			res, err = cl.RequestApproval(r.Context(), req.Reason, timeout)
		}
		if err != nil {
			return nil, err
		}
		if res.Approved() {
			return ApprovalResponse{Approved: true, Why: "approved", Proof: &ApprovalProof{
				Signature: res.Signature, Key: res.Key, KeyPin: res.KeyPin,
				Nonce: res.Nonce, PhoneID: res.PhoneID, PCID: res.PCID,
			}}, nil
		}
		return ApprovalResponse{Why: string(res.Why)}, nil
	})
}

// DecryptKeyResponse is the answer to GET /v1/decryption-key.
type DecryptKeyResponse struct {
	Key         string `json:"key"`
	Pin         string `json:"pin"`
	Attestation string `json:"attestation"`
}

func (d *Daemon) handleDecryptKey(w http.ResponseWriter, r *http.Request) {
	unattested := false
	if v := r.URL.Query().Get("unattested"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			d.fail(w, badRequestf("unattested must be true or false"))
			return
		}
		unattested = b
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		get := cl.DecryptKey
		if unattested {
			get = cl.DecryptKeyAllowUnattested
		}
		k, err := get()
		if err != nil {
			return nil, err
		}
		return DecryptKeyResponse{Key: security.B64(k.SPKI), Pin: k.Pin, Attestation: k.Attestation}, nil
	})
}

// DecryptRequest is the body of POST /v1/decryptions.
type DecryptRequest struct {
	Reason     string `json:"reason"`
	KeyPin     string `json:"key_pin"`
	Ciphertext string `json:"ciphertext"`
	TimeoutMS  int64  `json:"timeout_ms,omitempty"`
}

// DecryptResponse is the answer to POST /v1/decryptions.
type DecryptResponse struct {
	Decrypted bool   `json:"decrypted"`
	Why       string `json:"why"`
	Plaintext string `json:"plaintext,omitempty"`
}

func (d *Daemon) handleDecrypt(w http.ResponseWriter, r *http.Request) {
	var req DecryptRequest
	if err := decode(r, &req); err != nil {
		d.fail(w, err)
		return
	}
	if err := protocol.CheckReason(req.Reason); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	ct, err := security.DecodeB64(req.Ciphertext)
	if err != nil || len(ct) == 0 {
		d.fail(w, badRequestf("ciphertext must be base64url"))
		return
	}
	if req.KeyPin == "" {
		d.fail(w, badRequestf("key_pin is required"))
		return
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if req.TimeoutMS != 0 && (timeout < protocol.MinApprovalTimeout || timeout > protocol.MaxApprovalTimeout) {
		d.fail(w, badRequestf("timeout_ms must be between %d and %d", protocol.MinApprovalTimeout.Milliseconds(), protocol.MaxApprovalTimeout.Milliseconds()))
		return
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		if k, err := cl.DecryptKey(); err == nil && k.Pin == req.KeyPin && len(ct) != k.Public.Size() {
			return nil, badRequestf("ciphertext is %d bytes; this key's are %d", len(ct), k.Public.Size())
		}
		res, err := cl.Decrypt(r.Context(), req.Reason, req.KeyPin, ct, timeout)
		if err != nil {
			return nil, err
		}
		if res.Decrypted() {
			return DecryptResponse{Decrypted: true, Why: "decrypted", Plaintext: security.B64(res.Plaintext)}, nil
		}
		return DecryptResponse{Why: string(res.Why)}, nil
	})
}

func (d *Daemon) handlePanelList(w http.ResponseWriter, r *http.Request) {
	d.call(w, r, func(cl *client.Client) (any, error) {
		panels, err := cl.ListPanels(r.Context())
		if panels == nil {
			panels = []protocol.PanelInfo{}
		}
		return protocol.PanelListResult{Panels: panels}, err
	})
}

// PanelRequest is the body of PUT /v1/panels/{id}.
type PanelRequest struct {
	Title       string          `json:"title"`
	Layout      json.RawMessage `json:"layout"`
	KeepAwake   bool            `json:"keep_awake,omitempty"`
	Orientation string          `json:"orientation,omitempty"`
}

func (d *Daemon) handlePanelShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req PanelRequest
	if err := decode(r, &req); err != nil {
		d.fail(w, err)
		return
	}
	if len(req.Layout) == 0 {
		d.fail(w, badRequestf("layout is required"))
		return
	}
	if _, err := protocol.ParseLayout(req.Layout); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	var layout client.Widget
	if err := json.Unmarshal(req.Layout, &layout); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	// Go matches member names case-insensitively, so "Label" was ignored
	// above but is the label now: check what is sent too.
	if b, err := json.Marshal(layout); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	} else if _, err := protocol.ParseLayout(b); err != nil {
		d.fail(w, badRequest{err.Error()})
		return
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		res, err := cl.ShowPanel(r.Context(), id, req.Title, layout, client.PanelOptions{KeepAwake: req.KeepAwake, Orientation: req.Orientation})
		if err == nil {
			d.notePanel(id)
		}
		return res, err
	})
}

// PanelUpdateRequest is the body of PATCH /v1/panels/{id}.
type PanelUpdateRequest struct {
	Updates []protocol.WidgetUpdate `json:"updates"`
}

func (d *Daemon) handlePanelUpdate(w http.ResponseWriter, r *http.Request) {
	var req PanelUpdateRequest
	if err := decode(r, &req); err != nil {
		d.fail(w, err)
		return
	}
	d.call(w, r, func(cl *client.Client) (any, error) {
		return struct{}{}, cl.UpdatePanel(r.Context(), r.PathValue("id"), req.Updates...)
	})
}

func (d *Daemon) handlePanelClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.call(w, r, func(cl *client.Client) (any, error) {
		err := cl.ClosePanel(r.Context(), id)
		if err == nil {
			d.forgetPanel(id)
		}
		return struct{}{}, err
	})
}

func (d *Daemon) handleCanvasClear(w http.ResponseWriter, r *http.Request) {
	d.call(w, r, func(cl *client.Client) (any, error) {
		return struct{}{}, cl.ClearCanvas(r.Context(), r.PathValue("id"), r.PathValue("widget"))
	})
}

func streamQuery(r *http.Request) ([]string, StreamOptions, error) {
	q := r.URL.Query()
	var sources []string
	for _, v := range q["source"] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" && !contains(sources, s) {
				sources = append(sources, s)
			}
		}
	}
	if len(sources) == 0 {
		return nil, StreamOptions{}, badRequestf("give at least one source, e.g. /v1/events?source=gyro (GET /v1/sensors lists them)")
	}
	opts := StreamOptions{Type: q.Get("type")}
	for name, dst := range map[string]*float64{"rate": &opts.RateHz, "min_delta": &opts.MinDelta} {
		if v := q.Get(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || (name == "rate" && f == 0) {
				return nil, StreamOptions{}, badRequestf("%s must be a positive number", name)
			}
			*dst = f
		}
	}
	return sources, opts, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// handleEvents streams events: over a WebSocket when the request asks for
// an upgrade, as Server-Sent Events when it accepts text/event-stream (as
// EventSource does), and otherwise as one JSON object per line.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	sources, opts, err := streamQuery(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	s, err := d.openStream(r.Context(), sources, opts)
	if err != nil {
		d.fail(w, err)
		return
	}
	defer d.closeStream(s)

	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		d.streamWebSocket(w, r, s)
		return
	}
	sse := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	rc.Flush()

	write := func(line []byte) error {
		var err error
		if sse {
			_, err = fmt.Fprintf(w, "data: %s\n\n", line)
		} else if _, err = w.Write(line); err == nil {
			// Not append(line, '\n'): line is shared by every stream.
			_, err = io.WriteString(w, "\n")
		}
		return err
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case line := <-s.ch:
			if write(line) != nil {
				return
			}
			// Write what's queued, then flush once.
			for drained := false; !drained; {
				select {
				case line := <-s.ch:
					if write(line) != nil {
						return
					}
				default:
					drained = true
				}
			}
			if rc.Flush() != nil {
				return
			}
		case <-ping.C:
			if sse {
				// A comment: keeps proxies from timing out and finds
				// clients that went away.
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		case <-s.done:
			drainTo(s, write)
			write(encodeDaemonEvent(TypeError, errorValue(s.reason)))
			rc.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

func drainTo(s *stream, write func([]byte) error) {
	for {
		select {
		case line := <-s.ch:
			if write(line) != nil {
				return
			}
		default:
			return
		}
	}
}

func (d *Daemon) streamWebSocket(w http.ResponseWriter, r *http.Request, s *stream) {
	// Cross-origin pages are fine: on TCP, RequireToken already checked the
	// token, which a page can only have if the user gave it to it.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(r.Context()) // nothing to read; ends ctx when the client closes
	write := func(line []byte) error {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, line)
	}
	for {
		select {
		case line := <-s.ch:
			if write(line) != nil {
				return
			}
		case <-s.done:
			drainTo(s, write)
			write(encodeDaemonEvent(TypeError, errorValue(s.reason)))
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-ctx.Done():
			return
		}
	}
}
