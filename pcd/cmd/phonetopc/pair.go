package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/pairrun"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

const defaultPairPort = pairing.DefaultPort

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

type pairFlags struct {
	yes     bool
	png     string
	timeout time.Duration
	listen  string
	hosts   []string
}

func (c *cli) pair(ctx context.Context, args []string) error {
	fs := c.flagSet("pair")
	var f pairFlags
	fs.BoolVar(&f.yes, "yes", false, "")
	fs.StringVar(&f.png, "png", "", "")
	fs.DurationVar(&f.timeout, "timeout", 5*time.Minute, "")
	fs.StringVar(&f.listen, "listen", "", "")
	var hosts hostList
	fs.Var(&hosts, "host", "")
	if _, err := parse(fs, args, 0, ""); err != nil {
		return err
	}
	f.hosts = hosts
	if f.timeout <= 0 {
		return usageErr{errors.New("--timeout must be positive")}
	}
	api, err := c.system()
	if err != nil {
		return err
	}
	if api != nil {
		return c.pairViaDaemon(ctx, api, f)
	}
	st, err := c.store()
	if err != nil {
		return err
	}
	prompt := newTermPrompter(c.stdin, c.stdout, f.yes, isTerminal(c.stdin))
	s, err := pairrun.Start(pairrun.Options{
		Store: st, Ask: prompt, Yes: f.yes, Listen: f.listen, Hosts: f.hosts, NoMDNS: c.noMDNS, Expiry: f.timeout,
		NewQR: func(uri string) { c.showNewQR(uri, f.png) },
		Logf:  func(format string, a ...any) { fmt.Fprintf(c.stderr, "phonetopc: "+format+"\n", a...) },
	})
	if err != nil {
		return err
	}
	if s.MDNSErr != nil {
		fmt.Fprintf(c.stderr, "phonetopc: mDNS unavailable (%v); use the QR code\n", s.MDNSErr)
	}
	err = c.showPairing(pairShown{
		pcName: s.PC.Name, pcPin: s.PC.Pin, uri: s.URI(), port: s.Port, randomPort: s.RandomPort,
		noLAN: len(s.Hosts) == 0, advertised: s.Advertised,
	}, f)
	if err != nil {
		s.Close()
		return err
	}
	res, err := s.Serve(ctx)
	if err := c.pairDone(err, string(res.Way), res.Phone.Name, res.Phone.ID, f.timeout); err != nil {
		return err
	}
	if !f.yes && isTerminal(c.stdin) && !c.daemonInstalled() {
		fmt.Fprintln(c.stdout)
		if ok, _ := prompt.ask(ctx, "Start phonetopcd automatically when a program uses it? [Y/n]", true); ok {
			return c.installDaemon(ctx)
		}
	}
	return nil
}

// pairShown is what `phonetopc pair` shows once it listens.
type pairShown struct {
	pcName, pcPin, uri string
	port               int
	randomPort, noLAN  bool
	advertised         bool
}

func (c *cli) showPairing(p pairShown, f pairFlags) error {
	if p.randomPort {
		fmt.Fprintf(c.stderr, "phonetopc: port %d is busy; listening on random port %d instead\n", defaultPairPort, p.port)
	}
	if p.noLAN {
		fmt.Fprintln(c.stderr, "phonetopc: warning: no LAN address found; the QR code only works on this machine")
	}
	fmt.Fprintf(c.stdout, "Pairing %q. On the phone, open phonetopc → Pair a PC, then either:\n\n", p.pcName)
	fmt.Fprintln(c.stdout, "  • Scan QR code:")
	fmt.Fprintln(c.stdout)
	if err := c.showQR(p.uri, f.png); err != nil {
		return err
	}
	if f.yes {
		fmt.Fprintln(c.stdout, "  (--yes: only the QR code works; \"Find on network\" needs you to compare codes.)")
		fmt.Fprintln(c.stdout)
	} else if p.advertised {
		fmt.Fprintf(c.stdout, "  • Or tap Find on network → %q, and compare the 6-digit codes.\n\n", p.pcName)
	}
	writeFirewallHint(c.stdout, detectFirewalls(), p.port, discovery.LANPrefixes(), p.randomPort)
	fmt.Fprintf(c.stdout, "This PC's pin: %s\nWaiting for the phone (listening on port %d, for %v; Ctrl-C to cancel)…\n", p.pcPin, p.port, f.timeout)
	return nil
}

func (c *cli) showQR(uri, png string) error {
	if err := writeQR(c.rawStdout, uri, c.getenv("NO_COLOR") == ""); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "\n    %s\n\n", uri)
	if png != "" {
		if err := writePNG(png, uri); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "    (also written to %s)\n\n", png)
	}
	return nil
}

func (c *cli) showNewQR(uri, png string) {
	fmt.Fprintf(c.stdout, "\nThe QR code changed (the last one was used without pairing, maybe from a photo). Scan this one:\n\n")
	if err := c.showQR(uri, png); err != nil {
		fmt.Fprintf(c.stderr, "phonetopc: %v\n", err)
	}
}

// pairDone reports how the pairing ended.
func (c *cli) pairDone(err error, way, name, id string, timeout time.Duration) error {
	switch {
	case errors.Is(err, pairing.ErrExpired):
		return fmt.Errorf("no phone paired within %v; run `phonetopc pair` again", timeout)
	case errors.Is(err, pairing.ErrSASLimit):
		return errors.New("pairing stopped: too many code comparisons failed or were abandoned " +
			"(someone may be trying to pair in your phone's place); run `phonetopc pair` again, or scan the QR code")
	case err != nil:
		return err
	}
	how := "QR code"
	if way == string(pairing.WaySAS) {
		how = "code comparison"
	}
	fmt.Fprintf(c.stdout, "\nPaired with %q (id %s) by %s.\nTry: phonetopc info\n", name, id, how)
	return nil
}

// listedDevice is a devices.json entry without the token.
type listedDevice struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Pin       string   `json:"pin"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	PairedAt  string   `json:"paired_at"`
}

func (c *cli) devices(ctx context.Context, args []string) error {
	if _, err := parse(c.flagSet("devices"), args, 0, ""); err != nil {
		return err
	}
	devs, err := c.listDevices(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(map[string]any{"devices": devs})
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

func (c *cli) listDevices(ctx context.Context) ([]listedDevice, error) {
	api, err := c.system()
	if err != nil {
		return nil, err
	}
	if api != nil {
		return devicesViaDaemon(ctx, api)
	}
	st, err := c.store()
	if err != nil {
		return nil, err
	}
	devs, err := st.Devices()
	if err != nil {
		return nil, err
	}
	out := make([]listedDevice, len(devs))
	for i, d := range devs {
		out[i] = listedDevice{d.ID, d.Name, d.Pin, d.Port, d.Addresses, d.PairedAt}
	}
	return out, nil
}

func (c *cli) unpair(ctx context.Context, args []string) error {
	pos, err := parse(c.flagSet("unpair"), args, 1, "device")
	if err != nil {
		return err
	}
	api, err := c.system()
	if err != nil {
		return err
	}
	var id, name string
	if api != nil {
		if id, name, err = unpairViaDaemon(ctx, api, pos[0]); err != nil {
			return err
		}
	} else {
		st, err := c.store()
		if err != nil {
			return err
		}
		d, err := st.RemoveDevice(pos[0])
		if err != nil {
			return err
		}
		id, name = d.ID, d.Name
	}
	fmt.Fprintf(c.stdout, "Forgot %q (id %s).\nAlso revoke this PC on the phone (Paired devices), so its token stops working there.\n", name, id)
	return nil
}

func (c *cli) pc(ctx context.Context, args []string) error {
	if _, err := parse(c.flagSet("pc"), args, 0, ""); err != nil {
		return err
	}
	api, err := c.system()
	if err != nil {
		return err
	}
	var id, name, pin, dir string
	if api != nil {
		if id, name, pin, err = pcViaDaemon(ctx, api); err != nil {
			return err
		}
		dir = "phonetopcd at " + api.Socket()
	} else {
		st, err := c.store()
		if err != nil {
			return err
		}
		pc, err := st.PC()
		if err != nil {
			return err
		}
		id, name, pin, dir = pc.ID, pc.Name, pc.Pin, st.Dir()
	}
	if c.json {
		return c.printJSON(map[string]string{"id": id, "name": name, "pin": pin, "dir": dir})
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id\t%s\nname\t%s\npin\t%s\nstore\t%s\n", id, name, pin, dir)
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
	fresh bool

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
		if p.fresh {
			// Also drops a line typed but not yet ended with Enter.
			flushInput(p.in)
		}
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

// warnReplacing warns that a pairing replaces old, whose key differs: the
// phone was reset or reinstalled, or someone else claims its id.
func (p *termPrompter) warnReplacing(old *store.Device) {
	fmt.Fprintf(p.out, "\nWarning: %q (id %s) is already paired, with a different key.\n"+
		"Pairing replaces it, and this PC then trusts the new key, also for `phonetopc approve`.\n"+
		"Accept only if phonetopc was reset or reinstalled on that phone.\n", old.Name, old.ID)
}

func (p *termPrompter) ConfirmQR(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device) (bool, error) {
	// No default: whoever photographed the QR code may be the one asking.
	question := fmt.Sprintf("\nPair with %q? [y/N]", phone.Name)
	if replaces != nil {
		p.warnReplacing(replaces)
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

func (p *termPrompter) ConfirmSAS(ctx context.Context, phone pairing.PhoneInfo, replaces *store.Device, code string) (bool, error) {
	if p.yes { // the listener refuses SAS under --yes already
		fmt.Fprintf(p.out, "\nRefused a code comparison with %q: --yes can't compare codes.\n", phone.Name)
		return false, nil
	}
	if replaces != nil {
		p.warnReplacing(replaces)
	}
	fmt.Fprintf(p.out, "\n%q wants to pair. Pairing code:  %s\n", phone.Name, security.FormatCode(code))
	return p.ask(ctx, fmt.Sprintf("Does the phone show %s? [y/N]", security.FormatCode(code)), false)
}
