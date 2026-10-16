package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/localapi"
	"github.com/melker12345/phonetopc/pcd/internal/pairing"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/store"
)

// With phonetopcd running as a system service, the pairings are its own:
// the commands here ask it over its API instead of reading the store.

// system returns the system phonetopcd to use instead of the user's store,
// or nil (localapi.Socket). Developer mode talks to the phone directly.
func (c *cli) system() (*localapi.Client, error) {
	if c.dev {
		return nil, nil
	}
	sock, err := localapi.Socket(c.getenv)
	if err != nil || sock == "" {
		return nil, err
	}
	return localapi.New(sock), nil
}

// needsRoot turns the daemon's 403 for a change to the pairings into what
// to do.
func needsRoot(err error, command string) error {
	if localapi.IsCode(err, "forbidden") {
		return fmt.Errorf("this changes which phone this PC trusts, so it needs root: run `sudo phonetopc %s`", command)
	}
	return err
}

func (c *cli) pairViaDaemon(ctx context.Context, api *localapi.Client, f pairFlags) error {
	conn, err := api.WebSocket(ctx, "/v1/pairing")
	if err != nil {
		return needsRoot(err, "pair")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	var wmu sync.Mutex
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, websocket.MessageText, b)
	}
	err = write(daemon.PairingStart{Yes: f.yes, TimeoutMS: f.timeout.Milliseconds(), Listen: f.listen, Hosts: f.hosts, NoMDNS: c.noMDNS})
	if err != nil {
		return err
	}

	prompt := newTermPrompter(c.stdin, c.stdout, f.yes, isTerminal(c.stdin))
	var (
		pmu     sync.Mutex
		pending = map[string]context.CancelFunc{}
	)
	defer func() {
		pmu.Lock()
		defer pmu.Unlock()
		for _, cancel := range pending {
			cancel()
		}
	}()
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("phonetopcd ended the pairing: %w", err)
		}
		var m daemon.PairingMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("phonetopcd sent %q: %w", b, err)
		}
		switch m.Type {
		case daemon.PairListening:
			if m.PC == nil {
				return errors.New("phonetopcd didn't say which PC pairs")
			}
			err := c.showPairing(pairShown{
				pcName: m.PC.Name, pcPin: m.PC.Pin, uri: m.URI, port: m.Port, randomPort: m.RandomPort,
				noLAN: len(m.Hosts) == 0, advertised: m.Advertised,
			}, f)
			if err != nil {
				return err
			}
		case daemon.PairQR:
			c.showNewQR(m.URI, f.png)
		case daemon.PairLog:
			fmt.Fprintf(c.stderr, "phonetopc: %s\n", m.Message)
		case daemon.PairConfirm:
			if m.Phone == nil {
				return errors.New("phonetopcd asked about no phone")
			}
			pctx, cancel := context.WithCancel(ctx)
			pmu.Lock()
			pending[m.Prompt] = cancel
			pmu.Unlock()
			go func(m daemon.PairingMessage) {
				defer cancel()
				phone := pairing.PhoneInfo{ID: m.Phone.ID, Name: m.Phone.Name, Pin: m.Phone.Pin}
				var old *store.Device
				if m.Replaces != nil {
					old = &store.Device{ID: m.Replaces.ID, Name: m.Replaces.Name, Pin: m.Replaces.Pin}
				}
				var ok bool
				var err error
				if m.Way == string(pairing.WaySAS) {
					ok, err = prompt.ConfirmSAS(pctx, phone, old, m.Code)
				} else {
					ok, err = prompt.ConfirmQR(pctx, phone, old)
				}
				if err == nil {
					write(daemon.PairingAnswer{Type: "answer", Prompt: m.Prompt, Yes: ok})
				}
			}(m)
		case daemon.PairAbandoned:
			pmu.Lock()
			if cancel := pending[m.Prompt]; cancel != nil {
				cancel()
			}
			pmu.Unlock()
		case daemon.PairPaired:
			if m.Phone == nil {
				return errors.New("phonetopcd paired no phone")
			}
			conn.Close(websocket.StatusNormalClosure, "")
			return c.pairDone(nil, m.Way, m.Phone.Name, m.Phone.ID, f.timeout)
		case daemon.PairError:
			switch m.Error {
			case "expired":
				err = pairing.ErrExpired
			case "sas_limit":
				err = pairing.ErrSASLimit
			default:
				err = errors.New(m.Message)
			}
			return c.pairDone(err, "", "", "", f.timeout)
		}
	}
}

func devicesViaDaemon(ctx context.Context, api *localapi.Client) ([]listedDevice, error) {
	var res daemon.DevicesResponse
	if err := api.Get(ctx, "/v1/devices", &res); err != nil {
		return nil, err
	}
	out := make([]listedDevice, len(res.Devices))
	for i, d := range res.Devices {
		out[i] = listedDevice{d.ID, d.Name, d.Pin, d.Port, d.Addresses, d.PairedAt}
	}
	return out, nil
}

func unpairViaDaemon(ctx context.Context, api *localapi.Client, sel string) (id, name string, err error) {
	var res daemon.DeviceRef
	if err := api.Do(ctx, "DELETE", "/v1/devices/"+url.PathEscape(sel), nil, &res); err != nil {
		return "", "", needsRoot(err, "unpair "+sel)
	}
	return res.ID, res.Name, nil
}

func pcViaDaemon(ctx context.Context, api *localapi.Client) (id, name, pin string, err error) {
	var res daemon.PCInfo
	err = api.Get(ctx, "/v1/pc", &res)
	return res.ID, res.Name, res.Pin, err
}

// connected returns the daemon's status, or why it has no phone.
func connected(ctx context.Context, api *localapi.Client) (daemon.Status, error) {
	var st daemon.Status
	if err := api.Get(ctx, "/v1/status", &st); err != nil {
		return st, err
	}
	if !st.Connected || st.Device == nil {
		return st, fmt.Errorf("phonetopcd has no phone: %s", st.Error)
	}
	return st, nil
}

func (c *cli) approvalKeyViaDaemon(ctx context.Context, api *localapi.Client, accept, unattested bool) error {
	var (
		info daemon.ApprovalKeyInfo
		err  error
	)
	if accept {
		err = api.Do(ctx, "POST", "/v1/approval-key", daemon.ApprovalKeyRequest{Unattested: unattested}, &info)
	} else {
		err = api.Get(ctx, "/v1/approval-key", &info)
	}
	switch {
	case localapi.IsCode(err, "approval_key_unattested"), localapi.IsCode(err, "approval_invalid"):
		return exitError{2, err}
	case err != nil:
		return needsRoot(err, "approval-key --accept")
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintf(tw, "device\t%s (id %s)\n", info.Device.Name, info.Device.ID)
	accepted := info.Accepted
	if accepted == "" {
		accepted = "none yet"
	}
	fmt.Fprintf(tw, "accepted\t%s\n", accepted)
	if info.Status == daemon.KeyNone {
		fmt.Fprintf(tw, "current\tnone: the phone offers no approvals (no strong biometric enrolled?)\n")
		return nil
	}
	fmt.Fprintf(tw, "current\t%s\n", info.Current)
	switch info.Status {
	case daemon.KeyRefused:
		fmt.Fprintf(tw, "status\tREFUSED: %s\n", info.Message)
		return exitError{1, nil}
	case daemon.KeyChanged:
		fmt.Fprintf(tw, "status\tCHANGED: approvals are refused until you accept the new key\n")
		tw.Flush()
		fmt.Fprintln(c.stderr, "phonetopc: the approval key changed. That is expected after fingerprints were added or removed on the phone;\n"+
			"  if you did that, accept it with `sudo phonetopc approval-key --accept`. Otherwise someone may be tampering with the phone app.")
		return exitError{1, nil}
	case daemon.KeyUnattested:
		fmt.Fprintf(tw, "status\tNOT ATTESTED: approvals are refused\n")
		tw.Flush()
		fmt.Fprintf(c.stderr, "phonetopc: %s.\n  %s\n", info.Message, strings.ReplaceAll(unattestedHint, "`phonetopc", "`sudo phonetopc"))
		return exitError{1, nil}
	case daemon.KeyAccepted:
		fmt.Fprintf(tw, "attestation\t%s\n", describeAttestation(info.Attestation))
		fmt.Fprintf(tw, "status\taccepted now\n")
	case daemon.KeyFirstSeen:
		fmt.Fprintf(tw, "attestation\t%s\n", describeAttestation(info.Attestation))
		fmt.Fprintf(tw, "status\ttrusted (first seen now, over the pinned connection)\n")
	default:
		fmt.Fprintf(tw, "attestation\t%s\n", describeAttestation(info.Attestation))
		fmt.Fprintf(tw, "status\ttrusted\n")
	}
	return nil
}

// approveViaDaemon is approve, with the daemon asking and verifying.
func (c *cli) approveViaDaemon(ctx context.Context, api *localapi.Client, reason string, timeout time.Duration) error {
	fail := func(err error) error {
		if ctx.Err() != nil {
			err = errors.New("interrupted; not approved")
		}
		return exitError{2, err}
	}
	st, err := connected(ctx, api)
	if err != nil {
		return fail(err)
	}
	name := st.Device.Name
	match := 0
	show := func(m int, _ string) {
		match = m
		fmt.Fprintf(c.stderr, "phonetopc: pick %d on %q, then confirm with your fingerprint (up to %v)…\n", m, name, timeout)
	}
	var res daemon.ApprovalResponse
	err = api.Ask(ctx, "/v1/approvals", daemon.ApprovalRequest{Reason: reason, TimeoutMS: timeout.Milliseconds()}, show, &res)
	if err != nil {
		return fail(err)
	}
	if c.json {
		out := map[string]any{"approved": res.Approved, "device": st.Device.ID, "match": match}
		if !res.Approved {
			out["why"] = res.Why
		}
		if err := c.printJSON(out); err != nil {
			return exitError{2, err}
		}
	} else if res.Approved {
		fmt.Fprintf(c.stderr, "phonetopc: approved on %q\n", name)
	} else {
		fmt.Fprintf(c.stderr, "phonetopc: not approved: %s (%s)\n", res.Why, protocol.Why(res.Why).Describe())
	}
	if !res.Approved {
		return exitError{1, nil}
	}
	return nil
}

func (c *cli) infoViaDaemon(ctx context.Context, api *localapi.Client) error {
	st, err := connected(ctx, api)
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(st)
	}
	dev := st.Device
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("device", fmt.Sprintf("%s (id %s)", dev.Name, dev.ID))
	row("manufacturer", dev.Manufacturer)
	row("model", dev.Model)
	row("platform", strings.TrimSpace(dev.Platform+" "+dev.PlatformVersion))
	row("capabilities", strings.Join(st.Capabilities, ", "))
	row("via", "phonetopcd at "+api.Socket())
	return tw.Flush()
}

func (c *cli) listViaDaemon(ctx context.Context, api *localapi.Client) error {
	var res protocol.ListSensorsResult
	if err := api.Get(ctx, "/v1/sensors", &res); err != nil {
		return err
	}
	return c.printSensors(res.Sensors)
}

func (c *cli) getViaDaemon(ctx context.Context, api *localapi.Client, source string) error {
	var r client.Reading
	if err := api.Get(ctx, "/v1/sensors/"+url.PathEscape(source), &r); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(r)
	}
	_, err := fmt.Fprintln(c.stdout, formatReading(r, ""))
	return err
}

// events opens /v1/events, then calls f with each event until the stream
// ends, or f returns false or an error.
func events(ctx context.Context, api *localapi.Client, q url.Values, f func(ev client.Event) (bool, error)) error {
	body, err := api.Stream(ctx, "/v1/events?"+q.Encode())
	if err != nil {
		return err
	}
	defer body.Close()
	return readEvents(ctx, body, f)
}

func readEvents(ctx context.Context, body io.Reader, f func(ev client.Event) (bool, error)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev client.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("phonetopcd sent %q: %w", sc.Text(), err)
		}
		if ev.Source == daemon.SourceDaemon {
			var v struct{ Error string }
			json.Unmarshal(ev.Value, &v)
			if ev.Type == daemon.TypeError {
				return errors.New(v.Error)
			}
			if ev.Type == daemon.TypeDisconnected {
				continue
			}
		}
		more, err := f(ev)
		if !more || err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

func (c *cli) tailViaDaemon(ctx context.Context, api *localapi.Client, source string, opts client.SubscribeOptions, count uint64) error {
	q := url.Values{"source": {source}}
	if opts.Type != "" {
		q.Set("type", opts.Type)
	}
	if opts.RateHz > 0 {
		q.Set("rate", num(opts.RateHz))
	}
	if opts.MinDelta > 0 {
		q.Set("min_delta", num(opts.MinDelta))
	}
	fmt.Fprintf(c.stderr, "phonetopc: streaming %s through phonetopcd; Ctrl-C to stop\n", source)
	var next, seen, total uint64
	err := events(ctx, api, q, func(ev client.Event) (bool, error) {
		if ev.Source == daemon.SourceDaemon {
			if ev.Type == daemon.TypeConnected {
				next = 0 // the phone's subscriptions start again
			}
			return true, nil
		}
		if ev.Seq > next {
			gap := ev.Seq - next
			total += gap
			fmt.Fprintf(c.stderr, "phonetopc: gap: %d event(s) dropped (seq %d..%d)\n", gap, next, ev.Seq-1)
		}
		next = ev.Seq + 1
		if c.json {
			if err := c.printJSON(ev); err != nil {
				return false, err
			}
		} else if _, err := fmt.Fprintln(c.stdout, formatReading(ev.Reading, "#"+strconv.FormatUint(ev.Seq, 10))); err != nil {
			return false, err
		}
		seen++
		return count == 0 || seen < count, nil
	})
	if total > 0 {
		fmt.Fprintf(c.stderr, "phonetopc: %d event(s) dropped in total\n", total)
	}
	return err
}

func (c *cli) notifyViaDaemon(ctx context.Context, api *localapi.Client, p protocol.NotifyParams, wait bool) error {
	if !wait {
		var res protocol.NotifyResult
		if err := api.Do(ctx, "POST", "/v1/notifications", p, &res); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(map[string]string{"id": res.ID})
		}
		_, err := fmt.Fprintln(c.stdout, res.ID)
		return err
	}
	// Listen first, so an answer can't come before we do.
	body, err := api.Stream(ctx, "/v1/events?source="+client.SourceNotification)
	if err != nil {
		return err
	}
	defer body.Close()
	var res protocol.NotifyResult
	if err := api.Do(ctx, "POST", "/v1/notifications", p, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "phonetopc: posted notification %q; waiting for the user (Ctrl-C to stop)…\n", res.ID)
	return readEvents(ctx, body, func(ev client.Event) (bool, error) {
		n, err := ev.Notification()
		if ev.Source != client.SourceNotification || err != nil || n.Notification != res.ID {
			return true, nil
		}
		if c.json {
			return false, c.printJSON(map[string]string{"id": res.ID, "type": ev.Type, "action": n.Action})
		}
		answer := ev.Type
		if ev.Type == client.TypeAction {
			answer = n.Action
		}
		_, err = fmt.Fprintln(c.stdout, answer)
		return false, err
	})
}

func priority(high bool) string {
	if high {
		return protocol.PriorityHigh
	}
	return ""
}
