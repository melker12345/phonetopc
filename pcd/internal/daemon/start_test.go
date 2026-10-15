package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
)

func afterRelease(release <-chan struct{}, dial func(context.Context) (*client.Client, error)) func(context.Context) (*client.Client, error) {
	return func(ctx context.Context) (*client.Client, error) {
		select {
		case <-release:
			return dial(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// A daemon started by the request itself (socket activation) answers it
// once its first connection attempt is done, instead of 503 "connecting".
func TestFirstRequestWaitsForFirstConnection(t *testing.T) {
	p := newPhone(t, quietOptions())
	p.start()
	release := make(chan struct{})
	slow := afterRelease(release, p.dial)
	_, url := startDaemon(t, slow)

	if r := do(t, "GET", url+"/v1/status", ""); r.body["connected"] != false {
		t.Fatalf("status should answer at once: %s", r.raw)
	}
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	if r := do(t, "GET", url+"/v1/sensors/battery", ""); r.status != 200 {
		t.Fatalf("got %d: %s", r.status, r.raw)
	}
}

func TestFirstRequestGetsTheFirstAttemptsError(t *testing.T) {
	p := newPhone(t, quietOptions()) // never started
	release := make(chan struct{})
	slow := afterRelease(release, p.dial)
	_, url := startDaemon(t, slow)
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	r := do(t, "GET", url+"/v1/sensors/battery", "")
	wantStatus(t, r, 503, "phone_unavailable")
	if msg := r.body["error"].(map[string]any)["message"].(string); msg == "phone not connected: connecting to the phone" {
		t.Fatalf("answered before the attempt finished: %s", msg)
	}
}
