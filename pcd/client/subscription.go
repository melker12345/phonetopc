package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// SubscribeOptions are the optional subscribe parameters.
type SubscribeOptions struct {
	// Type restricts the subscription to one event type ("" = all types).
	Type string
	// RateHz requests a sample rate for continuous sensors (0 = the sensor's
	// default rate). The server clamps it; see Subscription.RateHz.
	RateHz float64
	// MinDelta asks the server to suppress events whose value differs from the
	// last sent one by less than this (0 = no filtering).
	MinDelta float64
}

// Subscription is an active subscription. Events arrive on Events() until
// the subscription is unsubscribed or the connection is lost.
type Subscription struct {
	// ID is the server-assigned subscription id.
	ID string
	// Source and Types are what the server subscribed to.
	Source string
	Types  []string
	// Mode is the source's mode as reported by the server. Continuous
	// sources are sampled at RateHz; on_change sources emit when their value
	// changes, starting with the current value if the source has one.
	Mode Mode
	// RateHz is the effective rate (0 for on_change sources).
	RateHz float64

	c       *Client
	ch      chan Event
	dropped atomic.Uint64
	err     error // guarded by c.mu; set when ch is closed
	closed  bool  // guarded by c.mu
}

// Events returns the event channel. It is closed after Unsubscribe or when
// the connection is lost; Err then tells which.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped returns the number of events dropped locally because the Events
// channel was full.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Err returns nil while the subscription is active or after a successful
// Unsubscribe, and the connection error if the connection was lost.
func (s *Subscription) Err() error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.err
}

// Subscribe subscribes to source. Events start flowing on the returned
// subscription's Events channel immediately.
//
// If ctx is cancelled before the server answers, a subscription the server
// still creates is unsubscribed in the background.
func (c *Client) Subscribe(ctx context.Context, source string, opts SubscribeOptions) (*Subscription, error) {
	p := protocol.SubscribeParams{Source: source, Type: opts.Type}
	if opts.RateHz != 0 {
		p.RateHz = &opts.RateHz
	}
	if opts.MinDelta != 0 {
		p.MinDelta = &opts.MinDelta
	}
	var sub *Subscription
	cl := &call{
		// Register the subscription in the read loop, before any following
		// event frame is processed, so no early events are lost.
		onResult: func(raw json.RawMessage) error {
			var res protocol.SubscribeResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			sub = &Subscription{
				ID: res.Subscription, Source: res.Source, Types: res.Types, Mode: res.Mode, RateHz: res.RateHz,
				c: c, ch: make(chan Event, c.opts.eventBuffer),
			}
			c.mu.Lock()
			if c.err == nil {
				c.subs[sub.ID] = sub
			} else {
				sub.closed, sub.err = true, c.err
				close(sub.ch)
			}
			c.mu.Unlock()
			return nil
		},
		onAbandoned: func(json.RawMessage) {
			if sub == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = sub.Unsubscribe(ctx)
		},
	}
	if _, err := c.roundTrip(ctx, protocol.MethodSubscribe, p, cl); err != nil {
		return nil, err
	}
	return sub, nil
}

// Unsubscribe ends the subscription and closes its Events channel. Events
// already buffered in the channel can still be read. Calling it again is a
// no-op.
func (s *Subscription) Unsubscribe(ctx context.Context) error {
	c := s.c
	c.mu.Lock()
	if s.closed {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	cl := &call{onResult: func(json.RawMessage) error {
		s.close(nil)
		return nil
	}}
	_, err := c.roundTrip(ctx, protocol.MethodUnsubscribe, protocol.UnsubscribeParams{Subscription: s.ID}, cl)
	if errors.Is(err, &protocol.Error{Code: protocol.CodeUnknownSubscription}) {
		s.close(nil) // the server doesn't know it either; we're consistent now
		return nil
	}
	return err
}

func (s *Subscription) close(err error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.err = err
	delete(c.subs, s.ID)
	close(s.ch)
}
