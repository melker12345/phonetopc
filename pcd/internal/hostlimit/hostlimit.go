// Package hostlimit throttles failed attempts per remote host: the pairing
// listener's failed pairing attempts (SECURITY.md §4.1) and the mock
// phone's failed hellos (SECURITY.md §5) both close further connections
// from an address once it has failed too often within a sliding window.
package hostlimit

import (
	"net"
	"slices"
	"sync"
	"time"
)

// The zero Limiter is not usable; create one with New. It is safe for
// concurrent use.
type Limiter struct {
	max    int
	window time.Duration

	mu       sync.Mutex
	failures map[string][]time.Time
}

// New returns a limiter that blocks a host after max failures within
// window. A negative max disables it: nothing is recorded or blocked.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, failures: make(map[string][]time.Time)}
}

// Blocked reports whether host failed max times or more within the window.
// It also forgets the host's failures that have left the window, so hosts
// that stop failing don't accumulate.
func (l *Limiter) Blocked(host string) bool {
	if l.max < 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	f := slices.DeleteFunc(l.failures[host], func(t time.Time) bool { return t.Before(cutoff) })
	if len(f) == 0 {
		delete(l.failures, host)
	} else {
		l.failures[host] = f
	}
	return len(f) >= l.max
}

func (l *Limiter) Fail(host string) {
	if l.max < 0 {
		return
	}
	l.mu.Lock()
	l.failures[host] = append(l.failures[host], time.Now())
	l.mu.Unlock()
}

// Host returns the host part of a remote address such as
// http.Request.RemoteAddr, the key limits are counted by: a client
// reconnecting from a new port is the same host. An address without a port
// is returned unchanged.
func Host(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
