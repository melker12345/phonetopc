package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Host firewall detection for the pairing hint (SECURITY.md §4.1). It is
// cheap and needs no privileges: `ufw status` needs root, so ufw is
// detected by its configuration file, and firewalld by its systemd unit.

type firewall string

const (
	fwUFW       firewall = "ufw"
	fwFirewalld firewall = "firewalld"
)

// firewallProbe is what detection looks at; tests replace it.
type firewallProbe struct {
	readFile   func(name string) ([]byte, error)
	unitActive func(unit string) bool
}

// detectFirewalls is replaced in tests.
var detectFirewalls = func() []firewall { return hostProbe().detect() }

func hostProbe() firewallProbe {
	return firewallProbe{readFile: os.ReadFile, unitActive: systemdUnitActive}
}

func systemdUnitActive(unit string) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil
}

func (p firewallProbe) detect() []firewall {
	var out []firewall
	if b, err := p.readFile("/etc/ufw/ufw.conf"); err == nil {
		// ufw.service is "active" even with ufw disabled on some systems;
		// ENABLED=yes is what `ufw enable` writes.
		if ufwEnabled(b) {
			out = append(out, fwUFW)
		}
	} else if p.unitActive("ufw") {
		out = append(out, fwUFW)
	}
	if p.unitActive("firewalld") {
		out = append(out, fwFirewalld)
	}
	return out
}

func ufwEnabled(conf []byte) bool {
	sc := bufio.NewScanner(strings.NewReader(string(conf)))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == "ENABLED" {
			return strings.EqualFold(strings.Trim(strings.TrimSpace(v), `"'`), "yes")
		}
	}
	return false
}

// writeFirewallHint prints how to let phones on the LAN reach the pairing
// listener on port, for each detected firewall. lans are the LAN networks
// ("192.168.1.0/24"); without any the hint allows every source.
func writeFirewallHint(w io.Writer, fws []firewall, port int, lans []string, randomPort bool) {
	if len(fws) == 0 {
		return
	}
	names := make([]string, len(fws))
	for i, f := range fws {
		names[i] = string(f)
	}
	fmt.Fprintf(w, "A host firewall (%s) is active and may block the phone. To allow pairing from your LAN, once:\n", strings.Join(names, ", "))
	for _, f := range fws {
		switch f {
		case fwUFW:
			if len(lans) == 0 {
				fmt.Fprintf(w, "    sudo ufw allow %d/tcp\n", port)
			}
			for _, lan := range lans {
				fmt.Fprintf(w, "    sudo ufw allow from %s to any port %d proto tcp\n", lan, port)
			}
			for _, lan := range lans {
				fmt.Fprintf(w, "    sudo ufw allow from %s to any port 5353 proto udp   # optional: \"Find on network\" (mDNS)\n", lan)
			}
		case fwFirewalld:
			fmt.Fprintf(w, "    sudo firewall-cmd --permanent --add-port=%d/tcp && sudo firewall-cmd --reload\n", port)
			fmt.Fprintln(w, "    sudo firewall-cmd --permanent --add-service=mdns && sudo firewall-cmd --reload   # optional: \"Find on network\"")
		}
	}
	if randomPort {
		fmt.Fprintf(w, "  (Port %d is random this time because %d was busy; free %d to use the rule permanently.)\n", port, defaultPairPort, defaultPairPort)
	}
	fmt.Fprintln(w)
}
