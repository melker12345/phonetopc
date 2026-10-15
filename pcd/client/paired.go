package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// Device is a paired phone, as stored in devices.json (SECURITY.md §6).
type Device = store.Device

// Errors of DialPaired when no device is selected.
var (
	// ErrNoDevices: nothing is paired yet.
	ErrNoDevices = store.ErrNoDevices
	// ErrNotFound: the selector matches no paired device.
	ErrNotFound = store.ErrNotFound
)

// AmbiguousError: several devices are paired and none (or an ambiguous
// name) was selected.
type AmbiguousError = store.AmbiguousError

// WithConfigDir makes DialPaired and PairedDevices use this credential store
// directory instead of $XDG_CONFIG_HOME/phonetopc.
func WithConfigDir(dir string) Option { return func(o *options) { o.configDir = dir } }

// WithDiscovery turns the mDNS lookup of DialPaired on or off (default on)
// and sets how long it waits for an answer (default 1.5s).
func WithDiscovery(enabled bool, timeout time.Duration) Option {
	return func(o *options) {
		o.discovery = enabled
		if timeout > 0 {
			o.discoveryTimeout = timeout
		}
	}
}

// WithAddress makes DialPaired connect to host:port instead of looking for
// the phone. The pin and token of the selected device are still used.
func WithAddress(hostport string) Option { return func(o *options) { o.addrOverride = hostport } }

func pairedOptions(opts []Option) options {
	o := options{discovery: true, discoveryTimeout: 1500 * time.Millisecond}
	for _, f := range opts {
		f(&o)
	}
	return o
}

func openStore(o options) (*store.Store, error) {
	dir := o.configDir
	if dir == "" {
		var err error
		if dir, err = store.DefaultDir(os.Getenv); err != nil {
			return nil, err
		}
	}
	return store.Open(dir), nil
}

// PairedDevices lists the phones in the credential store without contacting
// them; WithConfigDir selects the store. Nothing paired yet is an empty
// list, not an error.
func PairedDevices(opts ...Option) ([]Device, error) {
	st, err := openStore(pairedOptions(opts))
	if err != nil {
		return nil, err
	}
	return st.Devices()
}

// DialPaired connects securely to a paired phone. selector is a device id
// or name, or "" for the only paired phone.
//
// It looks for the phone with mDNS (_phonetopc._tcp with TXT id equal to
// the device id), then tries the stored addresses in order, and records
// where it found the phone. Every connection verifies the stored pin; if
// the phone can't be reached anywhere and some address presented a
// different key, the error matches ErrPinMismatch. A -32010 answer is
// returned at once (IsUnauthorized): the phone revoked this PC.
func DialPaired(ctx context.Context, selector string, opts ...Option) (*Client, error) {
	o := pairedOptions(opts)
	st, err := openStore(o)
	if err != nil {
		return nil, err
	}
	dev, err := st.Find(selector)
	if err != nil {
		return nil, fmt.Errorf("phonetopc: %w", err)
	}

	type candidate struct {
		host string
		port int
		mdns bool
	}
	var cands []candidate
	add := func(host string, port int, mdns bool) {
		for _, c := range cands {
			if c.host == host && c.port == port {
				return
			}
		}
		cands = append(cands, candidate{host, port, mdns})
	}
	if o.addrOverride != "" {
		host, ps, err := net.SplitHostPort(o.addrOverride)
		port, perr := strconv.Atoi(ps)
		if err != nil || perr != nil {
			return nil, fmt.Errorf("phonetopc: bad address %q: want host:port", o.addrOverride)
		}
		add(host, port, false)
	} else {
		if o.discovery {
			dctx, cancel := context.WithTimeout(ctx, o.discoveryTimeout)
			e, ok, _ := discovery.FindPhone(dctx, dev.ID)
			cancel()
			if ok {
				for _, a := range e.Addrs {
					add(a, e.Port, true)
				}
			}
		}
		for _, a := range dev.Addresses {
			add(a, dev.Port, false)
		}
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("phonetopc: no address known for %q and mDNS found nothing; is the phone on this network?", dev.Name)
	}

	// The store decides about approval keys unless the caller does.
	dialOpts := append(append([]Option{WithApprovalTrust(storeApprovalTrust(st, dev))}, opts...), WithPin(dev.Pin), WithToken(dev.Token))
	var (
		pinErr  error
		lastErr error
		tried   []string
	)
	for _, c := range cands {
		hp := net.JoinHostPort(c.host, strconv.Itoa(c.port))
		tried = append(tried, hp)
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cl, err := Dial(actx, SecureURL(hp), dialOpts...)
		cancel()
		if err == nil {
			if o.addrOverride == "" {
				_ = st.NoteAddress(dev.ID, c.host, c.port, c.mdns) // best effort
			}
			if got := cl.Server().Device.ID; got != dev.ID {
				// SECURITY.md §6: warn; the pin already proved the key.
				cl.warn(fmt.Sprintf("the phone paired as %q (id %s) now reports device id %s", dev.Name, dev.ID, got))
			}
			// APPROVAL.md §3: the first sight of the approval key stores it.
			// A changed key is reported by RequestApproval and
			// CheckApprovalKey.
			_, _ = cl.CheckApprovalKey()
			return cl, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if IsUnauthorized(err) {
			return nil, err
		}
		if errors.Is(err, ErrPinMismatch) && pinErr == nil {
			pinErr = err
		}
		lastErr = err
	}
	if pinErr != nil {
		return nil, pinErr
	}
	return nil, fmt.Errorf("phonetopc: cannot reach %q (tried %s): %w", dev.Name, strings.Join(tried, ", "), lastErr)
}
