// Package mockphone implements a mock phonetopc phone server: the full
// draft-0 protocol (spec/PROTOCOL.md) over WebSocket, backed by synthetic but
// plausible sensor data. It is used by the phonetopc-mock command and by
// tests, so that clients can be developed without a phone.
package mockphone

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// Version is the mock's server version reported in hello.
const Version = "0.1.0"

// ServerName is the mock's server name reported in hello.
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
	// Device is reported in hello. Default: id "mock-0001", name
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
}

func (o *Options) setDefaults() {
	if o.Device.ID == "" {
		o.Device.ID = "mock-0001"
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
	def(&o.PingInterval, 15*time.Second)
	def(&o.PingTimeout, 30*time.Second)
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
	opts  Options
	start time.Time // monotonic origin for ts_mono_ns
	world *world

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[*session]struct{}
	seedSeq  uint64
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
	}
	s.world = newWorld(s)
	s.wg.Go(func() { s.world.run(ctx, s.newRand()) })
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

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Draft 0 is unauthenticated dev mode, and the mock serves fake data,
		// so allow any Origin (browser-based clients).
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logf("mock: accept from %s: %v", r.RemoteAddr, err)
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

// sensors returns the catalog with availability applied.
func (s *Server) sensors() []protocol.SensorInfo {
	cat := protocol.Catalog()
	for i := range cat {
		if slices.Contains(s.opts.Unavailable, cat[i].Source) {
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

func (s *Server) helloResult() protocol.HelloResult {
	return protocol.HelloResult{
		Protocol:     protocol.Version,
		Server:       protocol.ServerInfo{Name: ServerName, Version: Version},
		Device:       s.opts.Device,
		Capabilities: []string{protocol.CapabilityEvents, protocol.CapabilityGet},
	}
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

// sampleContinuous samples a continuous source at now.
func (s *Server) sampleContinuous(source string, now time.Time, rng *rand.Rand) (protocol.Reading, error) {
	t := now.Sub(s.start).Seconds()
	var v []float64
	switch source {
	case protocol.SourceGyro:
		v = gyro(t, rng)
	case protocol.SourceAccelerometer:
		v = accelerometer(t, rng)
	case protocol.SourceGameRotationVector:
		v = gameRotationVector(t)
	default:
		return protocol.Reading{}, fmt.Errorf("%q is not a continuous source", source)
	}
	return s.reading(source, v, now)
}
