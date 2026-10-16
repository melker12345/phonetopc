// Command phonetopc pairs with phonetopc phones and talks to them (or to the
// mock) directly.
//
//	phonetopc pair [--yes] [--png FILE]
//	phonetopc devices | unpair <device> | pc
//	phonetopc [--device D] info | list | get <source> | tail <source> ...
//	phonetopc [--device D] approve "reason" [--timeout 30s] | approval-key [--accept]
//	phonetopc [--device D] notify "title" [--body B] [--action ID:LABEL] [--wait]
//	phonetopc --insecure-dev --addr host:port info ...
//
// Without --insecure-dev every connection is secure (wss:// with the phone's
// pinned key and this PC's token, SECURITY.md); see usage for details.
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/hints"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

const usage = `Usage: phonetopc [flags] <command> [flags] [args]

Commands:
  pair                 pair with a phone (shows a QR code and accepts "Find on network")
  devices              list paired phones
  unpair <device>      forget a paired phone
  pc                   show this PC's id, name and pin
  info                 show the phone's hello information
  list                 list the phone's sensors
  get <source>         read the latest value of a source
  tail <source>        stream events of a source until Ctrl-C
  approve "reason"     ask for approval with a fingerprint on the phone, after
                       picking the number printed here; exit 0 approved, 1 not
                       approved (denied, cancelled, mismatch, timeout, busy,
                       cooldown, lockout, unavailable), 2 error:
                         phonetopc approve "Deploy?" && ./deploy.sh
  approval-key         show the phone's accepted and current approval key, and
                       what the phone's hardware proved about it
  notify "title"       post a notification on the phone and print its id:
                         phonetopc notify "Claude Code is done" --body "3 files changed"
                       (tail panel / tail notification show panel input and
                       answers to notifications)
  daemon install       start phonetopcd whenever a program connects to it
                       (installs and enables systemd user units)
  daemon uninstall     undo daemon install

Flags (accepted before or after the command):
  --device D           paired phone to use, by name or id ($PHONETOPC_DEVICE);
                       needed only when several are paired
  --addr host:port     connect to this address instead of looking for the phone
                       ($PHONETOPC_ADDR)
  --insecure-dev       plaintext ws:// to a phone in developer mode (or the mock
                       without --state); no pairing involved. Default address
                       127.0.0.1:8765 ($PHONETOPC_INSECURE_DEV=1)
  --no-mdns            don't look for the phone with mDNS ($PHONETOPC_NO_MDNS=1)
  --json               print raw JSON instead of human-readable output

pair flags:
  --yes                accept the QR code without asking (for scripts and tests);
                       turns off "Find on network", whose codes need a person,
                       and never replaces a phone paired with another key
  --png FILE           also write the QR code to a PNG file
  --timeout D          give up after D (default 5m)
  --listen ADDR        listen address (default ":8766", all interfaces; a random
                       port if 8766 is busy)
  --host H[,H...]      addresses to put in the QR code (default: all LAN addresses)

approve flags:
  --timeout D          how long the phone waits for the user (default 30s,
                       5s to 2m)

approval-key flags:
  --accept             accept a changed approval key (after fingerprints were
                       added or removed on the phone)
  --unattested         with --accept: accept it even though the phone's hardware
                       can't prove that it needs a fingerprint for every use

notify flags:
  --body TEXT          text under the title (\n starts a new line)
  --id ID              replace the earlier notification with this id
  --action ID:LABEL    add a button (up to 3; LABEL alone derives the id)
  --high               ask for a heads-up notification
  --wait               wait for the user and print what they did: the
                       action's id, "tap" or "dismiss"

tail flags:
  --type T             only events of type T
  --rate HZ            requested rate for continuous sensors
  --min-delta D        suppress events that changed by less than D
  --count N            exit after N events (0 = run until Ctrl-C)

Sequence gaps (dropped events) are reported on stderr.

The phone's hardware must prove (attest) that its approval key needs a fingerprint
for every use. $PHONETOPC_REQUIRE_STRONGBOX=1 also requires StrongBox,
$PHONETOPC_CHECK_REVOCATION=1 checks Google's revocation list (online), and
$PHONETOPC_APP_SIGNING_DIGEST=<hex SHA-256> accepts only an app signed with that
certificate.
`

type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer // termSafe: they print what phones send
	rawStdout      io.Writer // for the QR code's colors
	getenv         func(string) string
	addr           string
	device         string
	dev            bool // --insecure-dev
	noMDNS         bool
	json           bool
}

// run is main without process globals, for testing. It returns the exit
// code: 0 success, 1 runtime error, 2 usage error.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	c := &cli{
		stdin: stdin, stdout: &termSafe{w: stdout}, stderr: &termSafe{w: stderr}, rawStdout: stdout, getenv: getenv,
		addr:   getenv("PHONETOPC_ADDR"),
		device: getenv("PHONETOPC_DEVICE"),
		dev:    envBool(getenv("PHONETOPC_INSECURE_DEV")),
		noMDNS: envBool(getenv("PHONETOPC_NO_MDNS")),
	}

	global := c.flagSet("phonetopc")
	showVersion := global.Bool("version", false, "")
	if err := global.Parse(args); err != nil {
		return c.usageError(err)
	}
	if *showVersion {
		fmt.Fprintln(stdout, "phonetopc", version.Version)
		return 0
	}
	if global.NArg() == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := global.Arg(0), global.Args()[1:]

	var err error
	switch cmd {
	case "info":
		err = c.info(ctx, rest)
	case "list":
		err = c.list(ctx, rest)
	case "get":
		err = c.get(ctx, rest)
	case "tail":
		err = c.tail(ctx, rest)
	case "pair":
		err = c.pair(ctx, rest)
	case "devices":
		err = c.devices(rest)
	case "unpair":
		err = c.unpair(rest)
	case "pc":
		err = c.pc(rest)
	case "approve":
		err = c.approve(ctx, rest)
	case "approval-key":
		err = c.approvalKey(ctx, rest)
	case "notify":
		err = c.notify(ctx, rest)
	case "daemon":
		err = c.daemon(ctx, rest)
	case "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		return c.usageError(fmt.Errorf("unknown command %q", cmd))
	}
	var (
		ue usageErr
		ee exitError
	)
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ee.err != nil {
			fmt.Fprintln(stderr, "phonetopc:", hints.Explain(ee.err))
		}
		return ee.code
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		return 0 // interrupted (Ctrl-C) while connecting or waiting
	case errors.As(err, &ue):
		return c.usageError(err)
	default:
		fmt.Fprintln(stderr, "phonetopc:", hints.Explain(err))
		return 1
	}
}

// attestationRoots replaces Google's attestation roots; only tests set it.
var attestationRoots []*x509.Certificate

func envBool(s string) bool {
	b, err := strconv.ParseBool(s)
	return err == nil && b
}

type usageErr struct{ error }

func (c *cli) usageError(err error) int {
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(c.stderr, "phonetopc:", err)
	}
	fmt.Fprint(c.stderr, usage)
	return 2
}

// The flags here are accepted by every command.
func (c *cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we print our own usage
	fs.StringVar(&c.addr, "addr", c.addr, "")
	fs.StringVar(&c.device, "device", c.device, "")
	fs.BoolVar(&c.dev, "insecure-dev", c.dev, "")
	fs.BoolVar(&c.noMDNS, "no-mdns", c.noMDNS, "")
	fs.BoolVar(&c.json, "json", c.json, "")
	return fs
}

// parse parses flags that may be interspersed with positional arguments
// ("tail gyro --rate 100") and checks the number of positionals.
func parse(fs *flag.FlagSet, args []string, wantPos int, posName string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr{err}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) != wantPos {
		if wantPos == 0 {
			return nil, usageErr{fmt.Errorf("%s takes no arguments", fs.Name())}
		}
		return nil, usageErr{fmt.Errorf("%s needs exactly one %s", fs.Name(), posName)}
	}
	return pos, nil
}

func (c *cli) dial(ctx context.Context) (*client.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info := client.WithClientInfo("phonetopc-cli", version.Version)
	if c.dev {
		addr := c.addr
		if addr == "" {
			addr = fmt.Sprintf("127.0.0.1:%d", protocol.DefaultPort)
		}
		return client.Dial(dctx, client.URL(addr), info)
	}
	dir, err := c.storeDir()
	if err != nil {
		return nil, err
	}
	opts := []client.Option{info, client.WithConfigDir(dir), client.WithDiscovery(!c.noMDNS, 0)}
	if c.addr != "" {
		opts = append(opts, client.WithAddress(c.addr))
	}
	aopts, err := client.AttestationOptionsFromEnv(c.getenv)
	if err != nil {
		return nil, err
	}
	opts = append(opts, aopts...)
	if attestationRoots != nil {
		opts = append(opts, client.WithAttestationRoots(attestationRoots...))
	}
	cl, err := client.DialPaired(dctx, c.device, opts...)
	if err != nil {
		return nil, err
	}
	for _, w := range cl.Warnings() {
		fmt.Fprintln(c.stderr, "phonetopc: warning:", w)
	}
	return cl, nil
}

func (c *cli) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdout, "%s\n", b)
	return err
}

func (c *cli) info(ctx context.Context, args []string) error {
	if _, err := parse(c.flagSet("info"), args, 0, ""); err != nil {
		return err
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	h := cl.Server()
	if c.json {
		return c.printJSON(h)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("protocol", h.Protocol)
	row("server", strings.TrimSpace(h.Server.Name+" "+h.Server.Version))
	row("device", fmt.Sprintf("%s (id %s)", h.Device.Name, h.Device.ID))
	row("manufacturer", h.Device.Manufacturer)
	row("model", h.Device.Model)
	row("platform", strings.TrimSpace(h.Device.Platform+" "+h.Device.PlatformVersion))
	row("capabilities", strings.Join(h.Capabilities, ", "))
	if h.PC != nil {
		row("paired as", fmt.Sprintf("%s (id %s)", h.PC.Name, h.PC.ID))
		row("approval", approvalRow(cl))
	} else {
		row("mode", "dev (plaintext, unauthenticated)")
	}
	return tw.Flush()
}

func (c *cli) list(ctx context.Context, args []string) error {
	if _, err := parse(c.flagSet("list"), args, 0, ""); err != nil {
		return err
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	sensors, err := cl.ListSensors(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(protocol.ListSensorsResult{Sensors: sensors})
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tTYPES\tUNIT\tSHAPE\tMODE\tRATE (Hz)\tAVAILABLE")
	for _, s := range sensors {
		rate := "-"
		if s.Mode == protocol.ModeContinuous {
			rate = fmt.Sprintf("%s-%s (default %s)", num(s.MinRateHz), num(s.MaxRateHz), num(s.DefaultRateHz))
		}
		unit := s.Unit
		if unit == "" {
			unit = "-"
		}
		avail := "yes"
		if !s.Available {
			avail = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Source, strings.Join(s.Types, ","), unit, s.Shape, s.Mode, rate, avail)
	}
	return tw.Flush()
}

func (c *cli) get(ctx context.Context, args []string) error {
	pos, err := parse(c.flagSet("get"), args, 1, "source")
	if err != nil {
		return err
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	r, err := cl.Get(ctx, pos[0])
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(r)
	}
	_, err = fmt.Fprintln(c.stdout, formatReading(r, ""))
	return err
}

func (c *cli) tail(ctx context.Context, args []string) error {
	fs := c.flagSet("tail")
	var opts client.SubscribeOptions
	fs.StringVar(&opts.Type, "type", "", "")
	fs.Float64Var(&opts.RateHz, "rate", 0, "")
	fs.Float64Var(&opts.MinDelta, "min-delta", 0, "")
	count := fs.Uint64("count", 0, "")
	pos, err := parse(fs, args, 1, "source")
	if err != nil {
		return err
	}
	if opts.RateHz < 0 || opts.MinDelta < 0 {
		return usageErr{errors.New("--rate and --min-delta must not be negative")}
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	sub, err := cl.Subscribe(ctx, pos[0], opts)
	if err != nil {
		return err
	}
	rate := ""
	if sub.RateHz > 0 {
		rate = " at " + num(sub.RateHz) + " Hz"
	}
	fmt.Fprintf(c.stderr, "phonetopc: subscribed to %s (%s, %s)%s as %s; Ctrl-C to stop\n",
		sub.Source, strings.Join(sub.Types, ","), sub.Mode, rate, sub.ID)

	var (
		next  uint64
		seen  uint64
		total uint64 // dropped events, per seq gaps
	)
	for {
		select {
		case <-ctx.Done():
			return c.finishTail(sub, total)
		case ev, ok := <-sub.Events():
			if !ok {
				if err := sub.Err(); err != nil {
					return err
				}
				return nil
			}
			if ev.Seq > next {
				gap := ev.Seq - next
				total += gap
				fmt.Fprintf(c.stderr, "phonetopc: gap: %d event(s) dropped (seq %d..%d)\n", gap, next, ev.Seq-1)
			}
			next = ev.Seq + 1
			if c.json {
				if err := c.printJSON(ev); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(c.stdout, formatReading(ev.Reading, "#"+strconv.FormatUint(ev.Seq, 10))); err != nil {
				return err
			}
			seen++
			if *count > 0 && seen >= *count {
				return c.finishTail(sub, total)
			}
		}
	}
}

func (c *cli) finishTail(sub *client.Subscription, dropped uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sub.Unsubscribe(ctx) // best effort; closing the connection drops it anyway
	if dropped > 0 {
		fmt.Fprintf(c.stderr, "phonetopc: %d event(s) dropped in total\n", dropped)
	}
	return nil
}

// formatReading renders a reading as one human-readable line:
//
//	14:03:07.123  gyro/rotation  #42  [+0.0100 -0.3200 +0.0500] rad/s
func formatReading(r client.Reading, tag string) string {
	var b strings.Builder
	b.WriteString(time.UnixMilli(r.TS).Format("15:04:05.000"))
	b.WriteString("  ")
	b.WriteString(r.Source + "/" + r.Type)
	if tag != "" {
		b.WriteString("  " + tag)
	}
	if s, ok := formatEvent(r); ok {
		b.WriteString("  " + s)
	} else {
		b.WriteString("  " + formatValue(r.Value))
	}
	if r.Unit != "" {
		b.WriteString(" " + r.Unit)
	}
	return b.String()
}

func formatValue(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	switch v := v.(type) {
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			if f, ok := x.(float64); ok {
				parts[i] = fmt.Sprintf("%+.4f", f)
			} else {
				parts[i] = fmt.Sprint(x)
			}
		}
		return "[" + strings.Join(parts, " ") + "]"
	case float64:
		return num(v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%v", k, v[k])
		}
		return strings.Join(parts, " ")
	}
	return fmt.Sprint(v)
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
