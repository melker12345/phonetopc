package main

import (
	"bytes"
	"io/fs"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDetectFirewalls(t *testing.T) {
	probe := func(conf string, active ...string) firewallProbe {
		return firewallProbe{
			readFile: func(string) ([]byte, error) {
				if conf == "" {
					return nil, fs.ErrNotExist
				}
				return []byte(conf), nil
			},
			unitActive: func(u string) bool { return slices.Contains(active, u) },
		}
	}
	for _, tc := range []struct {
		name string
		p    firewallProbe
		want []firewall
	}{
		{"nothing", probe(""), nil},
		{"ufw enabled", probe("# comment\nENABLED=yes\nLOGLEVEL=low\n"), []firewall{fwUFW}},
		{"ufw disabled but its unit active", probe("ENABLED=no\n", "ufw"), nil},
		{"no ufw.conf, unit active", probe("", "ufw"), []firewall{fwUFW}},
		{"firewalld", probe("", "firewalld"), []firewall{fwFirewalld}},
		{"both", probe(`ENABLED="yes"`, "firewalld"), []firewall{fwUFW, fwFirewalld}},
	} {
		if got := tc.p.detect(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFirewallHint(t *testing.T) {
	var b bytes.Buffer
	writeFirewallHint(&b, nil, 8766, []string{"192.168.1.0/24"}, false)
	if b.Len() != 0 {
		t.Errorf("hint without a firewall: %q", b.String())
	}
	writeFirewallHint(&b, []firewall{fwUFW}, 8766, []string{"192.168.1.0/24", "10.0.0.0/8"}, false)
	for _, want := range []string{
		"sudo ufw allow from 192.168.1.0/24 to any port 8766 proto tcp\n",
		"sudo ufw allow from 10.0.0.0/8 to any port 8766 proto tcp\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	writeFirewallHint(&b, []firewall{fwFirewalld}, 40000, nil, true)
	if !strings.Contains(b.String(), "--add-port=40000/tcp") || !strings.Contains(b.String(), "8766 was busy") {
		t.Errorf("firewalld hint:\n%s", b.String())
	}
}

func TestPairDefaultPortFallback(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	old := defaultPairListen
	defaultPairListen = busy.Addr().String()
	defer func() { defaultPairListen = old }()

	ln, random, err := listenPair("")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !random || ln.Addr().String() == busy.Addr().String() {
		t.Errorf("want a random port, got %v (random %v)", ln.Addr(), random)
	}
	ln.Close()

	// The default port itself when it is free.
	port := busy.Addr().(*net.TCPAddr).Port
	busy.Close()
	ln, random, err = listenPair("")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if random || ln.Addr().(*net.TCPAddr).Port != port {
		t.Errorf("want port %d, got %v (random %v)", port, ln.Addr(), random)
	}

	// An explicit --listen that fails is an error, without fallback.
	if _, _, err := listenPair(ln.Addr().String()); err == nil {
		t.Error("explicit busy --listen succeeded")
	}
}

func TestPairPrintsFirewallHint(t *testing.T) {
	old := detectFirewalls
	detectFirewalls = func() []firewall { return []firewall{fwUFW} }
	defer func() { detectFirewalls = old }()
	pc := startPair(t, testEnv(t, nil), nil, "--yes")
	u := pc.uri(t)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(pc.out.String(), "Waiting for the phone") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(pc.out.String(), "(ufw) is active") || !strings.Contains(pc.out.String(), "port "+strconv.Itoa(u.Port)) {
		t.Errorf("no firewall hint:\n%s", pc.out.String())
	}
}
