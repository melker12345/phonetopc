// Package discovery wraps mDNS/DNS-SD (pure Go, no cgo) for the two
// phonetopc services, and lists the PC's LAN addresses:
//
//   - _phonetopc._tcp: a phone's API server (PROTOCOL.md §8), TXT id, proto,
//     tls or dev.
//   - _phonetopc-pair._tcp: a PC's pairing listener (SECURITY.md §4.3), TXT
//     id, n, v.
//
// mDNS is best effort everywhere: callers must work without it.
package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/libp2p/zeroconf/v2"
)

// Service types.
const (
	ServicePhone = "_phonetopc._tcp"
	ServicePair  = "_phonetopc-pair._tcp"
	domain       = "local."
)

// Entry is one resolved service instance.
type Entry struct {
	Instance string
	Host     string // the advertiser's host name
	Port     int
	Addrs    []string // IPv4 first, then IPv6
	TXT      map[string]string
}

// HostPorts returns "host:port" for every address of e.
func (e Entry) HostPorts() []string {
	out := make([]string, len(e.Addrs))
	for i, a := range e.Addrs {
		out[i] = net.JoinHostPort(a, strconv.Itoa(e.Port))
	}
	return out
}

// Advertiser is a running mDNS advertisement.
type Advertiser struct{ srv *zeroconf.Server }

// Advertise announces a service instance on all multicast interfaces until
// Stop is called.
func Advertise(instance, service string, port int, txt map[string]string) (*Advertiser, error) {
	keys := make([]string, 0, len(txt))
	for k := range txt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	records := make([]string, len(keys))
	for i, k := range keys {
		records[i] = k + "=" + txt[k]
	}
	srv, err := zeroconf.Register(instanceName(instance), service, domain, port, records, nil)
	if err != nil {
		return nil, err
	}
	return &Advertiser{srv: srv}, nil
}

// Stop withdraws the advertisement.
func (a *Advertiser) Stop() {
	if a != nil && a.srv != nil {
		a.srv.Shutdown()
	}
}

// instanceName makes a DNS-SD instance label out of a free-form name: at most
// 63 bytes, cut on a rune boundary.
func instanceName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "phonetopc"
	}
	for len(s) > 63 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// Browse looks for instances of service until ctx is done or found returns
// true. Each instance is reported once.
func Browse(ctx context.Context, service string, found func(Entry) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopped := false
		// zeroconf closes entries when Browse returns; keep draining until
		// then so its goroutines never block on a send.
		for e := range entries {
			if stopped || e == nil {
				continue
			}
			if found(convert(e)) {
				stopped = true
				cancel()
			}
		}
	}()
	err := zeroconf.Browse(ctx, service, domain, entries)
	<-done
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// FindPhone browses _phonetopc._tcp for the phone whose TXT id is id. It
// returns false if none answered before ctx was done.
func FindPhone(ctx context.Context, id string) (Entry, bool, error) {
	var hit Entry
	ok := false
	err := Browse(ctx, ServicePhone, func(e Entry) bool {
		if e.TXT["id"] == id && len(e.Addrs) > 0 {
			hit, ok = e, true
		}
		return ok
	})
	return hit, ok, err
}

func convert(e *zeroconf.ServiceEntry) Entry {
	out := Entry{Instance: e.Instance, Host: e.HostName, Port: e.Port, TXT: map[string]string{}}
	for _, t := range e.Text {
		k, v, _ := strings.Cut(t, "=")
		if _, dup := out.TXT[k]; !dup && k != "" {
			out.TXT[k] = v // RFC 6763 §6.4: only the first occurrence counts
		}
	}
	for _, ip := range append(slices.Clone(e.AddrIPv4), e.AddrIPv6...) {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		if a.IsLinkLocalUnicast() || a.IsUnspecified() {
			continue // unusable without a zone
		}
		if s := a.String(); !slices.Contains(out.Addrs, s) {
			out.Addrs = append(out.Addrs, s)
		}
	}
	return out
}

// skippedInterface reports interfaces whose addresses are never reachable
// from a phone: container and VM bridges.
func skippedInterface(name string) bool {
	for _, p := range []string{"docker", "veth", "br-", "virbr", "vboxnet", "vmnet", "cni", "flannel", "podman"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// LANAddresses lists the PC's addresses a phone on the LAN could reach:
// IPv4 first, then IPv6, without loopback, link-local, multicast, or the
// addresses of container and VM bridges.
func LANAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skippedInterface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			pfx, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			ip := pfx.Addr().Unmap()
			if !usable(ip) {
				continue
			}
			if ip.Is4() {
				v4 = append(v4, ip.String())
			} else {
				v6 = append(v6, ip.String())
			}
		}
	}
	return slices.Compact(append(v4, v6...))
}

func usable(ip netip.Addr) bool {
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() && !ip.IsUnspecified()
}
