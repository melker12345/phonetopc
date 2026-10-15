// Package daemon is the core of phonetopcd: it keeps one connection to the
// paired phone open, reconnecting when it drops, and offers it to local
// programs as a plain HTTP API (spec/LOCAL-API.md). Programs on the PC then
// need no pairing, TLS pinning or JSON-RPC: a GET streams events, a POST
// posts a notification or asks for an approval.
//
// Several programs share the connection. Identical event streams share one
// phone subscription, which ends when the last program stops listening.
package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

type Dialer func(ctx context.Context) (*client.Client, error)

type Options struct {
	// Required.
	Dial Dialer
	// Optional.
	Logf func(format string, args ...any)
	// MinBackoff and MaxBackoff bound the wait between reconnect attempts.
	// Defaults 1s and 10s.
	MinBackoff, MaxBackoff time.Duration
	// StreamBuffer is the number of events queued for each local stream.
	// Default 1024.
	StreamBuffer int
	// Explain turns a connection error into a message for the status
	// endpoint. Default: err.Error().
	Explain func(error) string
	// Verbose logs every failed connection attempt; otherwise only those
	// that fail differently from the one before.
	Verbose bool
}

type Daemon struct {
	opts Options

	mu      sync.Mutex
	cl      *client.Client // nil while disconnected
	lastErr error          // why the last connection attempt or connection failed
	since   time.Time      // when cl was connected or lost
	changed chan struct{}  // closed and replaced whenever cl changes

	feeds   map[feedKey]*feed
	streams map[*stream]struct{}
	panels  map[string]int64 // panels shown through the daemon, by id: the last pseq seen
}

// It does not connect until Run.
func New(opts Options) *Daemon {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = max(10*time.Second, opts.MinBackoff)
	}
	if opts.StreamBuffer <= 0 {
		opts.StreamBuffer = 1024
	}
	if opts.Explain == nil {
		opts.Explain = func(err error) string { return err.Error() }
	}
	return &Daemon{
		opts:    opts,
		since:   time.Now(),
		changed: make(chan struct{}),
		feeds:   make(map[feedKey]*feed),
		streams: make(map[*stream]struct{}),
		panels:  make(map[string]int64),
	}
}

func (d *Daemon) logf(format string, args ...any) {
	if d.opts.Logf != nil {
		d.opts.Logf(format, args...)
	}
}

// Run keeps the phone connected until ctx is done, then closes the
// connection and every local stream.
func (d *Daemon) Run(ctx context.Context) {
	backoff := d.opts.MinBackoff
	lastLogged := ""
	for {
		cl, err := d.opts.Dial(ctx)
		if ctx.Err() != nil {
			if cl != nil {
				cl.Close()
			}
			break
		}
		if err != nil {
			d.setDisconnected(err)
			if msg := d.opts.Explain(err); d.opts.Verbose || msg != lastLogged {
				d.logf("phone: %s (retrying every %v or less)", msg, d.opts.MaxBackoff)
				lastLogged = msg
			}
			if !sleep(ctx, backoff) {
				break
			}
			backoff = min(backoff*2, d.opts.MaxBackoff)
			continue
		}
		backoff, lastLogged = d.opts.MinBackoff, ""
		h := cl.Server()
		d.logf("phone: connected to %s (%s)", h.Device.Name, h.Device.ID)
		d.connected(ctx, cl)

		select {
		case <-cl.Done():
		case <-ctx.Done():
		}
		cl.Close()
		err = cl.Err()
		if ctx.Err() != nil {
			break
		}
		if err == nil || errors.Is(err, client.ErrClosed) {
			err = errors.New("connection lost")
		}
		d.logf("phone: %s", d.opts.Explain(err))
		d.setDisconnected(err)
		d.broadcastStatus()
		if !sleep(ctx, d.opts.MinBackoff) {
			break
		}
	}
	d.setDisconnected(errors.New("phonetopcd is shutting down"))
	d.closeAllStreams()
}

func sleep(ctx context.Context, dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Daemon) setDisconnected(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cl != nil {
		d.since = time.Now()
	}
	d.cl, d.lastErr = nil, err
	close(d.changed)
	d.changed = make(chan struct{})
}

// connected installs cl, then restores what the previous connection had:
// the subscriptions of the open streams, and the ownership of the panels
// shown through the daemon (after the subscriptions, so the phone's replay
// of their strokes reaches the streams).
func (d *Daemon) connected(ctx context.Context, cl *client.Client) {
	d.mu.Lock()
	d.cl, d.lastErr, d.since = cl, nil, time.Now()
	close(d.changed)
	d.changed = make(chan struct{})
	feeds := make([]*feed, 0, len(d.feeds))
	for _, f := range d.feeds {
		feeds = append(feeds, f)
	}
	panels := make(map[string]int64, len(d.panels))
	for id, pseq := range d.panels {
		panels[id] = pseq
	}
	d.mu.Unlock()

	d.broadcastStatus()
	for _, f := range feeds {
		if err := d.startFeed(ctx, cl, f); err != nil && cl.Err() == nil {
			d.logf("phone: resubscribing %s: %v", f.key.Source, err)
			d.failFeed(f, err)
		}
	}
	for id, pseq := range panels {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := cl.AttachPanel(actx, id, pseq)
		cancel()
		if err != nil {
			d.logf("phone: reattaching panel %q: %v", id, err)
			if code, ok := protocol.ErrorCodeOf(err); ok && code == protocol.CodeUnknownPanel {
				d.forgetPanel(id)
			}
		}
	}
}

func (d *Daemon) phone() (*client.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cl != nil {
		return d.cl, nil
	}
	return nil, &unavailableError{reason: d.unavailableReason()}
}

// d.mu must be held.
func (d *Daemon) unavailableReason() string {
	if d.lastErr == nil {
		return "connecting to the phone"
	}
	return d.opts.Explain(d.lastErr)
}

type unavailableError struct{ reason string }

func (e *unavailableError) Error() string { return "phone not connected: " + e.reason }

func (d *Daemon) WaitConnected(ctx context.Context) error {
	for {
		d.mu.Lock()
		cl, ch := d.cl, d.changed
		d.mu.Unlock()
		if cl != nil {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Status is the body of GET /v1/status.
type Status struct {
	Connected    bool                 `json:"connected"`
	Since        int64                `json:"since"` // Unix ms of the last connect or disconnect
	Device       *protocol.DeviceInfo `json:"device,omitempty"`
	Capabilities []string             `json:"capabilities,omitempty"`
	Error        string               `json:"error,omitempty"` // why it isn't connected
	Streams      int                  `json:"streams"`
	Panels       []string             `json:"panels"`
}

func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Status{Since: d.since.UnixMilli(), Streams: len(d.streams), Panels: []string{}}
	for id := range d.panels {
		s.Panels = append(s.Panels, id)
	}
	slices.Sort(s.Panels)
	if d.cl != nil {
		h := d.cl.Server()
		dev := h.Device
		s.Connected, s.Device, s.Capabilities = true, &dev, h.Capabilities
	} else {
		s.Error = d.unavailableReason()
	}
	return s
}

// notePanel records a panel shown through the daemon, so it is reattached
// after a reconnect.
func (d *Daemon) notePanel(id string) {
	d.mu.Lock()
	d.panels[id] = -1
	d.mu.Unlock()
}

func (d *Daemon) forgetPanel(id string) {
	d.mu.Lock()
	delete(d.panels, id)
	d.mu.Unlock()
}

// notePanelEvent keeps track of the panels' pseq, and forgets a panel the
// user closed.
func (d *Daemon) notePanelEvent(ev client.Event) {
	p, err := ev.Panel()
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.panels[p.Panel]
	if !ok {
		return
	}
	if ev.Type == protocol.TypeState && p.State == protocol.StateClosed {
		delete(d.panels, p.Panel)
		return
	}
	if p.Pseq > last {
		d.panels[p.Panel] = p.Pseq
	}
}
