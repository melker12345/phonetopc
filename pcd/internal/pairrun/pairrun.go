// Package pairrun runs a pairing (SECURITY.md §4) for a credential store:
// the pairing listener, its mDNS advertisement, and the rules about
// replacing a phone that is paired already (§4.4). `phonetopc pair` uses
// it with the user's store, and phonetopcd with its own.
package pairrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// Asker asks the PC's user to confirm. replaces is the stored phone with
// the same id but another key that a yes would replace, or nil.
type Asker interface {
	ConfirmQR(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device) (bool, error)
	ConfirmSAS(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device, code string) (bool, error)
}

// DefaultListen is replaced in tests.
var DefaultListen = fmt.Sprintf(":%d", pairing.DefaultPort)

type Options struct {
	Store *store.Store
	Ask   Asker
	// Yes means the user isn't asked (Ask still reports the QR pairing):
	// Find on network is off, and no stored phone is replaced.
	Yes    bool
	Listen string   // "": DefaultListen, or a random port if that is busy
	Hosts  []string // for the QR code; default: the LAN addresses
	NoMDNS bool
	Expiry time.Duration
	// NewQR receives the QR code's URI after its secret was replaced
	// (pairing.Options.NewSecret).
	NewQR func(uri string)
	Logf  func(format string, args ...any)
	// Saved is called after a pairing was stored.
	Saved func(store.Device)
}

// Session is a pairing listener that is listening.
type Session struct {
	PC         *store.PC
	Port       int
	RandomPort bool     // the default port was busy
	Hosts      []string // in the QR code; empty if no LAN address was found
	Advertised bool     // on mDNS, for Find on network
	MDNSErr    error

	opts  Options
	ln    net.Listener
	l     *pairing.Listener
	adv   *discovery.Advertiser
	guard *guard
}

// Start listens. The caller then shows URI and calls Serve.
func Start(opts Options) (*Session, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	pc, err := opts.Store.PC()
	if err != nil {
		return nil, fmt.Errorf("loading this PC's identity: %w", err)
	}
	ln, random, err := listen(opts.Listen)
	if err != nil {
		return nil, err
	}
	s := &Session{PC: pc, Port: ln.Addr().(*net.TCPAddr).Port, RandomPort: random, Hosts: opts.Hosts, opts: opts, ln: ln}
	if len(s.Hosts) == 0 {
		s.Hosts = qrHosts(ln.Addr().(*net.TCPAddr).IP)
	}
	s.guard = &guard{ask: opts.Ask, st: opts.Store, yes: opts.Yes, shown: map[[2]string]string{}}
	s.l, err = pairing.New(pairing.Options{
		PC:     protocol.PCInfo{ID: pc.ID, Name: pc.Name},
		Cert:   pc.Cert,
		Prompt: s.guard,
		NewSecret: func(secret string) {
			if opts.NewQR != nil {
				opts.NewQR(s.uri(secret))
			}
		},
		// Yes can't compare codes, so Find on network is off (SECURITY.md §4.3).
		NoSAS:  opts.Yes,
		Expiry: opts.Expiry,
		Logf:   opts.Logf,
		Save:   s.save,
	})
	if err != nil {
		ln.Close()
		return nil, err
	}
	if !opts.Yes && !opts.NoMDNS {
		s.adv, s.MDNSErr = discovery.Advertise(pc.Name, discovery.ServicePair, s.Port, map[string]string{"id": pc.ID, "n": pc.Name, "v": "1"})
		s.Advertised = s.MDNSErr == nil
	}
	return s, nil
}

func (s *Session) uri(secret string) string {
	hosts := s.Hosts
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1"}
	}
	return pairing.URI{ID: s.PC.ID, Name: s.PC.Name, Hosts: hosts, Port: s.Port, Pin: s.PC.Pin, Secret: secret}.String()
}

// URI is what the QR code shows now.
func (s *Session) URI() string { return s.uri(s.l.Secret()) }

// Serve runs the listener until a phone has paired (pairing.Listener.Serve)
// and releases everything.
func (s *Session) Serve(ctx context.Context) (pairing.Paired, error) {
	if s.adv != nil {
		defer s.adv.Stop()
	}
	return s.l.Serve(ctx, s.ln)
}

// Close releases a session whose Serve wasn't called.
func (s *Session) Close() {
	if s.adv != nil {
		s.adv.Stop()
	}
	s.ln.Close()
}

func (s *Session) save(p pairing.Paired) error {
	addrs := slices.Clone(p.Phone.Addresses)
	if p.RemoteHost != "" && !slices.Contains(addrs, p.RemoteHost) {
		addrs = append(addrs, p.RemoteHost)
	}
	oldPin, ok := s.guard.wasShown(p.Phone)
	if !ok {
		return errors.New("the pairing was not confirmed")
	}
	d := store.Device{
		ID: p.Phone.ID, Name: p.Phone.Name, Pin: p.Phone.Pin, Token: p.Token,
		Port: p.Phone.Port, Addresses: addrs, PairedAt: store.Now(time.Now()),
	}
	err := s.opts.Store.PutDeviceIf(d, oldPin)
	if errors.Is(err, store.ErrDeviceChanged) {
		s.opts.Logf("not saved: something else changed the pairing with %q (id %s) while you were asked; the phone may try again", p.Phone.Name, p.Phone.ID)
	}
	if err == nil && s.opts.Saved != nil {
		s.opts.Saved(d)
	}
	return err
}

// listen opens the pairing listener's socket: on addr if given, otherwise
// on the default port, falling back to a random one if that is busy.
func listen(addr string) (ln net.Listener, randomPort bool, err error) {
	if addr != "" {
		ln, err = net.Listen("tcp", addr)
		return ln, false, err
	}
	if ln, err = net.Listen("tcp", DefaultListen); err == nil {
		return ln, false, nil
	}
	host, _, _ := net.SplitHostPort(DefaultListen)
	ln, err2 := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err2 != nil {
		return nil, false, err
	}
	return ln, true, nil
}

// qrHosts picks the addresses to put in the QR code for a listener on ip.
func qrHosts(ip net.IP) []string {
	if ip != nil && !ip.IsUnspecified() {
		return []string{ip.String()}
	}
	return discovery.LANAddresses()
}

// guard looks up the phone a pairing would replace before the user is
// asked, and lets save store only what the user agreed to (SECURITY.md
// §4.4).
type guard struct {
	ask Asker
	st  *store.Store
	yes bool

	mu sync.Mutex
	// shown holds, per phone id and pin, the pin stored for that id when
	// the user was asked ("" for none): save replaces only that entry.
	shown map[[2]string]string
}

func (g *guard) replacing(phone pairing.PhoneInfo) *store.Device {
	var old *store.Device
	if devs, err := g.st.Devices(); err == nil { // save reports a broken store
		if i := slices.IndexFunc(devs, func(d store.Device) bool { return d.ID == phone.ID }); i >= 0 {
			old = &devs[i]
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pin := ""
	if old != nil {
		pin = old.Pin
	}
	g.shown[[2]string{phone.ID, phone.Pin}] = pin
	if old == nil || old.Pin == phone.Pin {
		return nil
	}
	return old
}

func (g *guard) wasShown(phone pairing.PhoneInfo) (oldPin string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	oldPin, ok = g.shown[[2]string{phone.ID, phone.Pin}]
	return oldPin, ok
}

func (g *guard) ConfirmQR(ctx context.Context, phone pairing.PhoneInfo) (bool, error) {
	old := g.replacing(phone)
	ok, err := g.ask.ConfirmQR(ctx, phone, old)
	return ok && (old == nil || !g.yes), err
}

func (g *guard) ConfirmSAS(ctx context.Context, phone pairing.PhoneInfo, code string) (bool, error) {
	if g.yes {
		return false, nil
	}
	return g.ask.ConfirmSAS(ctx, phone, g.replacing(phone), code)
}
