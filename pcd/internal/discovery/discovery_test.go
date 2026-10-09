package discovery

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/security"
)

func TestLANAddresses(t *testing.T) {
	for _, a := range LANAddresses() {
		ip, err := netip.ParseAddr(a)
		if err != nil || !usable(ip) {
			t.Errorf("unusable address %q", a)
		}
	}
}

func TestLANPrefixes(t *testing.T) {
	for _, s := range LANPrefixes() {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Masked() != p {
			t.Errorf("bad prefix %q", s)
		}
	}
}

func TestInstanceName(t *testing.T) {
	if got := instanceName(strings.Repeat("ä", 40)); len(got) != 62 {
		t.Errorf("len %d", len(got))
	}
	if instanceName("  ") != "phonetopc" {
		t.Error("empty name")
	}
}

// TestAdvertiseAndBrowse needs working multicast, which CI sandboxes often
// lack; it skips instead of failing when nothing is found.
func TestAdvertiseAndBrowse(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS")
	}
	id := security.NewID()
	adv, err := Advertise("phonetopc test "+id, ServicePhone, 18765, map[string]string{"id": id, "proto": "1", "tls": "1"})
	if err != nil {
		t.Skipf("cannot advertise: %v", err)
	}
	defer adv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	e, ok, err := FindPhone(ctx, id)
	if err != nil || !ok {
		t.Skipf("mDNS not working here (err %v)", err)
	}
	if e.Port != 18765 || e.TXT["tls"] != "1" || len(e.HostPorts()) == 0 {
		t.Errorf("entry %+v", e)
	}
}
