package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// End-to-end tests: `phonetopc pair` against the mock phone's pairing
// client, then secure sessions against the secure mock, all in-process.

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// phone is an in-process secure mock phone with its own state.
type phone struct {
	state *mockphone.State
	id    mockphone.Identity
	srv   *mockphone.Server
	host  string
	port  int
}

func startPhone(t *testing.T) *phone { return startPhoneOn(t, "") }

// startPhoneOn starts a phone listening on addr ("" for loopback). tweaks
// adjust its options; they may use the phone's state.
func startPhoneOn(t *testing.T, addr string, tweaks ...func(*mockphone.Options, *mockphone.State)) *phone {
	t.Helper()
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("", "Test Phone")
	if err != nil {
		t.Fatal(err)
	}
	opts := mockphone.Options{
		Device: protocol.DeviceInfo{ID: id.ID, Name: id.Name}, Authenticate: st.Authenticate,
		TapEvery: -1, LightEvery: -1, ProximityEvery: -1, BatteryEvery: -1,
	}
	for _, f := range tweaks {
		f(&opts, st)
	}
	srv := mockphone.New(opts)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = security.ServerTLSConfig(id.Cert)
	hs.Config.ErrorLog = log.New(io.Discard, "", 0) // pin-mismatch tests abort handshakes
	if addr != "" {
		hs.Listener.Close()
		if hs.Listener, err = net.Listen("tcp", addr); err != nil {
			t.Fatal(err)
		}
	}
	hs.StartTLS()
	t.Cleanup(func() { srv.Close(); hs.Close() })
	host, ps, _ := net.SplitHostPort(strings.TrimPrefix(hs.URL, "https://"))
	port, _ := strconv.Atoi(ps)
	return &phone{state: st, id: id, srv: srv, host: host, port: port}
}

func (p *phone) info() pairing.PhoneInfo { return p.id.PhoneInfo(p.port, []string{p.host}) }

// pairCmd runs `phonetopc pair` in the background.
type pairCmd struct {
	out, errb syncBuffer
	code      int
	done      chan struct{}
	cancel    context.CancelFunc
}

func startPair(t *testing.T, env map[string]string, stdin io.Reader, args ...string) *pairCmd {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &pairCmd{done: make(chan struct{}), cancel: cancel}
	args = append([]string{"pair", "--listen", "127.0.0.1:0", "--timeout", "30s"}, args...)
	go func() {
		defer close(p.done)
		p.code = run(ctx, args, stdin, &p.out, &p.errb, func(k string) string { return env[k] })
	}()
	t.Cleanup(func() { cancel(); <-p.done })
	return p
}

var uriRE = regexp.MustCompile(`phonetopc://pair\?\S+`)

// uri waits for the pairing URI on stdout.
func (p *pairCmd) uri(t *testing.T) pairing.URI {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := uriRE.FindString(p.out.String()); m != "" {
			u, err := pairing.ParseURI(m)
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
		select {
		case <-p.done:
			t.Fatalf("pair exited %d:\n%s\n%s", p.code, p.out.String(), p.errb.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("no URI printed:\n%s", p.out.String())
	panic("unreachable")
}

func (p *pairCmd) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("pair did not finish:\n%s\n%s", p.out.String(), p.errb.String())
	}
}

func tctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPairQRThenStream(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	pc := startPair(t, env, nil, "--yes", "--png", filepath.Join(t.TempDir(), "qr.png"))
	u := pc.uri(t)
	if u.Hosts[0] != "127.0.0.1" || !strings.Contains(pc.out.String(), "▀") {
		t.Errorf("hosts %v / no QR drawn:\n%s", u.Hosts, pc.out.String())
	}

	paired, err := pairing.PairQR(tctx(t), u, ph.info(), security.NewToken())
	if err != nil {
		t.Fatal(err)
	}
	if err := ph.state.Record(paired); err != nil {
		t.Fatal(err)
	}
	pc.wait(t)
	if pc.code != 0 || !strings.Contains(pc.out.String(), `Paired with "Test Phone"`) || !strings.Contains(pc.out.String(), "by QR code") {
		t.Fatalf("pair: code %d\n%s\n%s", pc.code, pc.out.String(), pc.errb.String())
	}

	// devices.json has the phone, with the token the phone issued.
	dev, err := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")).Find("")
	if err != nil || dev.Token != paired.Token || dev.Pin != ph.id.Pin || dev.Port != ph.port || dev.Addresses[0] != "127.0.0.1" {
		t.Fatalf("stored %+v, %v", dev, err)
	}

	// Secure session with the stored credentials, no --addr needed.
	code, out, errs := runCLI(t, tctx(t), env, "info")
	if code != 0 || !strings.Contains(out, "paired as") || !strings.Contains(out, u.Name) || !strings.Contains(out, ph.id.ID) {
		t.Fatalf("info: code %d\n%s\n%s", code, out, errs)
	}
	code, out, errs = runCLI(t, tctx(t), env, "tail", "gyro", "--rate", "100", "--count", "3")
	if code != 0 || strings.Count(out, "gyro/rotation") != 3 {
		t.Fatalf("tail: code %d\n%s\n%s", code, out, errs)
	}

	// devices and pc.
	code, out, _ = runCLI(t, tctx(t), env, "devices")
	if code != 0 || !strings.Contains(out, "Test Phone") || !strings.Contains(out, net.JoinHostPort("127.0.0.1", strconv.Itoa(ph.port))) || strings.Contains(out, paired.Token) {
		t.Errorf("devices:\n%s", out)
	}
	code, out, _ = runCLI(t, tctx(t), env, "devices", "--json")
	if code != 0 || strings.Contains(out, paired.Token) || !strings.Contains(out, ph.id.Pin) {
		t.Errorf("devices --json:\n%s", out)
	}
	code, out, _ = runCLI(t, tctx(t), env, "pc", "--json")
	var me map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &me) != nil || me["pin"] != u.Pin || me["id"] != u.ID {
		t.Errorf("pc: %s", out)
	}
}

func TestPairSASThenStream(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	pc := startPair(t, env, strings.NewReader("y\n")) // the PC user types y
	u := pc.uri(t)                                    // only for the port: SAS skips the QR

	var phoneCode string
	paired, err := pairing.PairSAS(tctx(t), net.JoinHostPort("127.0.0.1", strconv.Itoa(u.Port)), ph.info(), security.NewToken(),
		func(_ context.Context, _ protocol.PCInfo, code string) (bool, error) {
			phoneCode = code
			return true, nil
		})
	if err != nil {
		t.Fatalf("%v\n%s", err, pc.out.String())
	}
	ph.state.Record(paired)
	pc.wait(t)
	if pc.code != 0 || !strings.Contains(pc.out.String(), "Does the phone show "+security.FormatCode(phoneCode)+"?") {
		t.Fatalf("pair: code %d\n%s\n%s", pc.code, pc.out.String(), pc.errb.String())
	}
	if paired.Pin != u.Pin {
		t.Errorf("phone stored PC pin %s, want %s", paired.Pin, u.Pin)
	}
	code, out, errs := runCLI(t, tctx(t), env, "get", "battery")
	if code != 0 || !strings.Contains(out, "battery/state") {
		t.Fatalf("get: code %d\n%s\n%s", code, out, errs)
	}
}

func TestPairSASRefusedOnPC(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	pc := startPair(t, env, strings.NewReader("\n")) // default answer: no
	u := pc.uri(t)
	_, err := pairing.PairSAS(tctx(t), net.JoinHostPort("127.0.0.1", strconv.Itoa(u.Port)), ph.info(), security.NewToken(),
		func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil })
	code, _ := protocol.ErrorCodeOf(err)
	if code != protocol.CodeSASMismatch {
		t.Fatalf("want -32023, got %v", err)
	}
	pc.cancel() // Ctrl-C
	pc.wait(t)
	if pc.code != 0 {
		t.Errorf("Ctrl-C exit code %d", pc.code)
	}
	if devs, _ := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")).Devices(); len(devs) != 0 {
		t.Errorf("stored %+v", devs)
	}
}

func TestPairSASLimit(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	pc := startPair(t, env, nil) // stdin at EOF: the PC user says no
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(pc.uri(t).Port))
	for range 3 {
		_, err := pairing.PairSAS(tctx(t), addr, ph.info(), security.NewToken(),
			func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil })
		if !errors.Is(err, pairing.ErrCodesDiffer) {
			t.Fatalf("want codes to differ, got %v", err)
		}
		time.Sleep(50 * time.Millisecond) // let the listener free its slot
	}
	pc.wait(t)
	if pc.code != 1 || !strings.Contains(pc.errb.String(), "too many code comparisons failed") {
		t.Errorf("code %d, stderr %s", pc.code, pc.errb.String())
	}
}

func TestPairExpires(t *testing.T) {
	env := testEnv(t, nil)
	var out, errb bytes.Buffer
	code := run(tctx(t), []string{"pair", "--listen", "127.0.0.1:0", "--timeout", "300ms"}, nil, &out, &errb, func(k string) string { return env[k] })
	if code != 1 || !strings.Contains(errb.String(), "no phone paired within 300ms") {
		t.Errorf("code %d, stderr %s", code, errb.String())
	}
}

// TestPairYesRefusesSAS: --yes never confirms a code, so anyone on the LAN
// can't pair through "Find on network" while a script waits for the QR.
func TestPairYesRefusesSAS(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	pc := startPair(t, env, nil, "--yes")
	u := pc.uri(t)
	if !strings.Contains(pc.out.String(), "only the QR code works") {
		t.Errorf("no hint that Find on network is off:\n%s", pc.out.String())
	}
	_, err := pairing.PairSAS(tctx(t), net.JoinHostPort("127.0.0.1", strconv.Itoa(u.Port)), ph.info(), security.NewToken(),
		func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil })
	if code, _ := protocol.ErrorCodeOf(err); code != protocol.CodeRejected {
		t.Fatalf("want -32021, got %v", err)
	}
	if strings.Contains(pc.out.String(), "Does the phone show") {
		t.Errorf("a code was shown:\n%s", pc.out.String())
	}
	// The QR code still pairs.
	if _, err := pairing.PairQR(tctx(t), u, ph.info(), security.NewToken()); err != nil {
		t.Fatal(err)
	}
	pc.wait(t)
	if pc.code != 0 {
		t.Fatalf("pair: code %d\n%s", pc.code, pc.errb.String())
	}
}

// TestPairReplacingAnotherKey: a pairing that would replace a paired phone
// with a different key is refused under --yes and needs an explicit yes
// otherwise.
func TestPairReplacingAnotherKey(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	old := pairDirect(t, env, ph, "phone", []string{"127.0.0.1"})
	st := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc"))
	impostor := startPhone(t).info()
	impostor.ID = ph.id.ID // same id, another key

	pc := startPair(t, env, nil, "--yes")
	_, err := pairing.PairQR(tctx(t), pc.uri(t), impostor, security.NewToken())
	if code, _ := protocol.ErrorCodeOf(err); code != protocol.CodeRejected {
		t.Fatalf("--yes: want -32021, got %v", err)
	}
	if !strings.Contains(pc.out.String(), "already paired, with a different key") || !strings.Contains(pc.out.String(), "Refused") {
		t.Errorf("no warning:\n%s", pc.out.String())
	}
	// The same key re-pairs under --yes (the phone lost its token, say).
	if _, err := pairing.PairQR(tctx(t), pc.uri(t), ph.info(), security.NewToken()); err != nil {
		t.Fatal(err)
	}
	pc.wait(t)

	// Without --yes, an empty answer is no, ...
	pc = startPair(t, env, strings.NewReader("\n"))
	_, err = pairing.PairQR(tctx(t), pc.uri(t), impostor, security.NewToken())
	if code, _ := protocol.ErrorCodeOf(err); code != protocol.CodeRejected {
		t.Fatalf("empty answer: want -32021, got %v", err)
	}
	pc.cancel()
	pc.wait(t)
	if d, err := st.Find(ph.id.ID); err != nil || d.Pin != old.Pin {
		t.Fatalf("replaced: %+v %v", d, err)
	}
	// ... and y replaces it.
	pc = startPair(t, env, strings.NewReader("y\n"))
	if _, err := pairing.PairQR(tctx(t), pc.uri(t), impostor, security.NewToken()); err != nil {
		t.Fatal(err)
	}
	pc.wait(t)
	if !strings.Contains(pc.out.String(), "Replace \"Test Phone\"? [y/N]") {
		t.Errorf("prompt:\n%s", pc.out.String())
	}
	if d, err := st.Find(ph.id.ID); err != nil || d.Pin != impostor.Pin {
		t.Fatalf("not replaced: %+v %v", d, err)
	}
}

// pairDirect stores a pairing for ph in env's store without the listener.
func pairDirect(t *testing.T, env map[string]string, ph *phone, name string, addrs []string) store.Device {
	t.Helper()
	tok := security.NewToken()
	ph.state.PutPC(mockphone.PairedPC{ID: "pc-" + name, Name: "test PC", Token: tok})
	d := store.Device{ID: ph.id.ID, Name: name, Pin: ph.id.Pin, Token: tok, Port: ph.port, Addresses: addrs, PairedAt: store.Now(time.Now())}
	if err := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")).PutDevice(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestErrorsExplained(t *testing.T) {
	env := testEnv(t, nil)
	code, _, errs := runCLI(t, tctx(t), env, "info")
	if code != 1 || !strings.Contains(errs, "no paired phone — run `phonetopc pair`") {
		t.Errorf("no device: %d %s", code, errs)
	}

	ph := startPhone(t)
	d := pairDirect(t, env, ph, "phone", []string{"127.0.0.1"})

	// Revoked on the phone.
	ph.state.RemovePC("pc-phone")
	code, _, errs = runCLI(t, tctx(t), env, "info")
	if code != 1 || !strings.Contains(errs, "this PC was revoked on the phone — run `phonetopc pair`") {
		t.Errorf("revoked: %d %s", code, errs)
	}

	// The phone's key changed.
	other := startPhone(t)
	d.Port = other.port
	store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")).PutDevice(d)
	code, _, errs = runCLI(t, tctx(t), env, "info")
	if code != 1 || !strings.Contains(errs, "phone identity changed — re-pair") || !strings.Contains(errs, other.id.Pin) {
		t.Errorf("pin mismatch: %d %s", code, errs)
	}
}

func TestDeviceIDChangedWarns(t *testing.T) {
	env := testEnv(t, nil)
	ph := startPhone(t)
	d := pairDirect(t, env, ph, "phone", []string{"127.0.0.1"})
	st := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc"))
	st.RemoveDevice(d.ID)
	d.ID = "0000000000000000"
	st.PutDevice(d)
	code, _, errs := runCLI(t, tctx(t), env, "info")
	if code != 0 || !strings.Contains(errs, "warning: the phone paired as \"phone\" (id 0000000000000000) now reports device id "+ph.id.ID) {
		t.Errorf("code %d, stderr %s", code, errs)
	}
}

func TestDeviceSelectionAndAddresses(t *testing.T) {
	env := testEnv(t, nil)
	a, b := startPhone(t), startPhone(t)
	// 127.0.0.2 refuses (nothing listens there); the phone is found at the
	// second address.
	pairDirect(t, env, a, "alpha", []string{"127.0.0.2", "127.0.0.1"})
	pairDirect(t, env, b, "beta", []string{"127.0.0.1"})

	code, _, errs := runCLI(t, tctx(t), env, "info")
	if code != 1 || !strings.Contains(errs, "--device") {
		t.Errorf("ambiguous: %d %s", code, errs)
	}
	code, out, errs := runCLI(t, tctx(t), env, "--device", "ALPHA", "info", "--json")
	var h protocol.HelloResult
	if code != 0 || json.Unmarshal([]byte(out), &h) != nil || h.Device.ID != a.id.ID || h.PC == nil {
		t.Fatalf("alpha: %d %s %s", code, out, errs)
	}
	st := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc"))
	// Loopback addresses (as from adb forward) are never recorded as the
	// working one (SECURITY.md §6), so the order is unchanged here; the
	// store tests cover LAN addresses.
	if d, _ := st.Find("alpha"); d.Addresses[0] != "127.0.0.2" {
		t.Errorf("addresses changed: %v", d.Addresses)
	}
	envB := testEnv(t, map[string]string{"XDG_CONFIG_HOME": env["XDG_CONFIG_HOME"], "PHONETOPC_DEVICE": b.id.ID})
	if code, out, _ := runCLI(t, tctx(t), envB, "info", "--json"); code != 0 || !strings.Contains(out, b.id.ID) {
		t.Errorf("beta by env: %d %s", code, out)
	}
	// --addr overrides discovery but keeps the device's credentials.
	if code, out, errs := runCLI(t, tctx(t), env, "--device", "beta", "--addr", net.JoinHostPort("127.0.0.1", strconv.Itoa(b.port)), "info"); code != 0 || !strings.Contains(out, "paired as") {
		t.Errorf("--addr: %d %s %s", code, out, errs)
	}

	code, out, _ = runCLI(t, tctx(t), env, "unpair", "beta")
	if code != 0 || !strings.Contains(out, "revoke this PC on the phone") {
		t.Errorf("unpair: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, tctx(t), env, "info"); code != 0 {
		t.Errorf("single device left: %d", code)
	}
	if code, _, errs := runCLI(t, tctx(t), env, "unpair", "beta"); code != 1 || !strings.Contains(errs, "no paired phone matches") {
		t.Errorf("unpair twice: %d %s", code, errs)
	}
}

func TestPCCommand(t *testing.T) {
	env := testEnv(t, nil)
	code, out, _ := runCLI(t, tctx(t), env, "pc")
	if code != 0 || !regexp.MustCompile(`pin\s+sha256/`).MatchString(out) {
		t.Fatalf("pc: %s", out)
	}
	dir := filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("store dir %v %v", fi, err)
	}
	code2, out2, _ := runCLI(t, tctx(t), env, "pc")
	if code2 != 0 || out2 != out {
		t.Errorf("identity changed between runs:\n%s\n%s", out, out2)
	}
}

func TestWriteQR(t *testing.T) {
	var b bytes.Buffer
	if err := writeQR(&b, "phonetopc://pair?v=1", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	// Version 1 or 2 plus a 4-module quiet zone on each side: 29 or 33
	// modules, drawn two rows per line.
	if n := len(lines); n != 15 && n != 17 {
		t.Errorf("%d lines:\n%s", n, b.String())
	}
	if !strings.HasPrefix(lines[0], "    "+strings.Repeat("█", 10)) {
		t.Errorf("quiet zone missing: %q", lines[0])
	}
}
