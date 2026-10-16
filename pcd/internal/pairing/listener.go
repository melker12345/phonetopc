package pairing

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/hostlimit"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Prompter asks the PC's user to confirm a pairing. Both methods must
// return promptly with ctx.Err() when ctx is done (the phone went away or
// the listener expired).
type Prompter interface {
	// ConfirmQR asks `Pair with "<name>"? [y/N]` (§4.2).
	ConfirmQR(ctx context.Context, phone PhoneInfo) (bool, error)
	// ConfirmSAS asks `Does the phone show 123 456? [y/N]` (§4.3).
	ConfirmSAS(ctx context.Context, phone PhoneInfo, code string) (bool, error)
}

type Way string

const (
	WayQR  Way = "qr"
	WaySAS Way = "sas"
)

type Paired struct {
	Phone PhoneInfo
	Token string
	Way   Way
	// RemoteHost is the address the phone connected from, often also where
	// it serves.
	RemoteHost string
}

type Options struct {
	PC     protocol.PCInfo
	Cert   tls.Certificate // the PC's long-term certificate
	Secret string          // first QR secret; default: a fresh random one
	// NewSecret, if set, receives a fresh QR secret that replaces the
	// current one after a pair_qr reached the prompt but did not pair
	// (refused, or the phone went away): someone may have photographed the
	// QR code and be retrying it (§4.2). The PC shows the new QR code.
	NewSecret func(secret string)

	// Prompt asks the user. Required.
	Prompt Prompter
	// Save stores a pairing (§6). It runs before pair_finish is answered;
	// if it fails, the phone gets internal_error and may retry. Required.
	Save func(Paired) error

	// NoSAS refuses pair_sas_begin with -32021, for a PC that can't ask its
	// user to compare codes (`pair --yes`): nobody may confirm a code on the
	// user's behalf (§4.3).
	NoSAS bool

	// Expiry closes the listener (default 5 minutes, §4.1).
	Expiry time.Duration
	// MaxFailures failed attempts (bad secret, rejection, SAS mismatch) per
	// FailureWindow from one address make the listener refuse further
	// attempts from it until the window has passed. Defaults 5 and 1 minute
	// (§4.1).
	MaxFailures   int
	FailureWindow time.Duration
	// MaxSASRuns code comparisons that revealed the PC's nonce but did not
	// end in a pairing (mismatch, refusal, timeout, disconnect), from any
	// address, stop the listener with ErrSASLimit: each one is a fresh
	// 1-in-a-million guess for a man in the middle (default 3, §4.3).
	MaxSASRuns int
	// StartTimeout closes a pairing connection with 1001 that hasn't taken
	// its next step this long after connecting or after its last step
	// (default 20 seconds, §4.1), so a stray client can't hold the one
	// pairing slot.
	StartTimeout time.Duration
	// IdleTimeout replaces StartTimeout while the users compare codes
	// (default 5 minutes, the time §4.1 gives users to answer). Only
	// requests that advance the pairing restart the timer.
	IdleTimeout time.Duration

	// Logf, if set, receives progress messages meant for the user.
	Logf func(format string, args ...any)
}

var (
	ErrExpired  = errors.New("pairing expired: no phone paired within the time limit")
	ErrSASLimit = errors.New("pairing stopped: too many code comparisons failed")
)

// Listener serves one pairing at a time and stops after the first
// successful one.
type Listener struct {
	opts Options
	pin  string

	ctx     context.Context // done when Serve stops
	expired chan struct{}   // closed on expiry, before ctx is cancelled

	// failures counts failed attempts by remote host (§4.1). It has its
	// own lock.
	failures *hostlimit.Limiter

	mu       sync.Mutex
	secret   string
	busy     bool
	sasRuns  int           // failed SAS runs that revealed Npc
	sasLimit chan struct{} // closed when sasRuns reaches MaxSASRuns
	result   chan Paired   // buffered(1)
	wg       sync.WaitGroup
}

func New(opts Options) (*Listener, error) {
	if opts.Prompt == nil || opts.Save == nil {
		return nil, errors.New("pairing: Prompt and Save are required")
	}
	pin, err := security.CertPin(opts.Cert)
	if err != nil {
		return nil, err
	}
	if opts.Secret == "" {
		opts.Secret = security.NewSecret()
	}
	if opts.Expiry <= 0 {
		opts.Expiry = 5 * time.Minute
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = 5
	}
	if opts.FailureWindow <= 0 {
		opts.FailureWindow = time.Minute
	}
	if opts.MaxSASRuns <= 0 {
		opts.MaxSASRuns = 3
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 20 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Listener{
		opts: opts, pin: pin, secret: opts.Secret, result: make(chan Paired, 1), expired: make(chan struct{}),
		failures: hostlimit.New(opts.MaxFailures, opts.FailureWindow), sasLimit: make(chan struct{}),
	}, nil
}

func (l *Listener) Secret() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.secret
}

func (l *Listener) rotateSecret() {
	if l.ctx.Err() != nil {
		return
	}
	select {
	case <-l.expired:
		return
	default:
	}
	l.mu.Lock()
	l.secret = security.NewSecret()
	secret := l.secret
	l.mu.Unlock()
	l.logf("the QR code was used without pairing; it no longer works, and a new one follows")
	if l.opts.NewSecret != nil {
		l.opts.NewSecret(secret)
	}
}

func (l *Listener) Pin() string { return l.pin }

func (l *Listener) logf(format string, args ...any) {
	if l.opts.Logf != nil {
		l.opts.Logf(format, args...)
	}
}

// Serve wraps ln in TLS and runs until a phone has paired, the listener
// expires (ErrExpired), too many SAS runs fail (ErrSASLimit), or ctx is
// done. It closes ln.
func (l *Listener) Serve(ctx context.Context, ln net.Listener) (Paired, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.ctx = ctx

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, l.serveWS)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Phones that abort the handshake after a pin check, port scanners
		// and the like are not worth a log line each.
		ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	tlsLn := tls.NewListener(ln, security.ServerTLSConfig(l.opts.Cert))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(tlsLn) }()

	expiry := time.NewTimer(l.opts.Expiry)
	defer expiry.Stop()
	var (
		res Paired
		err error
	)
	select {
	case res = <-l.result:
	case <-expiry.C:
		err = ErrExpired
		close(l.expired) // sessions answer pending requests with -32024
	case <-l.sasLimit:
		err = ErrSASLimit
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	cancel()
	// Give sessions a moment to send their last answer and close frame.
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	srv.Close()
	return res, err
}

// sasFailed is called for a SAS run that revealed Npc without pairing; it
// stops the listener after MaxSASRuns of them (§4.3).
func (l *Listener) sasFailed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sasRuns++
	if l.sasRuns == l.opts.MaxSASRuns {
		close(l.sasLimit)
	}
}

func (l *Listener) sasExhausted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sasRuns >= l.opts.MaxSASRuns
}

func (l *Listener) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Phones are not browsers and send no cookies; the Origin check
		// protects nothing here.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	l.wg.Add(1)
	defer l.wg.Done()
	defer conn.CloseNow()

	if l.failures.Blocked(hostlimit.Host(r.RemoteAddr)) {
		conn.Close(websocket.StatusTryAgainLater, "too many failed pairing attempts, try again in a minute")
		return
	}
	if !l.claim(r.RemoteAddr) {
		conn.Close(websocket.StatusTryAgainLater, reasonBusy)
		return
	}

	l.logf("phone connected from %s", r.RemoteAddr)
	s := &session{l: l, conn: conn, remote: r.RemoteAddr, slot: true}
	s.run()
}

const reasonBusy = "another pairing is in progress"

// claim takes the pairing slot for a phone at remote, if it is free.
func (l *Listener) claim(remote string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy {
		// Two phones at once may mean someone else is using the QR code.
		l.logf("refused a second phone from %s while a pairing is in progress", remote)
		return false
	}
	l.busy = true
	return true
}

func (l *Listener) release() {
	l.mu.Lock()
	l.busy = false
	l.mu.Unlock()
}
