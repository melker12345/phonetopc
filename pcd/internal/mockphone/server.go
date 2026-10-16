// Package mockphone implements a mock phonetopc phone server: the full
// protocol (spec/PROTOCOL.md) over WebSocket, in dev mode or in secure mode
// with tokens (spec/SECURITY.md §5), backed by synthetic but plausible
// sensor data, with notifications and panels (spec/PANELS.md) and a
// stand-in for the phone's user. It is used by the phonetopc-mock command and by
// tests, so that clients can be developed without a phone.
package mockphone

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/hostlimit"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

const ServerName = "phonetopc-mock"

// Direction says which way a frame went, for Options.Tap.
type Direction string

const (
	ClientToServer Direction = "c2s"
	ServerToClient Direction = "s2c"
)

// Options configures a Server. Zero values select the defaults noted on each
// field.
type Options struct {
	// Device is reported in hello. Default: id "6d6f636b00000001", name
	// "phonetopc mock", platform "android".
	Device protocol.DeviceInfo

	// Unavailable lists catalog sources reported as available:false, for
	// which subscribe and get fail with source_unavailable.
	Unavailable []string

	// TapEvery is the interval of synthetic ui/tile_tap events. Default 10s;
	// negative disables them (taps can still be injected with Server.Tap).
	TapEvery time.Duration
	// LightEvery, ProximityEvery and BatteryEvery are the mean intervals
	// between changes of those sources. Defaults 4s, 15s, 60s; negative
	// disables changes.
	LightEvery, ProximityEvery, BatteryEvery time.Duration
	// LocationEvery is the interval between location fixes. Default 1s;
	// negative disables new fixes (the first one stays current).
	LocationEvery time.Duration
	// NFCEvery is the interval of synthetic nfc/tag scans. Default 20s;
	// negative disables them (scans can still be injected with
	// Server.ScanTag).
	NFCEvery time.Duration

	// PingInterval and PingTimeout control WebSocket keepalive: a ping is sent
	// every PingInterval and the session is closed if the pong doesn't arrive
	// within PingTimeout. Defaults 15s and 30s (spec §3).
	PingInterval, PingTimeout time.Duration

	// QueueSize is the per-session outgoing message queue length. Events of
	// continuous sources may fill only three quarters of it; beyond that
	// they are dropped (seq gaps). The rest is reserved for on_change events
	// and responses, and an overflow of on_change events closes the session
	// with 1008 (spec §5). Default 1024.
	QueueSize int

	// Tap, if set, is called with every text frame received and sent, in the
	// order they were read and written. It must not block or retain frame.
	Tap func(dir Direction, frame []byte)

	// Logf, if set, receives log messages.
	Logf func(format string, args ...any)

	// Seed seeds the simulation's random numbers. Default: random.
	Seed uint64

	// Authenticate, if set, puts the server in secure mode (SECURITY.md
	// §5): hello must carry auth.token, which Authenticate maps to the
	// paired PC. Serving TLS is up to the caller (see SecureListener).
	Authenticate func(token string) (protocol.PCInfo, bool)
	// HelloTimeout closes secure sessions without a successful hello after
	// this long with 1008. Default 10s; negative disables it.
	HelloTimeout time.Duration
	// ApprovalKey, if set, returns the phone's approval key (APPROVAL.md
	// §2), or nil if it can't do approvals now. It is called at every
	// secure hello, so a rotated key applies to new sessions. Without it, or
	// in dev mode, the phone has no capability approval.
	ApprovalKey func() *ecdsa.PrivateKey
	// DecryptKey is ApprovalKey for the decryption key (DECRYPT.md §2).
	DecryptKey func() *rsa.PrivateKey
	// Approve plays the user for approval and decrypt requests. Default:
	// ApproveAll.
	Approve Approver
	// TamperApprovals corrupts the signature of every approval it gives: a
	// test hook for clients, which must report an error, not approval.
	TamperApprovals bool

	// NoNotify and NoPanels play a user who hasn't allowed notifications or
	// panels (PANELS.md §1): the methods fail with not_allowed. A phone
	// has both off by default; the mock, a development tool, has them on.
	// Server.AllowNotify and Server.AllowPanels change them later.
	NoNotify, NoPanels bool
	// UserEvery is the interval of the simulated user (simulate.go), who
	// opens panels, uses their widgets and answers notifications. Default
	// 4s; negative disables it: then no panel becomes visible unless a test
	// drives Server.User.
	UserEvery time.Duration
	// PanelExpiry is how long a detached panel is kept (PANELS.md §4.1).
	// Default 10 minutes.
	PanelExpiry time.Duration
	// NotifyPerMinute limits notify per PC (PANELS.md §3.1). Default 60;
	// negative disables the limit.
	NotifyPerMinute int
	// CanvasPoints is how many stroke points a canvas keeps before it is
	// full (PANELS.md §7.3). Default 100000.
	CanvasPoints int

	// MaxAuthFailures failed hellos per minute from one address make the
	// server close further connections from it with 1013 (SECURITY.md §5),
	// even with a valid token, for the rest of
	// the minute. Default 5; negative disables the limit.
	MaxAuthFailures int
}

func (o *Options) setDefaults() {
	if o.Device.ID == "" {
		o.Device.ID = "6d6f636b00000001"
	}
	if o.Device.Name == "" {
		o.Device.Name = "phonetopc mock"
	}
	if o.Device.Platform == "" {
		o.Device = protocol.DeviceInfo{
			ID: o.Device.ID, Name: o.Device.Name,
			Manufacturer: "phonetopc", Model: "Mock", Platform: "android", PlatformVersion: "34",
		}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.TapEvery, 10*time.Second)
	def(&o.LightEvery, 4*time.Second)
	def(&o.ProximityEvery, 15*time.Second)
	def(&o.BatteryEvery, 60*time.Second)
	def(&o.LocationEvery, time.Second)
	def(&o.NFCEvery, 20*time.Second)
	def(&o.PingInterval, 15*time.Second)
	def(&o.PingTimeout, 30*time.Second)
	def(&o.HelloTimeout, 10*time.Second)
	def(&o.UserEvery, 4*time.Second)
	def(&o.PanelExpiry, protocol.DetachedPanelTimeout*time.Second)
	if o.MaxAuthFailures == 0 {
		o.MaxAuthFailures = 5
	}
	if o.NotifyPerMinute == 0 {
		o.NotifyPerMinute = 60
	}
	if o.CanvasPoints <= 0 {
		o.CanvasPoints = protocol.MinCanvasPoints
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 1024
	}
	if o.Seed == 0 {
		o.Seed = rand.Uint64()
	}
}

// Server is a mock phone. Create it with New, serve Handler, and Close it
// when done.
type Server struct {
	opts   Options
	start  time.Time // monotonic origin for ts_mono_ns
	world  *world
	panels *panelHub

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[*session]struct{}
	seedSeq  uint64

	// authFailures counts failed hellos per remote host (SECURITY.md §5).
	authFailures *hostlimit.Limiter

	// The approval shown to the user, and those of other PCs waiting
	// their turn (APPROVAL.md §4).
	approvalShown   *approvalSlot
	approvalWaiting []*approvalSlot
}

// New creates a server and starts its simulation.
func New(opts Options) *Server {
	opts.setDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:     opts,
		start:    time.Now(),
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[*session]struct{}),

		authFailures: hostlimit.New(opts.MaxAuthFailures, time.Minute),
	}
	s.world = newWorld(s)
	s.panels = newPanelHub(s)
	s.wg.Go(func() { s.world.run(ctx, s.newRand()) })
	if opts.UserEvery > 0 {
		s.wg.Go(func() { s.panels.simulate(ctx, opts.UserEvery) })
	}
	return s
}

// Close stops the simulation and closes all sessions.
func (s *Server) Close() {
	s.mu.Lock()
	s.cancel() // under mu so serveWS can't register a session after this
	s.mu.Unlock()
	s.wg.Wait()
}

// Handler returns the HTTP handler serving protocol.WSPath.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.WSPath, s.serveWS)
	return mux
}

func (s *Server) DeviceID() string { return s.opts.Device.ID }

func (s *Server) Secure() bool { return s.opts.Authenticate != nil }

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Clients authenticate with a token in hello, not with cookies, and
		// the mock serves fake data, so allow any Origin (browser-based
		// clients).
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logf("mock: accept from %s: %v", r.RemoteAddr, err)
		return
	}
	if s.Secure() && s.authFailures.Blocked(hostlimit.Host(r.RemoteAddr)) {
		s.logf("mock: %s: too many failed hellos, closing with 1013", r.RemoteAddr)
		conn.Close(websocket.StatusTryAgainLater, "too many failed hellos, try again later")
		return
	}
	sess := newSession(s, conn, r.RemoteAddr)
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	s.sessions[sess] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sess)
		s.mu.Unlock()
		s.wg.Done()
	}()
	sess.run()
}

// Recheck re-validates the token of every authenticated session and closes
// those whose PC was revoked (or re-paired with a new token) with 1008
// (SECURITY.md §5). It returns how many sessions it closed.
func (s *Server) Recheck() int {
	if !s.Secure() {
		return 0
	}
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	n := 0
	for _, sess := range sessions {
		pc, token, ok := sess.auth()
		if !ok {
			continue
		}
		if now, valid := s.opts.Authenticate(token); !valid || now.ID != pc.ID {
			s.logf("mock: %s: PC %q was revoked, closing", sess.remote, pc.Name)
			sess.closeWith(websocket.StatusPolicyViolation, "this PC was revoked")
			n++
		}
	}
	return n
}

func (s *Server) newRand() *rand.Rand {
	s.mu.Lock()
	s.seedSeq++
	n := s.seedSeq
	s.mu.Unlock()
	return rand.New(rand.NewPCG(s.opts.Seed, n))
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// sensors returns the catalog with availability applied. notification and
// panel are available while the user allows them (PANELS.md §1).
func (s *Server) sensors() []protocol.SensorInfo {
	s.panels.mu.Lock()
	notify, panels := s.panels.allowNotify, s.panels.allowPanels
	s.panels.mu.Unlock()
	cat := protocol.Catalog()
	for i := range cat {
		switch src := cat[i].Source; {
		case slices.Contains(s.opts.Unavailable, src),
			src == protocol.SourceNotification && !notify,
			src == protocol.SourcePanel && !panels:
			cat[i].Available = false
		}
	}
	return cat
}

func (s *Server) sensor(source string) (protocol.SensorInfo, bool) {
	for _, info := range s.sensors() {
		if info.Source == source {
			return info, true
		}
	}
	return protocol.SensorInfo{}, false
}

func (s *Server) helloResult(pc *protocol.PCInfo, approval *protocol.ApprovalInfo, decrypt *protocol.DecryptInfo) protocol.HelloResult {
	h := protocol.HelloResult{
		Protocol:     protocol.Version,
		Server:       protocol.ServerInfo{Name: ServerName, Version: version.Version},
		Device:       s.opts.Device,
		Capabilities: []string{protocol.CapabilityEvents, protocol.CapabilityGet, protocol.CapabilityNotify, protocol.CapabilityPanels},
		PC:           pc,
		Approval:     approval,
		Decrypt:      decrypt,
	}
	if approval != nil {
		h.Capabilities = append(h.Capabilities, protocol.CapabilityApproval)
	}
	if decrypt != nil {
		h.Capabilities = append(h.Capabilities, protocol.CapabilityDecrypt)
	}
	return h
}

// reading builds a reading of source with the catalog's type and unit,
// timestamped at now.
func (s *Server) reading(source string, value any, now time.Time) (protocol.Reading, error) {
	info, ok := protocol.LookupSensor(source)
	if !ok {
		return protocol.Reading{}, fmt.Errorf("no catalog entry for %q", source)
	}
	r := protocol.Reading{
		Source:   source,
		Type:     info.Types[0],
		Unit:     info.Unit,
		TS:       now.UnixMilli(),
		TSMonoNS: now.Sub(s.start).Nanoseconds(),
		Device:   s.opts.Device.ID,
	}
	return r, r.SetValue(value)
}

func (s *Server) sampleContinuous(source string, now time.Time, rng *rand.Rand) (protocol.Reading, error) {
	t := now.Sub(s.start).Seconds()
	var v any
	switch source {
	case protocol.SourceGyro:
		v = gyro(t, rng)
	case protocol.SourceAccelerometer:
		v = accelerometer(t, rng)
	case protocol.SourceGameRotationVector:
		v = gameRotationVector(t)
	case protocol.SourceRotationVector:
		v = rotationVector(t)
	case protocol.SourceMagnetometer:
		v = magnetometer(t, rng)
	case protocol.SourceCompass:
		v = compass(t, rng)
	case protocol.SourceBarometer:
		v = barometer(t, rng)
	default:
		return protocol.Reading{}, fmt.Errorf("%q is not a continuous source", source)
	}
	return s.reading(source, v, now)
}
