//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// systemEnv runs phonetopcd in-process the way the system service does: it
// owns its store, and the CLI reaches it only through its socket.
func systemEnv(t *testing.T, ph *phone) (map[string]string, *store.Store, *daemon.Daemon) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "phonetopc")
	st := store.Open(cfg)
	addr := net.JoinHostPort(ph.host, strconv.Itoa(ph.port))
	dial := func(ctx context.Context) (*client.Client, error) {
		return client.DialPaired(ctx, "", client.WithConfigDir(cfg), client.WithAddress(addr), client.WithAttestationRoots(testCA.Root))
	}
	d := daemon.New(daemon.Options{Dial: dial, Store: st, MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond})
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
	srv := &http.Server{Handler: daemon.RequirePeer(d.Handler()), ConnContext: daemon.UserAccess().ConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); cancel(); <-done; os.RemoveAll(dir) })
	return testEnv(t, map[string]string{"PHONETOPC_MODE": "system", "PHONETOPC_SOCKET": sock}), st, d
}

func TestSystemMode(t *testing.T) {
	ph := startApprovalPhone(t, mockphone.ApproveAll, false)
	env, st, d := systemEnv(t, ph.phone)

	if code, out, errs := runCLI(t, tctx(t), env, "devices"); code != 0 || !strings.Contains(out, "No paired phones") {
		t.Fatalf("devices before pairing: %d %s %s", code, out, errs)
	}
	pc := startPair(t, env, strings.NewReader("y\n"))
	u := pc.uri(t)
	paired, err := pairing.PairQR(tctx(t), u, ph.info(), security.NewToken())
	if err != nil {
		t.Fatalf("%v\n%s", err, pc.out.String())
	}
	if err := ph.state.Record(paired); err != nil {
		t.Fatal(err)
	}
	pc.wait(t)
	if pc.code != 0 || !strings.Contains(pc.out.String(), `Pair with "Test Phone"? [y/N]`) || !strings.Contains(pc.out.String(), "by QR code") {
		t.Fatalf("pair: %d\n%s\n%s", pc.code, pc.out.String(), pc.errb.String())
	}
	if dev, err := st.Find(""); err != nil || dev.Token != paired.Token {
		t.Fatalf("the daemon's store: %+v %v", dev, err)
	}
	if _, err := os.Stat(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc")); !os.IsNotExist(err) {
		t.Fatalf("the user's own store was written: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !d.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"devices"}, "Test Phone"},
		{[]string{"info"}, "phonetopcd at"},
		{[]string{"list"}, "gyro"},
		{[]string{"get", "battery"}, "battery/state"},
		{[]string{"tail", "gyro", "--rate", "50", "--count", "2"}, "gyro/rotation"},
		{[]string{"approval-key"}, "trusted"},
	} {
		if code, out, errs := runCLI(t, tctx(t), env, tc.args...); code != 0 || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %d\n%s\n%s", tc.args, code, out, errs)
		}
	}
	code, out, _ := runCLI(t, tctx(t), env, "pc", "--json")
	var me map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &me) != nil || me["pin"] != u.Pin {
		t.Errorf("pc: %s", out)
	}
	if code, _, errs := runCLI(t, tctx(t), env, "approve", "Deploy?"); code != 0 || ph.asked.Load() != 1 || !strings.Contains(errs, "approved on") || !strings.Contains(errs, "phonetopc: pick ") {
		t.Errorf("approve: %d asked %d %s", code, ph.asked.Load(), errs)
	}
	if code, out, errs := runCLI(t, tctx(t), env, "unpair", "Test Phone"); code != 0 || !strings.Contains(out, "Forgot") {
		t.Errorf("unpair: %d %s %s", code, out, errs)
	}
	if devs, _ := st.Devices(); len(devs) != 0 {
		t.Errorf("still paired: %+v", devs)
	}
}

func TestSystemModeSAS(t *testing.T) {
	ph := startPhone(t)
	env, st, _ := systemEnv(t, ph)
	pc := startPair(t, env, strings.NewReader("y\n"))
	u := pc.uri(t)
	var phoneCode string
	paired, err := pairing.PairSAS(tctx(t), net.JoinHostPort("127.0.0.1", strconv.Itoa(u.Port)), ph.info(), security.NewToken(),
		func(_ context.Context, _ protocol.PCInfo, code string) (bool, error) {
			phoneCode = code
			return true, nil
		})
	if err != nil {
		t.Fatalf("%v\n%s", err, pc.out.String())
	}
	pc.wait(t)
	if pc.code != 0 || !strings.Contains(pc.out.String(), "Does the phone show "+security.FormatCode(phoneCode)+"?") {
		t.Fatalf("pair: %d\n%s\n%s", pc.code, pc.out.String(), pc.errb.String())
	}
	if dev, err := st.Find(""); err != nil || dev.Token != paired.Token {
		t.Fatalf("stored %+v %v", dev, err)
	}
}

// TestSystemModeExpires: the daemon's errors come out as phonetopc's own.
func TestSystemModeExpires(t *testing.T) {
	env, _, _ := systemEnv(t, startPhone(t))
	code, _, errs := runCLI(t, tctx(t), env, "pair", "--listen", "127.0.0.1:0", "--timeout", "1s")
	if code != 1 || !strings.Contains(errs, "no phone paired within 1s") {
		t.Errorf("code %d: %s", code, errs)
	}
}
