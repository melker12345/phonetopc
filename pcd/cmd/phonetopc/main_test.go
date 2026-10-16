package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func startMock(t *testing.T) (*mockphone.Server, string) {
	t.Helper()
	srv := mockphone.New(mockphone.Options{TapEvery: -1, LightEvery: -1, ProximityEvery: -1, BatteryEvery: -1, LocationEvery: -1, NFCEvery: -1, UserEvery: -1})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); hs.Close() })
	return srv, strings.TrimPrefix(hs.URL, "http://")
}

// testEnv returns an environment with an empty, private credential store
// and mDNS off, plus extra.
func testEnv(t *testing.T, extra map[string]string) map[string]string {
	env := map[string]string{
		"XDG_CONFIG_HOME":   t.TempDir(),
		"HOME":              "/nonexistent",
		"PHONETOPC_NO_MDNS": "1",
		"PHONETOPC_MODE":    "user",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func runCLI(t *testing.T, ctx context.Context, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLIWith(t, ctx, env, nil, args...)
}

func runCLIWith(t *testing.T, ctx context.Context, env map[string]string, stdin io.Reader, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	if env == nil {
		env = testEnv(t, nil)
	}
	var out, errb bytes.Buffer
	code = run(ctx, args, stdin, &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestInfoAndAddrFromEnv(t *testing.T) {
	_, addr := startMock(t)
	code, out, errs := runCLI(t, context.Background(), testEnv(t, map[string]string{"PHONETOPC_ADDR": addr, "PHONETOPC_INSECURE_DEV": "1"}), "info")
	if code != 0 || !strings.Contains(out, "phonetopc/1") || !strings.Contains(out, "6d6f636b00000001") {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errs)
	}
	// --addr overrides the environment, before or after the command.
	for _, args := range [][]string{{"--insecure-dev", "--addr", addr, "info", "--json"}, {"info", "--json", "--addr", addr, "--insecure-dev"}} {
		code, out, errs = runCLI(t, context.Background(), testEnv(t, map[string]string{"PHONETOPC_ADDR": "127.0.0.1:1"}), args...)
		var h protocol.HelloResult
		if code != 0 || json.Unmarshal([]byte(out), &h) != nil || h.Device.ID != "6d6f636b00000001" {
			t.Errorf("%v: code %d\nstdout %s\nstderr %s", args, code, out, errs)
		}
	}
}

func TestList(t *testing.T) {
	_, addr := startMock(t)
	code, out, errs := runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "list")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1+len(protocol.Catalog()) || !strings.HasPrefix(lines[0], "SOURCE") {
		t.Fatalf("table:\n%s", out)
	}
	for _, want := range []string{"gyro", "rad/s", "1-200 (default 50)", "game_rotation_vector", "tile_tap"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestGet(t *testing.T) {
	_, addr := startMock(t)
	code, out, _ := runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "get", "battery")
	if code != 0 || !strings.Contains(out, "battery/state") || !strings.Contains(out, "level=87") {
		t.Errorf("code %d, out %q", code, out)
	}
	code, out, _ = runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "get", "gyro", "--json")
	var r protocol.Reading
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Source != "gyro" {
		t.Errorf("code %d, out %q", code, out)
	}
	code, _, errs := runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "get", "nope")
	if code != 1 || !strings.Contains(errs, "-32001") || !strings.Contains(errs, "unknown_source") {
		t.Errorf("code %d, stderr %q", code, errs)
	}
}

func TestTail(t *testing.T) {
	_, addr := startMock(t)
	code, out, errs := runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "tail", "gyro", "--rate", "100", "--count", "5")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 5 || !strings.Contains(lines[4], "gyro/rotation  #4  [") || !strings.HasSuffix(lines[4], "rad/s") {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errs)
	}
	if !strings.Contains(errs, "at 100 Hz") {
		t.Errorf("stderr %q", errs)
	}

	code, out, _ = runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", addr, "tail", "--json", "accelerometer", "--count=3", "--min-delta", "0.01")
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("code %d, out %s", code, out)
	}
	for i, l := range lines {
		var ev protocol.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev.Seq != uint64(i) || ev.Source != "accelerometer" {
			t.Errorf("line %q: %v", l, err)
		}
	}
}

func TestTailInterruptExitsCleanly(t *testing.T) {
	srv, addr := startMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var code int
	var out string
	go func() {
		code, out, _ = runCLI(t, ctx, nil, "--insecure-dev", "--addr", addr, "tail", "ui")
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	srv.Tap()
	time.Sleep(100 * time.Millisecond)
	cancel() // what Ctrl-C does via signal.NotifyContext
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tail did not exit on interrupt")
	}
	if code != 0 || !strings.Contains(out, "ui/tile_tap  #0  true") {
		t.Errorf("code %d, out %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"get"}, {"tail", "a", "b"}, {"list", "x"}, {"tail", "gyro", "--rate", "fast"}, {"--nope"}} {
		code, _, errs := runCLI(t, context.Background(), nil, args...)
		if code != 2 || !strings.Contains(errs, "Usage:") {
			t.Errorf("%v: code %d, stderr %q", args, code, errs)
		}
	}
}

func TestConnectionRefused(t *testing.T) {
	code, _, errs := runCLI(t, context.Background(), nil, "--insecure-dev", "--addr", "127.0.0.1:1", "info")
	if code != 1 || !strings.Contains(errs, "dial") {
		t.Errorf("code %d, stderr %q", code, errs)
	}
}

func TestFormatValue(t *testing.T) {
	for in, want := range map[string]string{
		`[0.01,-0.32,0.05]`:            "[+0.0100 -0.3200 +0.0500]",
		`3.5`:                          "3.5",
		`true`:                         "true",
		`{"level":82,"charging":true}`: "charging=true level=82",
		`"x"`:                          "x",
	} {
		if got := formatValue(json.RawMessage(in)); got != want {
			t.Errorf("formatValue(%s) = %q, want %q", in, got, want)
		}
	}
}
