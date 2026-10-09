package pairing

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// This file is the phone's side of pairing, used by the mock phone and by
// tests. A real phone implements the same steps.

// PairedPC is what the phone stores after a successful pairing.
type PairedPC struct {
	ID    string
	Name  string
	Pin   string
	Token string
}

// ErrCommitment means the PC's nonce did not match its commitment: the
// pairing was attacked or the PC is broken (§4.3 step 4).
var ErrCommitment = errors.New("pairing: the PC's nonce does not match its commitment")

// ErrCodesDiffer means a user said the SAS codes differ.
var ErrCodesDiffer = errors.New("pairing: the codes differ")

// conn is a pairing connection from the phone to a PC.
type conn struct {
	ws     *websocket.Conn
	nextID atomic.Int64
	pin    string // the pin the PC presented
}

// dial connects to wss://hostport/v1/pair. If pin is not empty the PC's
// certificate must match it; otherwise any certificate is accepted and its
// pin recorded (SAS).
func dial(ctx context.Context, hostport, pin string) (*conn, error) {
	c := &conn{}
	var cfg *tls.Config
	if pin != "" {
		cfg = security.PinnedClientTLSConfig(pin)
		c.pin = pin
	} else {
		cfg = security.RecordingClientTLSConfig(func(p string) { c.pin = p })
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: false}}
	ws, _, err := websocket.Dial(ctx, "wss://"+hostport+Path, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(64 << 10)
	c.ws = ws
	return c, nil
}

func (c *conn) close() { c.ws.Close(websocket.StatusNormalClosure, "") }

// call sends one request and waits for its response. The listener sends no
// notifications, so the next frame is the answer.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	b, err := json.Marshal(protocol.NewRequest(protocol.IntID(id), method, params))
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("pairing: %s: %w", method, err)
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("pairing: %s: %w", method, err)
		}
		var m protocol.Message
		if json.Unmarshal(data, &m) != nil || m.Kind() != protocol.KindResponse {
			continue
		}
		if got, ok := protocol.ParseIntID(m.ID); !ok || got != id {
			continue
		}
		if m.Error != nil {
			return m.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

// PairQR pairs by QR code (§4.2): it connects to the hosts of u in order,
// verifying the PC's pin, proves knowledge of the secret, and issues token.
// The PC's user must confirm, so ctx should allow for that.
func PairQR(ctx context.Context, u URI, phone PhoneInfo, token string) (PairedPC, error) {
	var (
		c       *conn
		lastErr error
	)
	for _, h := range u.Hosts {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c, lastErr = dial(dctx, net.JoinHostPort(h, strconv.Itoa(u.Port)), u.Pin)
		cancel()
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			return PairedPC{}, ctx.Err()
		}
	}
	if c == nil {
		return PairedPC{}, fmt.Errorf("pairing: cannot reach the PC: %w", lastErr)
	}
	defer c.close()
	var res PCResult
	if err := c.call(ctx, MethodQR, QRParams{Protocol: Protocol, Secret: u.Secret, Phone: phone}, &res); err != nil {
		return PairedPC{}, err
	}
	if res.PC.ID != u.ID {
		return PairedPC{}, fmt.Errorf("pairing: the PC says it is %q, the QR code says %q", res.PC.ID, u.ID)
	}
	return c.finish(ctx, res.PC, token)
}

func (c *conn) finish(ctx context.Context, pc protocol.PCInfo, token string) (PairedPC, error) {
	var res PCResult
	if err := c.call(ctx, MethodFinish, FinishParams{Token: token}, &res); err != nil {
		return PairedPC{}, err
	}
	if res.PC.ID != pc.ID {
		return PairedPC{}, fmt.Errorf("pairing: the PC changed its id from %q to %q", pc.ID, res.PC.ID)
	}
	return PairedPC{ID: res.PC.ID, Name: res.PC.Name, Pin: c.pin, Token: token}, nil
}

// SASConfirmer shows the code to the phone's user and returns whether they
// say it matches the PC's.
type SASConfirmer func(ctx context.Context, pc protocol.PCInfo, code string) (bool, error)

// PairSAS pairs by numeric comparison (§4.3) with the listener at hostport
// ("host:port", e.g. from mDNS). confirm is asked while the PC's user is
// asked too.
func PairSAS(ctx context.Context, hostport string, phone PhoneInfo, token string, confirm SASConfirmer) (PairedPC, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := dial(dctx, hostport, "")
	cancel()
	if err != nil {
		return PairedPC{}, fmt.Errorf("pairing: cannot reach the PC: %w", err)
	}
	defer c.close()

	var begin SASBeginResult
	if err := c.call(ctx, MethodSASBegin, SASBeginParams{Protocol: Protocol, Phone: phone}, &begin); err != nil {
		return PairedPC{}, err
	}
	nphone := security.RandomBytes(security.NonceSize)
	var nr NonceMessage
	if err := c.call(ctx, MethodSASNonce, NonceMessage{Nonce: security.B64(nphone)}, &nr); err != nil {
		return PairedPC{}, err
	}
	npc, err := security.DecodeB64(nr.Nonce)
	if err != nil || len(npc) != security.NonceSize || !security.CheckCommitment(begin.Commitment, npc) {
		// Tell the PC to discard everything; the answer doesn't matter.
		f := false
		_ = c.call(ctx, MethodSASConfirm, SASConfirmParams{Match: &f}, nil)
		return PairedPC{}, ErrCommitment
	}
	code := security.SASCode(c.pin, phone.Pin, npc, nphone)
	ok, err := confirm(ctx, begin.PC, code)
	if err != nil {
		return PairedPC{}, err
	}
	if err := c.call(ctx, MethodSASConfirm, SASConfirmParams{Match: &ok}, nil); err != nil {
		if code, _ := protocol.ErrorCodeOf(err); code == protocol.CodeSASMismatch {
			return PairedPC{}, fmt.Errorf("%w (%v)", ErrCodesDiffer, err)
		}
		return PairedPC{}, err
	}
	if !ok {
		return PairedPC{}, ErrCodesDiffer
	}
	return c.finish(ctx, begin.PC, token)
}
