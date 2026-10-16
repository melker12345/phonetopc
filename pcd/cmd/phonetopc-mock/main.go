// Command phonetopc-mock is a mock phonetopc phone, so clients can be
// developed and tested without a phone.
//
//	phonetopc-mock [flags]                          serve (dev mode, or secure with --state)
//	phonetopc-mock pair --state DIR <URI>           pair by QR code (paste the phonetopc://pair?... URI)
//	phonetopc-mock pair --state DIR --discover      pair by code comparison, finding the PC with mDNS
//	phonetopc-mock pair --state DIR --pc-addr H:P   the same without mDNS
//	phonetopc-mock pcs --state DIR                  list paired PCs
//	phonetopc-mock revoke --state DIR <pc>          revoke a PC
//	phonetopc-mock approval-key --state DIR [--rotate]  show or replace the approval key
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/attest/attesttest"
	"github.com/melker12345/phonetopc/pcd/internal/discovery"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

const usage = `Usage:
  phonetopc-mock [flags]                         serve a mock phone
  phonetopc-mock pair --state DIR <URI>          pair by QR code: pass the phonetopc://pair?... URI
  phonetopc-mock pair --state DIR --discover     pair by code comparison, finding the PC with mDNS
  phonetopc-mock pair --state DIR --pc-addr H:P  the same, with the pairing listener's address
  phonetopc-mock pcs --state DIR                 list paired PCs
  phonetopc-mock revoke --state DIR <pc>         revoke a paired PC (by name or id)
  phonetopc-mock approval-key --state DIR [--rotate]
                                                 show the approval key, or replace it
                                                 (as when fingerprints change)

Serving is plaintext dev mode (ws://) unless --state is given, which
selects secure mode (wss:// with the mock's own key, tokens checked).
In secure mode the mock offers fingerprint approval and decryption;
--approval says how its "user" answers: auto (approve), deny, cancel, timeout (never answer),
lockout, ask (on this terminal), tamper (approve with a corrupted
signature, to test clients) or off (neither capability). Its keys are
attested by a test root made at start, which PCs don't trust: accept
them with phonetopc approval-key --accept --unattested.

Notifications and panels (spec/PANELS.md) are allowed for every PC. A
simulated user (--user-every) opens the newest panel, uses one of its
widgets per tick (press, toggle, slide, submit text, draw a circle) and
answers notifications with their first action, so a client sees events
without a phone.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:]
	var err error
	if len(args) > 0 {
		switch args[0] {
		case "pair":
			err = pairCmd(ctx, args[1:], os.Stdin, os.Stdout)
		case "pcs":
			err = pcsCmd(args[1:], os.Stdout)
		case "revoke":
			err = revokeCmd(args[1:], os.Stdout)
		case "approval-key":
			err = approvalKeyCmd(args[1:], os.Stdout)
		case "help":
			fmt.Print(usage)
			return
		default:
			err = serve(ctx, args)
		}
	} else {
		err = serve(ctx, args)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phonetopc-mock:", err)
		os.Exit(1)
	}
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage+"\nFlags of "+name+":\n")
		fs.PrintDefaults()
	}
	return fs
}

func serve(ctx context.Context, args []string) error {
	fs := newFlags("phonetopc-mock")
	var (
		addr        = fs.String("addr", fmt.Sprintf("127.0.0.1:%d", protocol.DefaultPort), "listen address")
		tapEvery    = fs.Duration("tap-every", 10*time.Second, "interval of synthetic ui/tile_tap events (0 disables)")
		nfcEvery    = fs.Duration("nfc-every", 20*time.Second, "mean interval of synthetic nfc/tag scans (0 disables)")
		userEvery   = fs.Duration("user-every", 4*time.Second, "interval of the simulated user, who opens panels, uses their widgets and answers notifications (0 disables)")
		noNotify    = fs.Bool("no-notify", false, "act as if the user hasn't allowed notifications (notify fails with -32030)")
		noPanels    = fs.Bool("no-panels", false, "act as if the user hasn't allowed panels (panel methods fail with -32030)")
		deviceID    = fs.String("device-id", "6d6f636b00000001", "device.id (dev mode; in secure mode only when the state is created)")
		name        = fs.String("name", "", "device name (default \"phonetopc mock\")")
		unavailable = fs.String("unavailable", "", "comma-separated sources to report as unavailable (e.g. light,proximity)")
		seed        = fs.Uint64("seed", 0, "random seed for the simulation (0 = random)")
		verbose     = fs.Bool("v", false, "log every frame")
		stateDir    = fs.String("state", "", "state directory (key, paired PCs); selects secure mode")
		secure      = fs.Bool("secure", false, "secure mode: wss:// and tokens (needs --state; the default with --state)")
		dev         = fs.Bool("dev", false, "dev mode: plaintext ws://, no authentication (the default without --state)")
		mdns        = fs.Bool("mdns", false, "advertise _phonetopc._tcp via mDNS")
		approval    = fs.String("approval", "auto", "secure mode: how approval requests are answered: "+approvalModes)
		approvalDly = fs.Duration("approval-delay", time.Second, "how long the \"user\" takes to answer for auto, deny, cancel, lockout and tamper")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return flag.ErrHelp
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	switch {
	case *dev && *secure:
		return errors.New("--dev and --secure exclude each other")
	case *secure && *stateDir == "":
		return errors.New("--secure needs --state DIR (where the mock keeps its key and paired PCs)")
	}
	isSecure := *secure || (*stateDir != "" && !*dev)

	opts := mockphone.Options{
		Device:    protocol.DeviceInfo{ID: *deviceID, Name: *name},
		Seed:      *seed,
		TapEvery:  *tapEvery,
		NFCEvery:  *nfcEvery,
		UserEvery: *userEvery,
		NoNotify:  *noNotify,
		NoPanels:  *noPanels,
		Logf:      log.Printf,
	}
	if *tapEvery == 0 {
		opts.TapEvery = -1
	}
	if *userEvery == 0 {
		opts.UserEvery = -1
	}
	if *nfcEvery == 0 {
		opts.NFCEvery = -1
	}
	for _, s := range strings.Split(*unavailable, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, ok := protocol.LookupSensor(s); !ok {
			return fmt.Errorf("--unavailable: %q is not in the catalog", s)
		}
		opts.Unavailable = append(opts.Unavailable, s)
	}
	if *verbose {
		opts.Tap = func(dir mockphone.Direction, frame []byte) { log.Printf("%s %s", dir, frame) }
	}

	var (
		st    *mockphone.State
		ident mockphone.Identity
	)
	if isSecure {
		var err error
		if st, err = mockphone.OpenState(*stateDir); err != nil {
			return err
		}
		idArg := ""
		if explicit["device-id"] {
			idArg = *deviceID
			if !pairing.ValidID(idArg) {
				return fmt.Errorf("--device-id: in secure mode, an id is 16 to 64 lowercase hex digits (SECURITY.md §2), got %q", idArg)
			}
		}
		if ident, err = st.Identity(idArg, *name); err != nil {
			return err
		}
		opts.Device = protocol.DeviceInfo{ID: ident.ID, Name: ident.Name}
		opts.Authenticate = st.Authenticate
		if err := approvalOptions(&opts, *approval, *approvalDly, st, os.Stdin, os.Stdout); err != nil {
			return err
		}
		ca, err := attesttest.NewCA()
		if err != nil {
			return err
		}
		opts.Attest = mockphone.TestAttestation(ca, ident.Pin, nil)
	} else if explicit["approval"] && *approval != "off" {
		return errors.New("--approval needs secure mode (--state DIR): approvals don't exist in dev mode")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	mock := mockphone.New(opts)
	srv := &http.Server{Handler: mock.Handler(), ReadHeaderTimeout: 10 * time.Second}
	scheme := "ws"
	if isSecure {
		scheme = "wss"
		ln = tls.NewListener(ln, security.ServerTLSConfig(ident.Cert))
		log.Printf("phonetopc-mock %s serving %s://%s%s, secure mode (device %s %q, pin %s, state %s, approvals: %s)",
			version.Version, scheme, ln.Addr(), protocol.WSPath, ident.ID, ident.Name, ident.Pin, *stateDir, *approval)
		// Pairings and revocations may come from other processes
		// (phonetopc-mock pair/revoke): close sessions of revoked PCs.
		go func() {
			changed := st.Changed()
			changed()
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if changed() {
						mock.Recheck()
					}
				}
			}
		}()
	} else {
		log.Printf("phonetopc-mock %s serving %s://%s%s, dev mode: no TLS, no authentication (device %s)",
			version.Version, scheme, ln.Addr(), protocol.WSPath, opts.Device.ID)
	}
	if *mdns {
		txt := map[string]string{"id": mock.DeviceID(), "proto": "1"}
		if isSecure {
			txt["tls"] = "1"
		} else {
			txt["dev"] = "1"
		}
		instance := opts.Device.Name
		if instance == "" {
			instance = "phonetopc mock"
		}
		adv, err := discovery.Advertise(instance, discovery.ServicePhone, ln.Addr().(*net.TCPAddr).Port, txt)
		if err != nil {
			log.Printf("mDNS unavailable: %v", err)
		} else {
			defer adv.Stop()
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Print("shutting down")
	mock.Close() // sends close frames to connected clients
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func openState(dir string) (*mockphone.State, error) {
	if dir == "" {
		return nil, errors.New("--state DIR is required")
	}
	return mockphone.OpenState(dir)
}

func pairCmd(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	fs := newFlags("pair")
	var (
		stateDir = fs.String("state", "", "state directory (required)")
		addr     = fs.String("addr", fmt.Sprintf("127.0.0.1:%d", protocol.DefaultPort), "address the mock serves on, told to the PC")
		name     = fs.String("name", "", "device name")
		discover = fs.Bool("discover", false, "find the PC's pairing listener with mDNS and compare codes")
		pcSel    = fs.String("pc", "", "with --discover: the PC to pair with, by name or id (default: the first found)")
		pcAddr   = fs.String("pc-addr", "", "compare codes with the pairing listener at host:port (no mDNS)")
		confirm  = fs.Bool("confirm", false, "confirm the code without asking")
		timeout  = fs.Duration("timeout", 6*time.Minute, "give up after this long (the PC user may take up to 5 minutes to answer)")
	)
	// Accept the URI before or after the flags.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	ways := len(pos)
	if *discover {
		ways++
	}
	if *pcAddr != "" {
		ways++
	}
	if ways != 1 || len(pos) > 1 {
		fs.Usage()
		return errors.New("give exactly one of: a pairing URI, --discover, --pc-addr")
	}
	st, err := openState(*stateDir)
	if err != nil {
		return err
	}
	ident, err := st.Identity("", *name)
	if err != nil {
		return err
	}
	host, ps, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	port, _ := strconv.Atoi(ps)
	var hosts []string
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		hosts = append(discovery.LANAddresses(), "127.0.0.1")
	} else {
		hosts = []string{host}
	}
	phone := ident.PhoneInfo(port, hosts)
	token := security.NewToken()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var paired pairing.PairedPC
	switch {
	case len(pos) == 1:
		u, err := pairing.ParseURI(pos[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Pairing %q with PC %q by QR code; confirm on the PC…\n", ident.Name, u.Name)
		paired, err = pairing.PairQR(ctx, u, phone, token)
		if err != nil {
			return err
		}
	default:
		targets := []string{*pcAddr}
		if *discover {
			if targets, err = discoverPC(ctx, *pcSel, out); err != nil {
				return err
			}
		}
		confirmer := sasConfirmer(stdin, out, *confirm)
		for i, hp := range targets {
			paired, err = pairing.PairSAS(ctx, hp, phone, token, confirmer)
			var pe *protocol.Error
			if err == nil || errors.As(err, &pe) || errors.Is(err, pairing.ErrCodesDiffer) || errors.Is(err, pairing.ErrCommitment) || i == len(targets)-1 {
				break // only unreachable addresses are worth another try
			}
		}
		if err != nil {
			return err
		}
	}
	if err := st.Record(paired); err != nil {
		return err
	}
	fmt.Fprintf(out, "Paired with PC %q (id %s, pin %s).\nServe it with: phonetopc-mock --state %s --addr %s\n", paired.Name, paired.ID, paired.Pin, *stateDir, *addr)
	return nil
}

func discoverPC(ctx context.Context, sel string, out io.Writer) ([]string, error) {
	fmt.Fprintln(out, "Looking for PCs in pairing mode (_phonetopc-pair._tcp)…")
	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var hit *discovery.Entry
	err := discovery.Browse(bctx, discovery.ServicePair, func(e discovery.Entry) bool {
		fmt.Fprintf(out, "  found %q (id %s) at %s\n", e.TXT["n"], e.TXT["id"], strings.Join(e.HostPorts(), ", "))
		if e.TXT["v"] != "1" || len(e.Addrs) == 0 {
			return false
		}
		if sel == "" || sel == e.TXT["id"] || strings.EqualFold(sel, e.TXT["n"]) {
			hit = &e
			return true
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("mDNS: %w", err)
	}
	if hit == nil {
		return nil, errors.New("no PC in pairing mode found; run `phonetopc pair` on the PC, or use --pc-addr / the QR URI")
	}
	return hit.HostPorts(), nil
}

func sasConfirmer(stdin io.Reader, out io.Writer, auto bool) pairing.SASConfirmer {
	return func(ctx context.Context, pc protocol.PCInfo, code string) (bool, error) {
		fmt.Fprintf(out, "Pairing with %q. Code: %s\n", pc.Name, security.FormatCode(code))
		if auto {
			fmt.Fprintln(out, "Does the PC show the same code? [y/N] y (--confirm)")
			return true, nil
		}
		fmt.Fprint(out, "Does the PC show the same code? [y/N] ")
		lines := make(chan string, 1)
		go func() {
			sc := bufio.NewScanner(stdin)
			if sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case l := <-lines:
			l = strings.ToLower(strings.TrimSpace(l))
			return l == "y" || l == "yes", nil
		}
	}
}

func pcsCmd(args []string, out io.Writer) error {
	fs := newFlags("pcs")
	stateDir := fs.String("state", "", "state directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openState(*stateDir)
	if err != nil {
		return err
	}
	ident, err := st.Identity("", "")
	if err != nil {
		return err
	}
	pcs, err := st.PCs()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "This mock: %q, id %s, pin %s\n", ident.Name, ident.ID, ident.Pin)
	if len(pcs) == 0 {
		fmt.Fprintln(out, "No paired PCs.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tPIN\tPAIRED")
	for _, pc := range pcs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", pc.Name, pc.ID, pc.Pin, pc.PairedAt)
	}
	return tw.Flush()
}

func revokeCmd(args []string, out io.Writer) error {
	fs := newFlags("revoke")
	stateDir := fs.String("state", "", "state directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("revoke needs exactly one PC name or id")
	}
	st, err := openState(*stateDir)
	if err != nil {
		return err
	}
	pc, err := st.RemovePC(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked %q (id %s). A running mock closes its sessions within a second.\n", pc.Name, pc.ID)
	return nil
}
