//go:build linux

package daemon_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/localapi"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// unpairedPhone is a secure mock that no PC has paired with yet.
type unpairedPhone struct {
	st   *mockphone.State
	id   mockphone.Identity
	addr string
}

func newUnpairedPhone(t *testing.T) *unpairedPhone {
	t.Helper()
	st, err := mockphone.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("", "Test Phone")
	if err != nil {
		t.Fatal(err)
	}
	opts := quietOptions()
	opts.Device = protocol.DeviceInfo{ID: id.ID, Name: id.Name}
	opts.Authenticate = st.Authenticate
	opts.ApprovalKey = func() *ecdsa.PrivateKey { k, _ := st.ApprovalKey(); return k }
	opts.DecryptKey = func() *rsa.PrivateKey { k, _ := st.DecryptKey(); return k }
	opts.Attest = mockphone.TestAttestation(testCA, id.Pin, nil)
	srv := mockphone.New(opts)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = security.ServerTLSConfig(id.Cert)
	hs.Config.ErrorLog = log.New(io.Discard, "", 0)
	hs.StartTLS()
	t.Cleanup(func() { srv.Close(); hs.Close() })
	return &unpairedPhone{st: st, id: id, addr: strings.TrimPrefix(hs.URL, "https://")}
}

func (p *unpairedPhone) info() pairing.PhoneInfo {
	host, port, _ := net.SplitHostPort(p.addr)
	n, _ := strconv.Atoi(port)
	return p.id.PhoneInfo(n, []string{host})
}

// systemDaemon runs a daemon that owns an empty store, as the system service
// does, on a Unix socket guarded by access. It waits a minute between
// connection attempts, so a prompt connection after pairing shows that
// pairing made it connect.
func systemDaemon(t *testing.T, ph *unpairedPhone, access *daemon.Access) (*daemon.Daemon, *store.Store, *localapi.Client) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "phonetopc")
	st := store.Open(cfg)
	dial := func(ctx context.Context) (*client.Client, error) {
		return client.DialPaired(ctx, "", client.WithConfigDir(cfg), client.WithAddress(ph.addr), client.WithAttestationRoots(testCA.Root))
	}
	d := daemon.New(daemon.Options{Dial: dial, Store: st, MinBackoff: time.Minute, MaxBackoff: time.Minute, FirstConnectWait: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	dir, err := os.MkdirTemp("", "ptc")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: daemon.RequirePeer(d.Handler()), ConnContext: access.ConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); cancel(); <-done; os.RemoveAll(dir) })
	return d, st, localapi.New(sock)
}

type pairingClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func startPairing(t *testing.T, api *localapi.Client, start daemon.PairingStart) *pairingClient {
	t.Helper()
	conn, err := api.WebSocket(testCtx(t), "/v1/pairing")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	pc := &pairingClient{t: t, conn: conn}
	pc.send(start)
	return pc
}

func (pc *pairingClient) send(v any) {
	pc.t.Helper()
	b, _ := json.Marshal(v)
	if err := pc.conn.Write(testCtx(pc.t), websocket.MessageText, b); err != nil {
		pc.t.Fatal(err)
	}
}

func (pc *pairingClient) next(typ string) daemon.PairingMessage {
	pc.t.Helper()
	for {
		_, b, err := pc.conn.Read(testCtx(pc.t))
		if err != nil {
			pc.t.Fatalf("waiting for %s: %v", typ, err)
		}
		var m daemon.PairingMessage
		if err := json.Unmarshal(b, &m); err != nil {
			pc.t.Fatal(err)
		}
		if m.Type == daemon.PairLog {
			continue
		}
		if m.Type != typ {
			pc.t.Fatalf("want %s, got %s", typ, b)
		}
		return m
	}
}

// TestPairingOverAPI: the daemon owns the store, so the CLI pairs through
// it; the questions reach the client, and only an answer to the question
// asked counts.
func TestPairingOverAPI(t *testing.T) {
	ph := newUnpairedPhone(t)
	d, st, api := systemDaemon(t, ph, daemon.UserAccess())
	pc := startPairing(t, api, daemon.PairingStart{Listen: "127.0.0.1:0", Hosts: []string{"127.0.0.1"}, NoMDNS: true, TimeoutMS: 30000})
	pc.send(daemon.PairingAnswer{Type: "answer", Prompt: "early", Yes: true})
	l := pc.next(daemon.PairListening)
	u, err := pairing.ParseURI(l.URI)
	if err != nil || l.PC == nil || u.Pin != l.PC.Pin || l.Port != u.Port {
		t.Fatalf("listening %+v: %v", l, err)
	}

	type result struct {
		p   pairing.PairedPC
		err error
	}
	pairQR := func(u pairing.URI) <-chan result {
		ch := make(chan result, 1)
		go func() {
			p, err := pairing.PairQR(testCtx(t), u, ph.info(), security.NewToken())
			ch <- result{p, err}
		}()
		return ch
	}

	// No: the phone is refused, and the QR code changes.
	got := pairQR(u)
	q := pc.next(daemon.PairConfirm)
	if q.Way != "qr" || q.Phone == nil || q.Phone.Name != "Test Phone" || len(q.Prompt) < 20 {
		t.Fatalf("question %+v", q)
	}
	pc.send(daemon.PairingAnswer{Type: "answer", Prompt: q.Prompt, Yes: false})
	if r := <-got; !isCode(r.err, protocol.CodeRejected) {
		t.Fatalf("want -32021, got %v", r.err)
	}
	u2, err := pairing.ParseURI(pc.next(daemon.PairQR).URI)
	if err != nil || u2.Secret == u.Secret {
		t.Fatalf("new QR code: %+v %v", u2, err)
	}

	// Yes, after an answer to another question that doesn't count.
	got = pairQR(u2)
	q = pc.next(daemon.PairConfirm)
	pc.send(daemon.PairingAnswer{Type: "answer", Prompt: q.Prompt + "x", Yes: true})
	select {
	case r := <-got:
		t.Fatalf("answered by the wrong prompt id: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	pc.send(daemon.PairingAnswer{Type: "answer", Prompt: q.Prompt, Yes: true})
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if err := ph.st.Record(r.p); err != nil {
		t.Fatal(err)
	}
	if m := pc.next(daemon.PairPaired); m.Phone == nil || m.Phone.ID != ph.id.ID || m.Way != "qr" {
		t.Fatalf("paired %+v", m)
	}
	dev, err := st.Find("")
	if err != nil || dev.Pin != ph.id.Pin {
		t.Fatalf("stored %+v %v", dev, err)
	}
	waitFor(t, func() bool { return d.Status().Connected })

	ctx := testCtx(t)
	var devs daemon.DevicesResponse
	if err := api.Get(ctx, "/v1/devices", &devs); err != nil || len(devs.Devices) != 1 || devs.Devices[0].ID != ph.id.ID {
		t.Fatalf("devices %+v %v", devs, err)
	}
	if b, _ := json.Marshal(devs); strings.Contains(string(b), dev.Token) {
		t.Fatal("GET /v1/devices shows the token")
	}
	var me daemon.PCInfo
	if err := api.Get(ctx, "/v1/pc", &me); err != nil || me.Pin != u.Pin || me.ID != u.ID {
		t.Fatalf("pc %+v %v", me, err)
	}
	var key daemon.ApprovalKeyInfo
	if err := api.Get(ctx, "/v1/approval-key", &key); err != nil || key.Status != daemon.KeyTrusted || key.Attestation == "" || key.Current == "" {
		t.Fatalf("approval key %+v %v", key, err)
	}

	var gone daemon.DeviceRef
	if err := api.Do(ctx, "DELETE", "/v1/devices/"+ph.id.ID, nil, &gone); err != nil || gone.ID != ph.id.ID {
		t.Fatalf("unpair %+v %v", gone, err)
	}
	waitFor(t, func() bool { return !d.Status().Connected })
	if err := api.Do(ctx, "DELETE", "/v1/devices/"+ph.id.ID, nil, nil); !localapi.IsCode(err, "unknown_device") {
		t.Fatalf("unpair again: %v", err)
	}
}

func isCode(err error, code protocol.ErrorCode) bool {
	c, ok := protocol.ErrorCodeOf(err)
	return ok && c == code
}

func TestPairingOneAtATime(t *testing.T) {
	_, _, api := systemDaemon(t, newUnpairedPhone(t), daemon.UserAccess())
	pc := startPairing(t, api, daemon.PairingStart{Listen: "127.0.0.1:0", NoMDNS: true})
	pc.next(daemon.PairListening)
	if _, err := api.WebSocket(testCtx(t), "/v1/pairing"); !localapi.IsCode(err, "busy") {
		t.Fatalf("second pairing: %v", err)
	}
}

func TestPairingBadStart(t *testing.T) {
	_, _, api := systemDaemon(t, newUnpairedPhone(t), daemon.UserAccess())
	pc := startPairing(t, api, daemon.PairingStart{TimeoutMS: 1})
	if m := pc.next(daemon.PairError); m.Error != "invalid_request" {
		t.Fatalf("%+v", m)
	}
}

// TestAdministrationNeedsRoot: members of the group may use the phone, but
// only root (or the daemon's own user) may change the pairings.
func TestAdministrationNeedsRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may do everything")
	}
	member := &daemon.Access{Self: os.Getuid() + 1, Group: os.Getgid()}
	_, _, api := systemDaemon(t, newUnpairedPhone(t), member)
	ctx := testCtx(t)
	var st daemon.Status
	if err := api.Get(ctx, "/v1/status", &st); err != nil {
		t.Fatalf("a member's status: %v", err)
	}
	var devs daemon.DevicesResponse
	if err := api.Get(ctx, "/v1/devices", &devs); err != nil {
		t.Fatalf("a member's devices: %v", err)
	}
	for _, c := range []struct{ method, path string }{{"DELETE", "/v1/devices/x"}, {"POST", "/v1/approval-key"}} {
		if err := api.Do(ctx, c.method, c.path, struct{}{}, nil); !localapi.IsCode(err, "forbidden") {
			t.Errorf("%s %s: %v", c.method, c.path, err)
		}
	}
	if _, err := api.WebSocket(ctx, "/v1/pairing"); !localapi.IsCode(err, "forbidden") {
		t.Errorf("pairing: %v", err)
	}

	stranger := &daemon.Access{Self: os.Getuid() + 1, Group: -1}
	_, _, api = systemDaemon(t, newUnpairedPhone(t), stranger)
	if err := api.Get(ctx, "/v1/status", &st); !localapi.IsCode(err, "forbidden") {
		t.Errorf("a stranger's status: %v", err)
	}
}

func TestAccessRules(t *testing.T) {
	lookups := 0
	a := &daemon.Access{Self: 500, Group: 900, GroupsOf: func(uid int) ([]int, error) {
		lookups++
		switch uid {
		case 1000:
			return []int{1000, 900}, nil
		case 1001:
			return []int{1001}, nil
		}
		return nil, errors.New("no such user")
	}}
	for _, tc := range []struct {
		peer       daemon.Peer
		use, admin bool
	}{
		{daemon.Peer{UID: 0}, true, true},
		{daemon.Peer{UID: 500}, true, true},
		{daemon.Peer{UID: 1000}, true, false},                     // in the group, says the user database
		{daemon.Peer{UID: 1001}, false, false},                    // not in it
		{daemon.Peer{UID: 1002}, false, false},                    // unknown
		{daemon.Peer{UID: 1001, Groups: []int{900}}, true, false}, // the kernel says it is
		{daemon.Peer{UID: 1000, Groups: []int{1000}}, false, false},
	} {
		if use, admin := a.MayUse(tc.peer), a.MayAdmin(tc.peer); use != tc.use || admin != tc.admin {
			t.Errorf("%+v: use %v admin %v, want %v %v", tc.peer, use, admin, tc.use, tc.admin)
		}
	}
	if lookups != 3 {
		t.Errorf("%d lookups, want 3: the kernel's groups are used when it gives them", lookups)
	}
	if (&daemon.Access{Self: 500, Group: -1}).MayUse(daemon.Peer{UID: 1000, Groups: []int{900}}) {
		t.Error("no group, and a stranger may use it")
	}
}

// TestTCPGroupMembers: on TCP the kernel gives only the peer's uid; its
// groups come from the user database.
func TestTCPGroupMembers(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may do everything")
	}
	_, d, _ := connectedDaemon(t, quietOptions())
	for _, tc := range []struct {
		groups []int
		want   int
	}{{[]int{4242}, 200}, {[]int{1}, 401}} {
		a := &daemon.Access{Self: os.Getuid() + 1, Group: 4242, GroupsOf: func(uid int) ([]int, error) {
			if uid != os.Getuid() {
				t.Errorf("looked up uid %d", uid)
			}
			return tc.groups, nil
		}}
		hs := httptest.NewUnstartedServer(daemon.RequireToken("", d.Handler()))
		hs.Config.ConnContext = a.ConnContext
		hs.Start()
		res, err := http.Get(hs.URL + "/v1/status")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		hs.Close()
		if res.StatusCode != tc.want {
			t.Errorf("groups %v: %d, want %d", tc.groups, res.StatusCode, tc.want)
		}
	}
}
