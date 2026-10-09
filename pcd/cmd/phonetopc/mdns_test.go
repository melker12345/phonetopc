package main

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// TestMDNS pairs by code comparison with the listener found via mDNS, then
// finds the phone via mDNS too (no stored address works). Multicast is
// often unavailable in CI sandboxes, so the test skips when nothing is
// found; TestPairSASThenStream covers the same flow without mDNS.
func TestMDNS(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS")
	}
	env := testEnv(t, nil)
	delete(env, "PHONETOPC_NO_MDNS")
	ph := startPhoneOn(t, ":0") // reachable at the addresses mDNS announces
	pc := startPair(t, env, nil, "--yes")
	u := pc.uri(t)

	var target string
	bctx, cancel := context.WithTimeout(tctx(t), 4*time.Second)
	discovery.Browse(bctx, discovery.ServicePair, func(e discovery.Entry) bool {
		if e.TXT["id"] == u.ID && e.TXT["v"] == "1" && e.TXT["n"] == u.Name && e.Port == u.Port {
			target = net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Port))
			return true
		}
		return false
	})
	cancel()
	if target == "" {
		t.Skip("mDNS not working here")
	}
	paired, err := pairing.PairSAS(tctx(t), target, ph.info(), security.NewToken(),
		func(context.Context, protocol.PCInfo, string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	ph.state.Record(paired)
	pc.wait(t)

	// Make the stored address useless; advertise the phone instead.
	st := store.Open(filepath.Join(env["XDG_CONFIG_HOME"], "phonetopc"))
	d, _ := st.Find("")
	d.Addresses = []string{"127.0.0.2"}
	st.PutDevice(d)
	adv, err := discovery.Advertise("Test Phone "+ph.id.ID, discovery.ServicePhone, ph.port, map[string]string{"id": ph.id.ID, "proto": "1", "tls": "1"})
	if err != nil {
		t.Skipf("cannot advertise: %v", err)
	}
	defer adv.Stop()
	code, out, errs := runCLI(t, tctx(t), env, "info")
	if code != 0 || !strings.Contains(out, "paired as") {
		t.Fatalf("info via mDNS: %d\n%s\n%s", code, out, errs)
	}
	if d, _ := st.Find(""); d.Addresses[0] == "127.0.0.2" {
		t.Errorf("address not updated after mDNS: %v", d.Addresses)
	}
}
