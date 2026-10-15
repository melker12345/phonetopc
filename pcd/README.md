# pcd: phonetopc on the PC (Go)

The Go side of phonetopc. It implements protocol `phonetopc/1` (draft 4, see
[`../spec/PROTOCOL.md`](../spec/PROTOCOL.md)), pairing, TLS and tokens (milestone M2, see
[`../spec/SECURITY.md`](../spec/SECURITY.md)), fingerprint approval (see
[`../spec/APPROVAL.md`](../spec/APPROVAL.md)), and notifications and panels (see
[`../spec/PANELS.md`](../spec/PANELS.md)).

| Path | What |
|---|---|
| `cmd/phonetopcd` | The daemon: one connection to the paired phone, offered to local programs as plain HTTP ([`../spec/LOCAL-API.md`](../spec/LOCAL-API.md)) |
| `cmd/phonetopc` | CLI: `pair`, `devices`, `unpair`, `pc`, and `info`, `list`, `get`, `tail`, `approve`, `approval-key`, `notify` against a phone or the mock |
| `cmd/phonetopc-mock` | Mock phone with synthetic sensor data, dev or secure mode, approvals, panels with a simulated user, and a pairing client |
| `client` | Importable Go client library, including secure mode, `DialPaired`, `RequestApproval`, `Notify` and `ShowPanel` |
| `internal/protocol` | Wire types, error codes and the sensor catalog, shared by everything here |
| `internal/security` | Pins, base64url, the SAS commitment and code, constant-time compares, keys, pinned TLS, approval signatures |
| `internal/store` | The PC's credential store: `pc.json`, `pc-key.pem`, `devices.json` (SECURITY.md §6) |
| `internal/pairing` | Pairing (SECURITY.md §4): the PC's listener and the phone's client side |
| `internal/discovery` | mDNS for `_phonetopc._tcp` and `_phonetopc-pair._tcp`, and the PC's LAN addresses |
| `internal/mockphone` | The mock server implementation and its state directory, also used in-process by tests |
| `internal/daemon` | The daemon's core: reconnecting, sharing subscriptions between local streams, reattaching panels, and the HTTP handlers |
| `internal/hints` | Error messages that say what to do, shared by the CLI and the daemon |

Programs should use `phonetopcd` (below); the client library is for Go programs that want to talk to the phone
themselves, and is what the CLI and the daemon are built on.

## Build

You need Go 1.27 or newer. The binaries are static, with no cgo.

```sh
cd pcd
CGO_ENABLED=0 go build -o bin/ ./cmd/...     # bin/phonetopc-mock, bin/phonetopc
```

## Pair with a phone

On the PC:

```sh
bin/phonetopc pair            # prints a QR code, and advertises itself for "Find on network"
```

On the phone, open phonetopc → *Pair a PC*, then either scan the QR code or tap the PC under
*Find on network* and check that both screens show the same 6-digit code. The PC asks for
confirmation too, and takes only an explicit `y`; what you type before the question appears is
ignored. If you answer no, or the phone goes away while you're asked, that QR code stops working
(someone may have photographed it) and a new one is printed. `--yes` skips that for the QR code, for scripts; it turns *Find on network* off
(nobody would compare the codes) and refuses a phone whose id is already paired with a different
key. Without `--yes`, replacing such a phone takes an explicit `y`. `--png FILE` also writes the QR code as an
image. The listener closes after one pairing, on Ctrl-C, after 5 minutes (`--timeout`), or after
3 code comparisons that failed or were abandoned (SECURITY.md §4.3): each one is a fresh guess for
someone in the middle, so it stops and says so.

The pairing listener uses TCP port **8766** (SECURITY.md §4.1), so a firewall rule only has to be
added once; if 8766 is busy it says so and uses a random port. `--listen ADDR` picks another
address or port. When ufw (`ENABLED=yes` in `/etc/ufw/ufw.conf`) or firewalld is active,
`phonetopc pair` prints the rule to add, for example:

```sh
sudo ufw allow from 192.168.1.0/24 to any port 8766 proto tcp
```

Afterwards every command connects securely to the paired phone: `wss://` with the phone's key
pinned, and the token the phone issued in `hello`. The phone is looked up with mDNS and then at
its last known addresses.

```sh
bin/phonetopc devices                     # paired phones
bin/phonetopc info                        # the only paired phone
bin/phonetopc --device "Pixel 8" tail gyro   # one of several ($PHONETOPC_DEVICE works too)
bin/phonetopc unpair "Pixel 8"            # forget it (also revoke the PC on the phone)
bin/phonetopc pc                          # this PC's id, name and pin
```

Errors say what to do: a phone presenting a different key ("phone identity changed — re-pair")
is never connected to, and a `-32010` answer means the phone revoked this PC.

## Run the daemon

`phonetopcd` connects to the paired phone, keeps the connection up (reconnecting within 10 s when the phone comes
back), and serves the local API ([`../spec/LOCAL-API.md`](../spec/LOCAL-API.md)) to programs on this PC:

```sh
bin/phonetopcd                       # the only paired phone; --device D picks one of several
bin/phonetopcd --insecure-dev        # a phone in developer mode, or the mock without --state

S=$XDG_RUNTIME_DIR/phonetopc.sock
curl -s --unix-socket $S http://phonetopc/v1/status
curl -sN --unix-socket $S 'http://phonetopc/v1/events?source=gyro&rate=20'
curl -s --unix-socket $S http://phonetopc/v1/approvals -d '{"reason":"Deploy?"}'
curl -s -H "Authorization: Bearer $(cat $XDG_RUNTIME_DIR/phonetopc.token)" http://127.0.0.1:8767/v1/status
```

It serves on a Unix socket only this user can open, `$XDG_RUNTIME_DIR/phonetopc.sock`, and on `127.0.0.1:8767`,
where requests need the token in `$XDG_RUNTIME_DIR/phonetopc.token`. `--device`, `--addr`, `--insecure-dev` and
`--no-mdns` work as for the CLI; `--socket PATH`, `--listen ADDR` and `--token-file PATH` move the listeners
(`off` turns one off; `--listen` takes only loopback addresses). A second daemon on the same socket refuses to
start.

To start it with your session, use the systemd user unit:

```sh
go install ./cmd/phonetopcd          # or copy bin/phonetopcd to ~/go/bin
cp systemd/phonetopcd.service ~/.config/systemd/user/
systemctl --user enable --now phonetopcd
journalctl --user -u phonetopcd -f   # it logs connects, disconnects and new errors
```

Without systemd, start `phonetopcd` from your session's autostart (for example `exec-once = phonetopcd` in
Hyprland).

## Fingerprint approval

`phonetopc approve` asks the phone's user to confirm with a fingerprint. The phone signs the
request with a key that only works right after a fingerprint check, and the PC verifies the
signature itself (APPROVAL.md), so the exit code can gate anything:

```sh
phonetopc approve "Deploy to production?" && ./deploy.sh
phonetopc approve "Unlock the vault?" --timeout 1m
```

| Exit code | Meaning |
|---|---|
| 0 | Approved, and the signature verified |
| 1 | Not approved: `denied`, `cancelled`, `timeout`, `busy` (another approval is pending), `lockout` or `unavailable`; the reason is printed |
| 2 | Error: an approval that failed verification (never treated as approval), a changed approval key, an invalid reason or timeout, connection problems, Ctrl-C |

The reason is shown on the phone and signed exactly as given: 1–200 characters, no leading or
trailing whitespace, no control or bidirectional-control characters. `--json` prints
`{"approved": …, "why": …, "device": …}`.

The phone's approval key is pinned like its TLS key: the first time a session sees it, its pin
is stored as `approval_key` in `devices.json`. If the phone later presents a different key
(typically because fingerprints were added or removed, which makes Android replace it),
approvals are refused, without asking the phone, until you accept the new key:

```sh
phonetopc approval-key            # accepted and current key; exit 1 if it changed
phonetopc approval-key --accept   # accept the current key
```

`phonetopc info` shows whether the phone offers approvals and whether its key is trusted.
Re-pairing clears the stored key.

## Notifications and panels

A PC program can show things on the phone (PANELS.md): notifications with up to three buttons,
and full-screen panels built from widgets (buttons, toggles, sliders, text, progress bars, a
text field, a drawing canvas). What the user does comes back as events of the sources
`notification` and `panel`. The phone's user allows both per PC; they are off by default, and
until then the calls fail with `-32030`.

```sh
phonetopc notify "Claude Code is done" --body "3 files changed"
phonetopc notify "Deploy?" --action yes:Deploy --action no:Later --wait   # prints yes, no, tap or dismiss
phonetopc tail panel                     # input on the panels this connection owns
phonetopc tail notification              # answers to this PC's notifications
```

Panels need a program that stays connected, since a panel's events go to the connection that
showed it; see the client library below. When that connection drops, the panel stays on the
phone, detached, for 10 minutes, and its canvases keep taking ink; `AttachPanel` takes it over
again and replays the strokes drawn meanwhile.

### Where credentials live

`$XDG_CONFIG_HOME/phonetopc/` (default `~/.config/phonetopc/`), directory mode 0700:

| File | Content |
|---|---|
| `pc.json` | `{"id", "name"}` of this PC; the name defaults to the host name and may be edited |
| `pc-key.pem` | This PC's ECDSA P-256 key (SEC 1 PEM), 0600. Its certificate is regenerated at each start; only the key's pin matters |
| `devices.json` | Paired phones, exactly as in SECURITY.md §6 (including `approval_key`), 0600. Readable by any language; members this version doesn't know are kept when it rewrites the file |
| `.lock` | Empty lock file serialising updates of `devices.json` between processes |

Files are replaced atomically (temp file, fsync, rename). The token in `devices.json` is a
credential, like an SSH key. When a phone is found at a stored address, that address moves to
the front of `addresses`, unless it is a loopback address (such as from `adb forward`); `port`
is only updated from mDNS.

## Run the mock

Dev mode (plaintext, no authentication), as before:

```sh
bin/phonetopc-mock                          # ws://127.0.0.1:8765/v1/ws
bin/phonetopc-mock --addr 0.0.0.0:8765 --tap-every 3s --unavailable light -v
bin/phonetopc --insecure-dev info           # --addr host:port for another address
```

Secure mode, which is the default once the mock has a state directory:

```sh
bin/phonetopc-mock --state /tmp/mock        # wss://127.0.0.1:8765/v1/ws with the mock's own key
```

| Flag | Meaning |
|---|---|
| `--addr` | Listen address. Default `127.0.0.1:8765` |
| `--state DIR` | Keeps the mock's id, key (`phone-key.pem`) and paired PCs (`pcs.json`); selects secure mode |
| `--secure` / `--dev` | Force the mode. `--secure` needs `--state` |
| `--mdns` | Advertise `_phonetopc._tcp` (TXT `id`, `proto=1`, `tls=1` or `dev=1`) |
| `--name` | Device name. Default `phonetopc mock` |
| `--tap-every` | Interval between synthetic `ui/tile_tap` events. Default `10s`; `0` turns them off |
| `--nfc-every` | Mean interval between synthetic `nfc/tag` scans. Default `20s`; `0` turns them off |
| `--user-every` | Interval of the simulated user, who opens the newest panel, uses one of its widgets per tick (press, flip, slide, submit text, draw a circle) and answers notifications with their first action. Default `4s`; `0` turns it off, and then no panel ever becomes visible |
| `--no-notify` / `--no-panels` | Act as a phone whose user hasn't allowed notifications or panels (`-32030`). The mock allows both by default; a phone doesn't |
| `--unavailable` | Comma-separated list of sources to report as `available: false` |
| `--device-id` | Device id. Default `mock-0001`; in secure mode it only applies when the state is created (otherwise a random id), and must be 16 to 64 lowercase hex digits (SECURITY.md §2) |
| `--seed` | Simulation seed |
| `--approval MODE` | Secure mode: how the mock's "user" answers approval requests: `auto` (approve, the default), `deny`, `cancel`, `lockout`, `timeout` (never answer), `ask` (prompt on the mock's terminal: y/n/c/l), `tamper` (approve with a corrupted signature, to test clients), `off` (no approval capability) |
| `--approval-delay` | How long `auto`, `deny`, `cancel`, `lockout` and `tamper` take to answer. Default `1s` |
| `-v` | Log every frame |

In secure mode the mock checks the token in `hello` (`-32010` then close 1008 on failure, and
close 1008 after any failed `hello` that follows a successful one), closes connections without a
successful `hello` after 10 s, limits failed `hello`s to 5 a minute per address (then closes
with 1013), and reports the recognised PC in `hello.pc`. It rereads `pcs.json` once a second, so
pairings and revocations made by other `phonetopc-mock` commands apply to running sessions.

It also offers fingerprint approval: a software P-256 key in `approval-key.pem` in its state
directory stands in for the phone's biometric-bound key, `hello` lists capability `approval` and
`hello.approval`, and `request_approval` is answered as `--approval` says. One approval is
pending at a time (others get `busy`), and closing the session cancels a pending prompt. In dev
mode the method doesn't exist (`-32601`).

The mock also plays the phone's side of pairing:

```sh
bin/phonetopc-mock pair --state /tmp/mock 'phonetopc://pair?v=1&...'   # the QR way: paste the URI
bin/phonetopc-mock pair --state /tmp/mock --discover [--confirm]        # "Find on network" + code
bin/phonetopc-mock pair --state /tmp/mock --pc-addr 127.0.0.1:40123     # the same without mDNS
bin/phonetopc-mock pcs --state /tmp/mock                                # paired PCs
bin/phonetopc-mock revoke --state /tmp/mock arch-desktop                # revoke one
bin/phonetopc-mock approval-key --state /tmp/mock [--rotate]            # show, or replace as after re-enrollment
```

`pair --addr` (default `127.0.0.1:8765`) is the address the mock will serve on, which it tells the
PC; use the same `--addr` when serving.

### Try pairing end to end on one machine

```sh
# terminal 1: the PC
bin/phonetopc pair
# terminal 2: the phone, either way
bin/phonetopc-mock pair --state /tmp/mock 'phonetopc://pair?v=1&id=...'   # copy the URI printed in terminal 1
bin/phonetopc-mock pair --state /tmp/mock --discover                       # compare codes, answer y in both terminals
# terminal 2: serve, then use it from the PC
bin/phonetopc-mock --state /tmp/mock
bin/phonetopc info
bin/phonetopc tail gyro
bin/phonetopc approve "Deploy?" && echo deploying           # the mock approves after a second
bin/phonetopc-mock approval-key --state /tmp/mock --rotate  # then: approve exits 2 until `phonetopc approval-key --accept`
bin/phonetopc-mock revoke --state /tmp/mock "$(hostname)"   # running tail ends; phonetopc info says the PC was revoked
```

What the mock simulates:

- The motion sensors model one phone held in a hand. It rolls and pitches gently and turns slowly.
  - The gyro reports the matching angular rate.
  - The accelerometer includes gravity (about 9.81 on z when face up) plus some hand shake.
  - `game_rotation_vector` is the matching unit quaternion.
  - `rotation_vector` is the same orientation relative to magnetic north, `compass` its heading
    (it turns slowly through all 360°), and `magnetometer` the earth's field (~50 µT, pointing
    north and down) seen by the phone.
- The barometer drifts slowly around 1013 hPa, with a little noise.
- Light changes every few seconds.
- Proximity toggles about every 15 s.
- The battery drains by 1% a minute, and starts charging when it reaches 15%.
- The location is a fix in Stockholm, updated every second with a few meters of jitter.
- An NFC tag is scanned about every 20 s, alternating between a sticker with a URL and a text
  record, and a card without NDEF.

- Notifications and panels are shown in the log. The simulated user answers a notification a
  tick after it was posted, opens a new panel on the next tick, and then works through its
  widgets in layout order, one per tick; a circle on a canvas arrives in chunks of two points
  every 32 ms. In tests, `Server.User` drives the same actions by hand.

The mock implements all of draft 4, including the error codes, ping/pong keepalive,
backpressure, approvals, notifications and panels (with detached panels, expiry and stroke
replay). It is the reference for the transcripts in [`../spec/conformance`](../spec/conformance).

## Use the CLI

```sh
bin/phonetopc info
bin/phonetopc list
bin/phonetopc get battery
bin/phonetopc tail gyro --rate 100
bin/phonetopc tail accelerometer --min-delta 0.5 --json | jq -c .value
bin/phonetopc tail ui --count 1             # wait for one tile tap
bin/phonetopc tail nfc --count 1            # wait for one NFC tag
```

`tail` prints one line per event and keeps going until you press Ctrl-C. It reports `seq` gaps
(dropped events) on stderr.

| Flag / variable | Meaning |
|---|---|
| `--device D` / `PHONETOPC_DEVICE` | Paired phone by name or id; needed when several are paired |
| `--addr H:P` / `PHONETOPC_ADDR` | Connect there instead of looking for the phone (still pinned and authenticated) |
| `--insecure-dev` / `PHONETOPC_INSECURE_DEV=1` | Plaintext dev mode, no pairing; default address `127.0.0.1:8765` |
| `--no-mdns` / `PHONETOPC_NO_MDNS=1` | Skip the mDNS lookup |

## Use the client library

```go
c, err := client.DialPaired(ctx, "")      // the only paired phone; or its name or id
if err != nil { ... }                     // errors.Is(err, client.ErrPinMismatch), client.IsUnauthorized(err)
defer c.Close()
sub, err := c.Subscribe(ctx, "gyro", client.SubscribeOptions{RateHz: 100})
for ev := range sub.Events() {
	v, _ := ev.Vec3()
	fmt.Println(ev.Seq, v)
}
```

Values decode by shape: `Number()`, `Bool()`, `Vec3()`, `Quat()`, and for the object sources
`Battery()`, `Position()` (`location`) and `Tag()` (`nfc`).

With a known address and credentials: `client.Dial(ctx, client.SecureURL("192.168.1.153:8765"),
client.WithPin(pin), client.WithToken(token))`. Dev mode: `client.Dial(ctx, client.URL(addr))`.
A `wss://` URL without a pin, or a pin with a `ws://` URL, is refused.

Approval:

```go
res, err := c.RequestApproval(ctx, "Deploy to production?", 30*time.Second)
switch {
case err != nil:     // errors.Is(err, client.ErrApprovalKeyChanged), client.ErrApprovalInvalid, ...
case res.Approved(): // true only after the signature verified
default:             // res.Why: client.WhyDenied, WhyCancelled, WhyTimeout, WhyBusy, WhyLockout, WhyUnavailable
}
```

`DialPaired` trusts approval keys through `devices.json` (stored on first sight, a change refused
until accepted); a client from `Dial` needs `client.WithApprovalKey(pin)` or
`client.WithApprovalTrust(func(pin string) error)`.

A stream deck with a status line (subscribe before showing, so no event is missed):

```go
sub, _ := c.Subscribe(ctx, client.SourcePanel, client.SubscribeOptions{})
mute := client.Button("mute", "🔇 Mute")
mute.Tone = "bad"
_, err := c.ShowPanel(ctx, "deck", "Stream deck", client.Column(
	client.Text("status", "Claude: working…"),
	client.Grid(2, mute, client.Toggle("mic", "Mic", true)),
	client.Slider("vol", "Volume", 0, 100, 40),
), client.PanelOptions{KeepAwake: true})
if client.IsNotAllowed(err) { ... }        // the user hasn't allowed panels for this PC yet
for ev := range sub.Events() {
	p, _ := ev.Panel()
	switch {
	case ev.Type == client.TypePress && p.Widget == "mute":
		c.UpdatePanel(ctx, "deck", client.Set("status", "text", "muted"))
	case ev.Type == client.TypeChange && p.Widget == "vol":
		v, _ := p.NumberValue()             // p.Final is set on release
		_ = v
	case ev.Type == client.TypeStroke:      // p.Phase, p.Points ([x, y, pressure, ms]), p.Replayed
	}
}
```

`ShowPanel` and `Notify` check the layout and texts before sending. `c.Notify(ctx, title,
client.NotifyOptions{...})` returns the notification's id; its answers arrive on a
`client.SourceNotification` subscription.

## Test

```sh
go vet ./... && go test -race ./...
go test ./internal/mockphone -run TestTranscripts -record   # regenerate spec/conformance
```

The tests start the mock in-process and drive it through the client and over raw WebSockets. They
cover every error code. They also validate every frame in both directions against
[`../spec/schemas/phonetopc-1.schema.json`](../spec/schemas/phonetopc-1.schema.json), check
that the committed conformance transcripts still match the mock, and run
[`server-cases.json`](../spec/conformance/server-cases.json) (dev mode),
[`secure-server-cases.json`](../spec/conformance/secure-server-cases.json) (secure mode),
[`approval-server-cases.json`](../spec/conformance/approval-server-cases.json) and
[`panel-server-cases.json`](../spec/conformance/panel-server-cases.json) (a fresh mock per case)
against it.
`internal/security` checks [`vectors.json`](../spec/conformance/vectors/vectors.json), and
`internal/security` and `client` check [`approval.json`](../spec/conformance/vectors/approval.json),
the valid vector and every tampered one.

End-to-end tests in `cmd/phonetopc` pair the CLI with the mock's pairing client (QR and code
comparison), then stream securely, and cover pin mismatch, revocation, re-pairing, expiry and
device selection. Approval end-to-end tests cover approved, denied, cancelled, lockout, timeout,
busy, a changed key refused until accepted, and a tampered signature. The tests that use real mDNS skip themselves where multicast doesn't work
(`go test -short` skips them outright); the same flows are also tested with addresses given
directly.

## Dependencies

| Module | Why |
|---|---|
| `github.com/coder/websocket` | WebSocket client and server |
| `github.com/libp2p/zeroconf/v2` | mDNS/DNS-SD in pure Go (maintained fork of grandcat/zeroconf); brings `miekg/dns` and `golang.org/x/net` |
| `github.com/skip2/go-qrcode` | QR code encoder for the terminal and `--png`; no dependencies |
| `github.com/santhosh-tekuri/jsonschema/v6` | Tests only: schema validation |
