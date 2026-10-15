package mockphone

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

type subscription struct {
	sess     *session
	id       string
	info     protocol.SensorInfo
	types    []string
	rate     float64 // effective rate; 0 for on_change
	minDelta float64

	mu      sync.Mutex
	started bool               // set by start, once the subscribe response is queued
	pending []protocol.Reading // emitted before start, queued by start
	closed  bool
	seq     uint64
	last    json.RawMessage // value of the last event actually queued
	stopFn  func()
}

// begin starts producing events. Until start, emit keeps them back, so the
// subscribe response precedes them and none is lost in between.
func (sub *subscription) begin() {
	switch sub.info.Source {
	case protocol.SourcePanel, protocol.SourceNotification:
		return // delivered by the panel hub (session.deliver), not the world
	}
	if sub.info.Mode == protocol.ModeContinuous {
		stop := make(chan struct{})
		sub.setStop(func() { close(stop) })
		go sub.generate(stop)
		return
	}
	sub.setStop(sub.sess.srv.world.listen(sub.info.Source, sub.emit))
}

func (sub *subscription) start() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	sub.started = true
	for _, r := range sub.pending {
		if !sub.closed {
			sub.queue(r)
		}
	}
	sub.pending = nil
}

func (sub *subscription) setStop(f func()) {
	sub.mu.Lock()
	if sub.closed { // closed before it started (session ended)
		sub.mu.Unlock()
		f()
		return
	}
	sub.stopFn = f
	sub.mu.Unlock()
}

func (sub *subscription) generate(stop <-chan struct{}) {
	period := time.Duration(float64(time.Second) / sub.rate)
	rng := sub.sess.srv.newRand()
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-sub.sess.ctx.Done():
			return
		case now := <-t.C:
			r, err := sub.sess.srv.sampleContinuous(sub.info.Source, now, rng)
			if err != nil {
				sub.sess.srv.logf("mock: %v", err)
				return
			}
			sub.emit(r)
		}
	}
}

// emit filters, numbers and queues one reading. It never blocks.
func (sub *subscription) emit(r protocol.Reading) {
	if !slices.Contains(sub.types, r.Type) {
		return
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	switch {
	case sub.closed:
	case !sub.started:
		sub.pending = append(sub.pending, r)
	default:
		sub.queue(r)
	}
}

// queue numbers and queues r. sub.mu must be held.
func (sub *subscription) queue(r protocol.Reading) {
	if sub.minDelta > 0 && sub.last != nil {
		d, err := protocol.ValueDistance(sub.info.Shape, sub.last, r.Value)
		if err == nil && d < sub.minDelta {
			return // filtered: not sent, so no seq consumed
		}
	}
	ev := &protocol.Event{Subscription: sub.id, Seq: sub.seq, Reading: r}
	b, err := json.Marshal(protocol.NewEventNotification(ev))
	if err != nil {
		sub.sess.srv.logf("mock: marshal event: %v", err)
		return
	}
	if sub.sess.trySend(b, sub.info.Mode == protocol.ModeContinuous) {
		sub.seq++
		sub.last = r.Value
		return
	}
	// Queue full: the client isn't keeping up (spec §5, backpressure).
	if sub.info.Mode == protocol.ModeContinuous {
		sub.seq++ // dropped; the client sees a seq gap
		return
	}
	sub.sess.srv.logf("mock: %s: on_change queue overflow, closing", sub.sess.remote)
	sub.sess.closeWith(websocket.StatusPolicyViolation, fmt.Sprintf("event queue overflow (%s)", sub.info.Source))
}

// close stops the subscription. After it returns, emit queues nothing more.
func (sub *subscription) close() {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return
	}
	sub.closed = true
	stop := sub.stopFn
	sub.mu.Unlock()
	if stop != nil {
		stop()
	}
}
