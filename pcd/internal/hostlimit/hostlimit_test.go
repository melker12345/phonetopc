package hostlimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New(2, time.Hour)
	l.Fail("a")
	if l.Blocked("a") {
		t.Fatal("blocked after one failure")
	}
	l.Fail("a")
	if !l.Blocked("a") || l.Blocked("b") {
		t.Fatal("want a blocked and b not after two failures of a")
	}

	short := New(1, time.Millisecond)
	short.Fail("a")
	time.Sleep(5 * time.Millisecond)
	if short.Blocked("a") || len(short.failures) != 0 {
		t.Fatal("failure outside the window still counts")
	}

	off := New(-1, time.Hour)
	off.Fail("a")
	if off.Blocked("a") {
		t.Fatal("a disabled limiter blocked")
	}
}

func TestHost(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.5:8765": "192.168.1.5",
		"[fe80::1]:443":    "fe80::1",
		"pipe":             "pipe",
	} {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}
