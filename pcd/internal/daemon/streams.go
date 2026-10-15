package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// SourceDaemon is the source of the events phonetopcd adds to a stream
// itself: connected, disconnected and error.
const SourceDaemon = "phonetopcd"

// Event types of SourceDaemon.
const (
	TypeConnected    = "connected"    // the phone is connected again; value: the device
	TypeDisconnected = "disconnected" // the phone connection was lost; value: {"error"}
	TypeError        = "error"        // the stream ends; value: {"error"}
)

// StreamOptions are the subscribe options of a stream, applied to each of
// its sources.
type StreamOptions struct {
	Type     string
	RateHz   float64
	MinDelta float64
}

// feedKey identifies one phone subscription. Streams asking for the same
// thing share it.
type feedKey struct {
	Source string
	StreamOptions
}

// feed is one phone subscription and the streams it delivers to.
type feed struct {
	key   feedKey
	ready chan struct{} // closed when the first subscribe has an answer
	err   error         // the first subscribe's error, set before ready closes

	// guarded by Daemon.mu
	streams map[*stream]struct{}
	sub     *client.Subscription // nil while disconnected
	mode    protocol.Mode
}

// stream is one local program's event stream. Events arrive on ch as
// encoded JSON objects; done is closed when the daemon ends the stream,
// and reason then says why.
type stream struct {
	ch     chan []byte
	done   chan struct{}
	reason string
	once   sync.Once
	feeds  []*feed // guarded by Daemon.mu
}

func (s *stream) end(reason string) {
	s.once.Do(func() {
		s.reason = reason
		close(s.done)
	})
}

// send queues an event without blocking. A stream that can't keep up
// loses events of continuous sources, which shows as a seq gap; it is
// ended rather than lose an event of an on_change source, as the phone
// does (PROTOCOL.md §5).
func (s *stream) send(line []byte, mode protocol.Mode) {
	select {
	case s.ch <- line:
	default:
		if mode != protocol.ModeContinuous {
			s.end("this stream was too slow to read its events; events would have been lost")
		}
	}
}

// localEvent is an event as local programs see it: the phone's event
// without the subscription id, which only means something on the phone
// connection.
type localEvent struct {
	Seq uint64 `json:"seq"`
	protocol.Reading
}

type daemonEvent struct {
	Source string `json:"source"`
	Type   string `json:"type"`
	Value  any    `json:"value"`
	TS     int64  `json:"ts"`
}

func encodeDaemonEvent(typ string, value any) []byte {
	b, _ := json.Marshal(daemonEvent{Source: SourceDaemon, Type: typ, Value: value, TS: time.Now().UnixMilli()})
	return b
}

func errorValue(msg string) map[string]string { return map[string]string{"error": msg} }

// openStream subscribes a new stream to sources. While the phone isn't
// connected the stream opens anyway, starts with a disconnected event, and
// is subscribed when the phone connects. On success the caller must
// closeStream it.
func (d *Daemon) openStream(ctx context.Context, sources []string, opts StreamOptions) (*stream, error) {
	s := &stream{ch: make(chan []byte, d.opts.StreamBuffer), done: make(chan struct{})}
	d.mu.Lock()
	d.streams[s] = struct{}{}
	if d.cl == nil {
		s.send(encodeDaemonEvent(TypeDisconnected, errorValue(d.unavailableReason())), protocol.ModeContinuous)
	}
	d.mu.Unlock()

	for _, src := range sources {
		key := feedKey{Source: src, StreamOptions: opts}
		// Registering the feed and reading the connection under one lock
		// means exactly one of us and connected subscribes a new feed.
		d.mu.Lock()
		cl := d.cl
		f, ok := d.feeds[key]
		if !ok {
			f = &feed{key: key, ready: make(chan struct{}), streams: make(map[*stream]struct{})}
			d.feeds[key] = f
			if cl == nil {
				close(f.ready) // connected subscribes it
			}
		}
		f.streams[s] = struct{}{}
		s.feeds = append(s.feeds, f)
		d.mu.Unlock()

		if !ok && cl != nil {
			if f.err = d.startFeed(ctx, cl, f); f.err != nil {
				d.mu.Lock()
				if d.feeds[key] == f {
					delete(d.feeds, key)
				}
				d.mu.Unlock()
			}
			close(f.ready)
		} else {
			select {
			case <-f.ready:
			case <-ctx.Done():
				d.closeStream(s)
				return nil, ctx.Err()
			}
		}
		if f.err != nil {
			d.closeStream(s)
			return nil, f.err
		}
	}
	return s, nil
}

// closeStream detaches s from its feeds and unsubscribes the feeds nobody
// listens to anymore.
func (d *Daemon) closeStream(s *stream) {
	var unsub []*client.Subscription
	d.mu.Lock()
	delete(d.streams, s)
	for _, f := range s.feeds {
		delete(f.streams, s)
		if len(f.streams) == 0 && d.feeds[f.key] == f {
			delete(d.feeds, f.key)
			if f.sub != nil {
				unsub = append(unsub, f.sub)
				f.sub = nil
			}
		}
	}
	s.feeds = nil
	d.mu.Unlock()
	s.end("closed")
	for _, sub := range unsub {
		go unsubscribe(sub)
	}
}

func unsubscribe(sub *client.Subscription) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = sub.Unsubscribe(ctx)
}

func (d *Daemon) startFeed(ctx context.Context, cl *client.Client, f *feed) error {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sub, err := cl.Subscribe(sctx, f.key.Source, client.SubscribeOptions{
		Type: f.key.Type, RateHz: f.key.RateHz, MinDelta: f.key.MinDelta,
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	if d.feeds[f.key] != f {
		// Everyone left while we subscribed.
		d.mu.Unlock()
		go unsubscribe(sub)
		return nil
	}
	f.sub, f.mode = sub, sub.Mode
	d.mu.Unlock()
	go d.pump(f, sub)
	return nil
}

func (d *Daemon) pump(f *feed, sub *client.Subscription) {
	for ev := range sub.Events() {
		if ev.Source == protocol.SourcePanel {
			d.notePanelEvent(ev)
		}
		line, err := json.Marshal(localEvent{Seq: ev.Seq, Reading: ev.Reading})
		if err != nil {
			continue
		}
		d.mu.Lock()
		if f.sub == sub {
			for s := range f.streams {
				s.send(line, f.mode)
			}
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	if f.sub == sub {
		f.sub = nil
	}
	d.mu.Unlock()
}

// failFeed ends the streams of a feed that couldn't be subscribed again
// after a reconnect, e.g. because the user turned the sensor off.
func (d *Daemon) failFeed(f *feed, err error) {
	d.mu.Lock()
	if d.feeds[f.key] == f {
		delete(d.feeds, f.key)
	}
	var streams []*stream
	for s := range f.streams {
		streams = append(streams, s)
	}
	d.mu.Unlock()
	msg := "subscribing to " + f.key.Source + " again after the reconnect failed: " + err.Error()
	for _, s := range streams {
		s.end(msg)
	}
}

func (d *Daemon) broadcastStatus() {
	d.mu.Lock()
	defer d.mu.Unlock()
	var line []byte
	if d.cl != nil {
		line = encodeDaemonEvent(TypeConnected, d.cl.Server().Device)
	} else {
		line = encodeDaemonEvent(TypeDisconnected, errorValue(d.unavailableReason()))
	}
	for s := range d.streams {
		s.send(line, protocol.ModeContinuous) // dropped if the stream is full
	}
}

func (d *Daemon) closeAllStreams() {
	d.mu.Lock()
	var streams []*stream
	for s := range d.streams {
		streams = append(streams, s)
	}
	d.mu.Unlock()
	for _, s := range streams {
		s.end("phonetopcd is shutting down")
	}
}
