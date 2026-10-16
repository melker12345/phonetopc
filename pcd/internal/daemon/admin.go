package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/pairrun"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// The daemon owns the credential store, so pairing and the other changes
// to it go through the API (LOCAL-API.md §9). Changes are for root only:
// whoever may change the pairings decides which phone's approvals count.

func (d *Daemon) adminRoutes() []route {
	return []route{
		{"GET", "/v1/pc", d.handlePC},
		{"GET", "/v1/devices", d.handleDevices},
		{"DELETE", "/v1/devices/{device}", d.handleUnpair},
		{"GET", "/v1/approval-key", d.handleApprovalKey},
		{"POST", "/v1/approval-key", d.handleApprovalKey},
		{"GET", "/v1/pairing", d.handlePairing},
	}
}

// PCInfo is the answer to GET /v1/pc.
type PCInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pin  string `json:"pin"`
}

// DeviceInfo is a paired phone in GET /v1/devices: its devices.json entry
// without the token.
type DeviceInfo struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Pin                 string   `json:"pin"`
	Port                int      `json:"port"`
	Addresses           []string `json:"addresses"`
	PairedAt            string   `json:"paired_at,omitempty"`
	ApprovalKey         string   `json:"approval_key,omitempty"`
	ApprovalAttestation string   `json:"approval_attestation,omitempty"`
	DecryptKey          string   `json:"decrypt_key,omitempty"`
	DecryptAttestation  string   `json:"decrypt_attestation,omitempty"`
}

// DevicesResponse is the answer to GET /v1/devices.
type DevicesResponse struct {
	Devices []DeviceInfo `json:"devices"`
}

// DeviceRef names a paired phone.
type DeviceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ApprovalKeyRequest is the body of POST /v1/approval-key.
type ApprovalKeyRequest struct {
	Unattested bool `json:"unattested,omitempty"`
}

// ApprovalKeyInfo is the answer to GET and POST /v1/approval-key.
type ApprovalKeyInfo struct {
	Device DeviceRef `json:"device"`
	// Accepted is the key accepted before this request, "" if none.
	Accepted string `json:"accepted"`
	// Current is the phone's key now, "" if it offers no approvals.
	Current     string `json:"current,omitempty"`
	Attestation string `json:"attestation,omitempty"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
}

// ApprovalKeyInfo statuses.
const (
	KeyTrusted    = "trusted"
	KeyFirstSeen  = "first_seen" // trusted, seen for the first time now
	KeyAccepted   = "accepted"   // by this POST
	KeyChanged    = "changed"
	KeyUnattested = "unattested"
	KeyRefused    = "refused" // StrongBox is required and the key isn't in it
	KeyNone       = "none"    // the phone offers no approvals
)

func (d *Daemon) credentials(w http.ResponseWriter) *store.Store {
	if d.opts.Store == nil {
		writeError(w, http.StatusConflict, "dev_mode", "phonetopcd runs in developer mode, without paired phones")
	}
	return d.opts.Store
}

// failStore answers errors of the store's device selection, and the rest
// as fail does.
func (d *Daemon) failStore(w http.ResponseWriter, err error) {
	var amb *store.AmbiguousError
	switch {
	case errors.Is(err, store.ErrNoDevices), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_device", err.Error())
	case errors.As(err, &amb):
		writeError(w, http.StatusConflict, "ambiguous_device", amb.Error())
	default:
		d.fail(w, err)
	}
}

func (d *Daemon) handlePC(w http.ResponseWriter, r *http.Request) {
	st := d.credentials(w)
	if st == nil {
		return
	}
	pc, err := st.PC()
	if err != nil {
		d.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PCInfo{ID: pc.ID, Name: pc.Name, Pin: pc.Pin})
}

func (d *Daemon) handleDevices(w http.ResponseWriter, r *http.Request) {
	st := d.credentials(w)
	if st == nil {
		return
	}
	devs, err := st.Devices()
	if err != nil {
		d.fail(w, err)
		return
	}
	res := DevicesResponse{Devices: []DeviceInfo{}}
	for _, dev := range devs {
		res.Devices = append(res.Devices, DeviceInfo{
			ID: dev.ID, Name: dev.Name, Pin: dev.Pin, Port: dev.Port, Addresses: dev.Addresses, PairedAt: dev.PairedAt,
			ApprovalKey: dev.ApprovalKey, ApprovalAttestation: dev.ApprovalAttestation,
			DecryptKey: dev.DecryptKey, DecryptAttestation: dev.DecryptAttestation,
		})
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Daemon) handleUnpair(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	st := d.credentials(w)
	if st == nil {
		return
	}
	dev, err := st.RemoveDevice(r.PathValue("device"))
	if err != nil {
		d.failStore(w, err)
		return
	}
	d.pairingsChanged(dev.ID)
	writeJSON(w, http.StatusOK, DeviceRef{ID: dev.ID, Name: dev.Name})
}

// handleApprovalKey shows the phone's approval key (APPROVAL.md §3), and on
// POST accepts it.
func (d *Daemon) handleApprovalKey(w http.ResponseWriter, r *http.Request) {
	accept := r.Method == http.MethodPost
	if accept && !admin(w, r) {
		return
	}
	st := d.credentials(w)
	if st == nil {
		return
	}
	var req ApprovalKeyRequest
	if accept {
		if err := decode(r, &req); err != nil {
			d.fail(w, err)
			return
		}
	}
	before, err := st.Find(d.opts.Device)
	if err != nil {
		d.failStore(w, err)
		return
	}
	cl, err := d.phone(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	info := ApprovalKeyInfo{Device: DeviceRef{ID: before.ID, Name: before.Name}, Accepted: before.ApprovalKey}
	offered, err := cl.CheckApprovalKey()
	if !offered {
		info.Status = KeyNone
		writeJSON(w, http.StatusOK, info)
		return
	}
	h := cl.Server()
	info.Current = h.Approval.Pin
	var (
		changed *client.ApprovalKeyChangedError
		kae     *client.KeyAttestationError
	)
	switch {
	case errors.As(err, &kae) && kae.Check == client.CheckStrongBox:
		info.Status, info.Message = KeyRefused, kae.Err.Error()
	case (errors.As(err, &changed) || errors.As(err, &kae)) && !accept:
		info.Status, info.Message = KeyUnattested, err.Error()
		if changed != nil {
			info.Status = KeyChanged
		}
	case errors.As(err, &changed) || errors.As(err, &kae):
		pin, perr := security.ApprovalKeyPin(h.Approval.Key)
		if perr != nil {
			d.fail(w, &client.ApprovalInvalidError{Problem: "the phone's hello has a bad approval key", Err: perr})
			return
		}
		level, aerr := cl.AttestApprovalKey(r.Context())
		if aerr != nil {
			if !req.Unattested {
				d.fail(w, aerr)
				return
			}
			level = client.Unattested
		}
		if err := st.AcceptApprovalKey(before.ID, pin, level); err != nil {
			d.failStore(w, err)
			return
		}
		info.Status, info.Attestation = KeyAccepted, level
	case err != nil:
		d.fail(w, err)
		return
	default:
		after, err := st.Find(before.ID)
		if err != nil {
			d.failStore(w, err)
			return
		}
		info.Status, info.Attestation = KeyTrusted, after.ApprovalAttestation
		if before.ApprovalKey == "" {
			info.Status = KeyFirstSeen
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// PairingStart is the first message of GET /v1/pairing.
type PairingStart struct {
	// Yes confirms the QR code without asking. Find on network is off then,
	// and no paired phone is replaced (SECURITY.md §4.3, §4.4).
	Yes       bool     `json:"yes,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
	Listen    string   `json:"listen,omitempty"`
	Hosts     []string `json:"hosts,omitempty"`
	NoMDNS    bool     `json:"no_mdns,omitempty"`
}

// PairingPhone is a phone in a PairingMessage.
type PairingPhone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pin  string `json:"pin,omitempty"`
}

// PairingMessage is what the daemon sends during GET /v1/pairing.
type PairingMessage struct {
	Type       string        `json:"type"`
	URI        string        `json:"uri,omitempty"`
	PC         *PCInfo       `json:"pc,omitempty"`
	Port       int           `json:"port,omitempty"`
	RandomPort bool          `json:"random_port,omitempty"`
	Hosts      []string      `json:"hosts,omitempty"`
	Advertised bool          `json:"advertised,omitempty"`
	Message    string        `json:"message,omitempty"`
	Prompt     string        `json:"prompt,omitempty"`
	Way        string        `json:"way,omitempty"`
	Phone      *PairingPhone `json:"phone,omitempty"`
	Code       string        `json:"code,omitempty"`
	Replaces   *PairingPhone `json:"replaces,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// PairingMessage types.
const (
	PairListening = "listening"
	PairQR        = "qr"
	PairLog       = "log"
	PairConfirm   = "confirm"
	PairAbandoned = "abandoned"
	PairPaired    = "paired"
	PairError     = "error"
)

// PairingAnswer answers a PairingMessage of type confirm.
type PairingAnswer struct {
	Type   string `json:"type"` // "answer"
	Prompt string `json:"prompt"`
	Yes    bool   `json:"yes"`
}

// handlePairing runs a pairing listener for the client on a WebSocket and
// relays its questions to the client: the client's user confirms, as
// SECURITY.md §4 requires.
func (d *Daemon) handlePairing(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	st := d.credentials(w)
	if st == nil {
		return
	}
	if !d.pairing.TryLock() {
		writeError(w, http.StatusConflict, "busy", "another pairing is running")
		return
	}
	defer d.pairing.Unlock()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	pc := &pairingConn{conn: conn, answers: make(chan PairingAnswer, 8)}

	var start PairingStart
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	err = readJSON(sctx, conn, &start)
	scancel()
	expiry := time.Duration(start.TimeoutMS) * time.Millisecond
	switch {
	case err != nil:
		pc.fail("invalid_request", "the first message must be the pairing's options: "+err.Error())
		return
	case start.TimeoutMS != 0 && (expiry < time.Second || expiry > time.Hour):
		pc.fail("invalid_request", "timeout_ms must be between 1000 and 3600000")
		return
	}
	go pc.readAnswers(ctx, cancel)

	s, err := pairrun.Start(pairrun.Options{
		Store: st, Ask: pc, Yes: start.Yes, Listen: start.Listen, Hosts: start.Hosts, NoMDNS: start.NoMDNS, Expiry: expiry,
		NewQR: func(uri string) { pc.send(PairingMessage{Type: PairQR, URI: uri}) },
		Logf: func(format string, args ...any) {
			pc.send(PairingMessage{Type: PairLog, Message: fmt.Sprintf(format, args...)})
		},
		Saved: func(dev store.Device) { d.pairingsChanged(dev.ID) },
	})
	if err != nil {
		pc.fail("error", err.Error())
		return
	}
	if s.MDNSErr != nil {
		pc.send(PairingMessage{Type: PairLog, Message: "mDNS unavailable (" + s.MDNSErr.Error() + "); use the QR code"})
	}
	pc.send(PairingMessage{
		Type: PairListening, URI: s.URI(), PC: &PCInfo{ID: s.PC.ID, Name: s.PC.Name, Pin: s.PC.Pin},
		Port: s.Port, RandomPort: s.RandomPort, Hosts: s.Hosts, Advertised: s.Advertised,
	})
	d.logf("pairing: listening on port %d", s.Port)
	res, err := s.Serve(ctx)
	switch {
	case errors.Is(err, pairing.ErrExpired):
		pc.fail("expired", err.Error())
	case errors.Is(err, pairing.ErrSASLimit):
		pc.fail("sas_limit", err.Error())
	case err != nil:
		pc.fail("error", err.Error())
	default:
		d.logf("pairing: paired with %s (%s)", res.Phone.Name, res.Phone.ID)
		pc.send(PairingMessage{Type: PairPaired, Way: string(res.Way), Phone: &PairingPhone{ID: res.Phone.ID, Name: res.Phone.Name, Pin: res.Phone.Pin}})
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

func readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// pairingConn is the client of GET /v1/pairing, as the pairing's Asker.
type pairingConn struct {
	conn    *websocket.Conn
	answers chan PairingAnswer
	mu      sync.Mutex // one write at a time
}

func (p *pairingConn) send(m PairingMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.conn.Write(ctx, websocket.MessageText, b)
}

func (p *pairingConn) fail(code, msg string) {
	p.send(PairingMessage{Type: PairError, Error: code, Message: msg})
	p.conn.Close(websocket.StatusNormalClosure, "")
}

// readAnswers ends the pairing (cancel) when the client goes away.
func (p *pairingConn) readAnswers(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		_, b, err := p.conn.Read(ctx)
		if err != nil {
			return
		}
		var a PairingAnswer
		if json.Unmarshal(b, &a) != nil {
			continue
		}
		select {
		case p.answers <- a:
		default:
		}
	}
}

// ask relays a question. Each question has a new random id, and only an
// answer with that id counts: nothing sent before the question was asked
// can answer it (SECURITY.md §4.2).
func (p *pairingConn) ask(ctx context.Context, m PairingMessage) (bool, error) {
	m.Type, m.Prompt = PairConfirm, security.NewSecret()
	if err := p.send(m); err != nil {
		return false, err
	}
	for {
		select {
		case a := <-p.answers:
			if a.Type == "answer" && a.Prompt == m.Prompt {
				return a.Yes, nil
			}
		case <-ctx.Done():
			p.send(PairingMessage{Type: PairAbandoned, Prompt: m.Prompt})
			return false, ctx.Err()
		}
	}
}

func pairingPhone(p pairing.PhoneInfo) *PairingPhone {
	return &PairingPhone{ID: p.ID, Name: p.Name, Pin: p.Pin}
}

func replacing(d *store.Device) *PairingPhone {
	if d == nil {
		return nil
	}
	return &PairingPhone{ID: d.ID, Name: d.Name, Pin: d.Pin}
}

func (p *pairingConn) ConfirmQR(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device) (bool, error) {
	return p.ask(ctx, PairingMessage{Way: string(pairing.WayQR), Phone: pairingPhone(phone), Replaces: replacing(replaces)})
}

func (p *pairingConn) ConfirmSAS(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device, code string) (bool, error) {
	return p.ask(ctx, PairingMessage{Way: string(pairing.WaySAS), Phone: pairingPhone(phone), Replaces: replacing(replaces), Code: code})
}
