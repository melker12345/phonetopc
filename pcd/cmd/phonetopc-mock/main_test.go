package main

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

type yes struct{}

func (yes) ConfirmQR(context.Context, pairing.PhoneInfo) (bool, error) { return true, nil }
func (yes) ConfirmSAS(context.Context, pairing.PhoneInfo, string) (bool, error) {
	return true, nil
}

// startListener runs a PC pairing listener and returns its URI and a
// channel with the result.
func startListener(t *testing.T) (pairing.URI, <-chan pairing.Paired) {
	t.Helper()
	key, _ := security.GenerateKey()
	cert, _ := security.SelfSigned(key, "pc")
	got := make(chan pairing.Paired, 1)
	l, err := pairing.New(pairing.Options{
		PC: protocol.PCInfo{ID: "pc-id", Name: "Test PC"}, Cert: cert, Prompt: yes{},
		Save: func(p pairing.Paired) error { got <- p; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return pairing.URI{ID: "pc-id", Name: "Test PC", Hosts: []string{"127.0.0.1"}, Port: ln.Addr().(*net.TCPAddr).Port, Pin: l.Pin(), Secret: l.Secret()}, got
}

func TestPairQRAndRevoke(t *testing.T) {
	dir := t.TempDir()
	u, got := startListener(t)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := pairCmd(ctx, []string{u.String(), "--state", dir, "--addr", "127.0.0.1:9999", "--name", "CLI mock"}, nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	p := <-got
	if p.Phone.Name != "CLI mock" || p.Phone.Port != 9999 || p.Phone.Addresses[0] != "127.0.0.1" {
		t.Errorf("PC got %+v", p.Phone)
	}
	st, _ := mockphone.OpenState(dir)
	if pc, ok := st.Authenticate(p.Token); !ok || pc.ID != "pc-id" {
		t.Errorf("mock doesn't accept the token it issued")
	}

	out.Reset()
	if err := pcsCmd([]string{"--state", dir}, &out); err != nil || !strings.Contains(out.String(), "Test PC") {
		t.Errorf("pcs: %v\n%s", err, out.String())
	}
	if err := revokeCmd([]string{"--state", dir, "test pc"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Authenticate(p.Token); ok {
		t.Error("revoked token still accepted")
	}
}

func TestPairSASDirect(t *testing.T) {
	dir := t.TempDir()
	u, got := startListener(t)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := pairCmd(ctx, []string{"--state", dir, "--pc-addr", net.JoinHostPort("127.0.0.1", strconv.Itoa(u.Port))}, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if p := <-got; p.Way != pairing.WaySAS {
		t.Errorf("way %s", p.Way)
	}
	if !strings.Contains(out.String(), "Code: ") {
		t.Errorf("no code shown:\n%s", out.String())
	}
}

func TestPairUsage(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{"--state", t.TempDir()}, {"--state", t.TempDir(), "--discover", "--pc-addr", "x:1"}, {"phonetopc://pair?v=1"}} {
		if err := pairCmd(context.Background(), args, nil, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
