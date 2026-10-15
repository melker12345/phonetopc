package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// defaultPairListen is replaced in tests.
const defaultPairPort = pairing.DefaultPort

var defaultPairListen = fmt.Sprintf(":%d", defaultPairPort)

func (c *cli) storeDir() (string, error) { return store.DefaultDir(c.getenv) }

func (c *cli) store() (*store.Store, error) {
	dir, err := c.storeDir()
	if err != nil {
		return nil, err
	}
	return store.Open(dir), nil
}

// hostList is a repeatable, comma-separated flag.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }
func (h *hostList) Set(s string) error {
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			*h = append(*h, strings.Trim(x, "[]"))
		}
	}
	return nil
}

func (c *cli) pair(ctx context.Context, args []string) error {
	fs := c.flagSet("pair")
	yes := fs.Bool("yes", false, "")
	png := fs.String("png", "", "")
	timeout := fs.Duration("timeout", 5*time.Minute, "")
	listen := fs.String("listen", "", "")
	var hosts hostList
	fs.Var(&hosts, "host", "")
	if _, err := parse(fs, args, 0, ""); err != nil {
		return err
	}
	if *timeout <= 0 {
		return usageErr{errors.New("--timeout must be positive")}
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	pc, err := st.PC()
	if err != nil {
		return fmt.Errorf("loading this PC's identity: %w", err)
	}

	ln, randomPort, err := listenPair(*listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if randomPort {
		fmt.Fprintf(c.stderr, "phonetopc: port %d is busy; listening on random port %d instead\n", defaultPairPort, port)
	}
	if len(hosts) == 0 {
		hosts = qrHosts(ln.Addr().(*net.TCPAddr).IP)
	}
	if len(hosts) == 0 {
		fmt.Fprintln(c.stderr, "phonetopc: warning: no LAN address found; the QR code only works on this machine")
		hosts = []string{"127.0.0.1"}
	}

	showQR := func(secret string) error {
		uri := pairing.URI{ID: pc.ID, Name: pc.Name, Hosts: hosts, Port: port, Pin: pc.Pin, Secret: secret}.String()
		if err := writeQR(c.stdout, uri, c.getenv("NO_COLOR") == ""); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "\n    %s\n\n", uri)
		if *png != "" {
			if err := writePNG(*png, uri); err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "    (also written to %s)\n\n", *png)
		}
		return nil
	}

	prompt := newTermPrompter(c.stdin, c.stdout, *yes, isTerminal(c.stdin))
	prompt.replaces = func(phone pairing.PhoneInfo) (store.Device, bool) { return replaces(st, phone) }
	l, err := pairing.New(pairing.Options{
		PC:     protocol.PCInfo{ID: pc.ID, Name: pc.Name},
		Cert:   pc.Cert,
		Prompt: prompt,
		NewSecret: func(secret string) {
			fmt.Fprintf(c.stdout, "\nThe QR code changed (the last one was used without pairing, maybe from a photo). Scan this one:\n\n")
			if err := showQR(secret); err != nil {
				fmt.Fprintf(c.stderr, "phonetopc: %v\n", err)
			}
		},
		// --yes can't compare codes, so Find on network is off (SECURITY.md §4.3).
		NoSAS:  *yes,
		Expiry: *timeout,
		Logf:   func(format string, a ...any) { fmt.Fprintf(c.stderr, "phonetopc: "+format+"\n", a...) },
		Save: func(p pairing.Paired) error {
			addrs := slices.Clone(p.Phone.Addresses)
			if p.RemoteHost != "" && !slices.Contains(addrs, p.RemoteHost) {
				addrs = append(addrs, p.RemoteHost)
			}
			return st.PutDevice(store.Device{
				ID: p.Phone.ID, Name: p.Phone.Name, Pin: p.Phone.Pin, Token: p.Token,
				Port: p.Phone.Port, Addresses: addrs, PairedAt: store.Now(time.Now()),
			})
		},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(c.stdout, "Pairing %q. On the phone, open phonetopc → Pair a PC, then either:\n\n", pc.Name)
	fmt.Fprintln(c.stdout, "  • Scan QR code:")
	fmt.Fprintln(c.stdout)
	if err := showQR(l.Secret()); err != nil {
		return err
	}

	var adv *discovery.Advertiser
	if *yes {
		fmt.Fprintln(c.stdout, "  (--yes: only the QR code works; \"Find on network\" needs you to compare codes.)")
		fmt.Fprintln(c.stdout)
	} else if !c.noMDNS {
		adv, err = discovery.Advertise(pc.Name, discovery.ServicePair, port, map[string]string{"id": pc.ID, "n": pc.Name, "v": "1"})
		if err != nil {
			fmt.Fprintf(c.stderr, "phonetopc: mDNS unavailable (%v); use the QR code\n", err)
		}
	}
	if adv != nil {
		defer adv.Stop()
		fmt.Fprintf(c.stdout, "  • Or tap Find on network → %q, and compare the 6-digit codes.\n\n", pc.Name)
	}
	writeFirewallHint(c.stdout, detectFirewalls(), port, discovery.LANPrefixes(), randomPort)
	fmt.Fprintf(c.stdout, "This PC's pin: %s\nWaiting for the phone (listening on port %d, for %v; Ctrl-C to cancel)…\n", pc.Pin, port, *timeout)

	res, err := l.Serve(ctx, ln)
	switch {
	case errors.Is(err, pairing.ErrExpired):
		return fmt.Errorf("no phone paired within %v; run `phonetopc pair` again", *timeout)
	case errors.Is(err, pairing.ErrSASLimit):
		return errors.New("pairing stopped: too many code comparisons failed or were abandoned " +
			"(someone may be trying to pair in your phone's place); run `phonetopc pair` again, or scan the QR code")
	case err != nil:
		return err
	}
	how := "QR code"
	if res.Way == pairing.WaySAS {
		how = "code comparison"
	}
	fmt.Fprintf(c.stdout, "\nPaired with %q (id %s) by %s.\nTry: phonetopc info\n", res.Phone.Name, res.Phone.ID, how)
	return nil
}

// replaces returns the stored device that pairing phone would replace with a
// different key: the phone was reset or reinstalled, or someone else claims
// its id.
func replaces(st *store.Store, phone pairing.PhoneInfo) (store.Device, bool) {
	devs, err := st.Devices()
	if err != nil {
		return store.Device{}, false // Save reports a broken store
	}
	i := slices.IndexFunc(devs, func(d store.Device) bool { return d.ID == phone.ID })
	if i < 0 || devs[i].Pin == phone.Pin {
		return store.Device{}, false
	}
	return devs[i], true
}

// listenPair opens the pairing listener's socket: on addr if given,
// otherwise on the default port 8766, falling back to a random port if that
// is busy (randomPort true).
func listenPair(addr string) (ln net.Listener, randomPort bool, err error) {
	if addr != "" {
		ln, err = net.Listen("tcp", addr)
		return ln, false, err
	}
	if ln, err = net.Listen("tcp", defaultPairListen); err == nil {
		return ln, false, nil
	}
	host, _, _ := net.SplitHostPort(defaultPairListen)
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

func (c *cli) devices(args []string) error {
	if _, err := parse(c.flagSet("devices"), args, 0, ""); err != nil {
		return err
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	devs, err := st.Devices()
	if err != nil {
		return err
	}
	type pub struct { // devices.json without the token
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Pin       string   `json:"pin"`
		Port      int      `json:"port"`
		Addresses []string `json:"addresses"`
		PairedAt  string   `json:"paired_at"`
	}
	if c.json {
		out := make([]pub, len(devs))
		for i, d := range devs {
			out[i] = pub{d.ID, d.Name, d.Pin, d.Port, d.Addresses, d.PairedAt}
		}
		return c.printJSON(map[string]any{"devices": out})
	}
	if len(devs) == 0 {
		fmt.Fprintln(c.stdout, "No paired phones. Run `phonetopc pair`.")
		return nil
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tADDRESSES\tPAIRED")
	for _, d := range devs {
		addrs := make([]string, len(d.Addresses))
		for i, a := range d.Addresses {
			addrs[i] = net.JoinHostPort(a, fmt.Sprint(d.Port))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, d.ID, strings.Join(addrs, ","), d.PairedAt)
	}
	return tw.Flush()
}

func (c *cli) unpair(args []string) error {
	pos, err := parse(c.flagSet("unpair"), args, 1, "device")
	if err != nil {
		return err
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	d, err := st.RemoveDevice(pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Forgot %q (id %s).\nAlso revoke this PC on the phone (Paired devices), so its token stops working there.\n", d.Name, d.ID)
	return nil
}

func (c *cli) pc(args []string) error {
	if _, err := parse(c.flagSet("pc"), args, 0, ""); err != nil {
		return err
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	pc, err := st.PC()
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(map[string]string{"id": pc.ID, "name": pc.Name, "pin": pc.Pin, "dir": st.Dir()})
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id\t%s\nname\t%s\npin\t%s\nstore\t%s\n", pc.ID, pc.Name, pc.Pin, st.Dir())
	return tw.Flush()
}

// termPrompter asks pairing questions on the terminal. Lines are read by
// one goroutine for the whole run, so a prompt abandoned because the phone
// went away doesn't leave a reader behind.
type termPrompter struct {
	out io.Writer
	yes bool // confirm QR pairings without asking; never codes
	in  io.Reader
	// fresh drops lines typed while no question is waiting (stdin is a
	// terminal): a stray Enter pressed while waiting for the phone must not
	// answer a later question. Piped answers are kept for the next one.
	fresh    bool
	replaces func(pairing.PhoneInfo) (store.Device, bool)

	once sync.Once
	ch   chan string // closed at EOF
	mu   sync.Mutex  // one prompt at a time
}

func newTermPrompter(in io.Reader, out io.Writer, yes, fresh bool) *termPrompter {
	p := &termPrompter{in: in, out: out, yes: yes, fresh: fresh}
	if fresh {
		p.lines() // read (and drop) from now on, not from the first question
	}
	return p
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (p *termPrompter) lines() <-chan string {
	p.once.Do(func() {
		p.ch = make(chan string)
		go func() {
			defer close(p.ch)
			if p.in == nil {
				return
			}
			sc := bufio.NewScanner(p.in)
			for sc.Scan() {
				if !p.fresh {
					p.ch <- sc.Text()
					continue
				}
				select {
				case p.ch <- sc.Text():
				default: // nobody asked yet
				}
			}
		}()
	})
	return p.ch
}

// def is the answer for an empty line. At EOF the answer is no.
func (p *termPrompter) ask(ctx context.Context, question string, def bool) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		fmt.Fprintf(p.out, "%s ", question)
		select {
		case <-ctx.Done():
			fmt.Fprintln(p.out, "\n(the phone disconnected or pairing timed out)")
			return false, ctx.Err()
		case line, ok := <-p.lines():
			if !ok {
				fmt.Fprintln(p.out)
				return false, nil
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "":
				return def, nil
			case "y", "yes":
				return true, nil
			case "n", "no":
				return false, nil
			}
		}
	}
}

func (p *termPrompter) replacing(phone pairing.PhoneInfo) bool {
	if p.replaces == nil {
		return false
	}
	old, ok := p.replaces(phone)
	if !ok {
		return false
	}
	fmt.Fprintf(p.out, "\nWarning: %q (id %s) is already paired, with a different key.\n"+
		"Pairing replaces it, and this PC then trusts the new key, also for `phonetopc approve`.\n"+
		"Accept only if phonetopc was reset or reinstalled on that phone.\n", old.Name, old.ID)
	return true
}

func (p *termPrompter) ConfirmQR(ctx context.Context, phone pairing.PhoneInfo) (bool, error) {
	// No default: whoever photographed the QR code may be the one asking.
	question := fmt.Sprintf("\nPair with %q? [y/N]", phone.Name)
	if p.replacing(phone) {
		if p.yes {
			fmt.Fprintln(p.out, "Refused: --yes never replaces a phone paired with a different key; pair without --yes.")
			return false, nil
		}
		return p.ask(ctx, fmt.Sprintf("Replace %q? [y/N]", phone.Name), false)
	}
	if p.yes {
		p.mu.Lock()
		defer p.mu.Unlock()
		fmt.Fprintf(p.out, "%s y (--yes)\n", question)
		return true, nil
	}
	return p.ask(ctx, question, false)
}

func (p *termPrompter) ConfirmSAS(ctx context.Context, phone pairing.PhoneInfo, code string) (bool, error) {
	if p.yes { // the listener refuses SAS under --yes already
		fmt.Fprintf(p.out, "\nRefused a code comparison with %q: --yes can't compare codes.\n", phone.Name)
		return false, nil
	}
	p.replacing(phone)
	fmt.Fprintf(p.out, "\n%q wants to pair. Pairing code:  %s\n", phone.Name, security.FormatCode(code))
	return p.ask(ctx, fmt.Sprintf("Does the phone show %s? [y/N]", security.FormatCode(code)), false)
}
