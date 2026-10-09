# pcd: phonetopc on the PC (Go)

The Go side of phonetopc. It implements protocol `phonetopc/1` (draft 2, see
[`../spec/PROTOCOL.md`](../spec/PROTOCOL.md)) and pairing, TLS and tokens (milestone M2, see
[`../spec/SECURITY.md`](../spec/SECURITY.md)).

| Path | What |
|---|---|
| `cmd/phonetopc` | CLI: `pair`, `devices`, `unpair`, `pc`, and `info`, `list`, `get`, `tail` against a phone or the mock |
| `cmd/phonetopc-mock` | Mock phone with synthetic sensor data, dev or secure mode, with a pairing client |
| `client` | Importable Go client library, including secure mode and `DialPaired` |
| `internal/protocol` | Wire types, error codes and the sensor catalog, shared by everything here |
| `internal/security` | Pins, base64url, the SAS commitment and code, constant-time compares, keys, pinned TLS |
| `internal/store` | The PC's credential store: `pc.json`, `pc-key.pem`, `devices.json` (SECURITY.md §6) |
| `internal/pairing` | Pairing (SECURITY.md §4): the PC's listener and the phone's client side |
| `internal/discovery` | mDNS for `_phonetopc._tcp` and `_phonetopc-pair._tcp`, and the PC's LAN addresses |
| `internal/mockphone` | The mock server implementation and its state directory, also used in-process by tests |

The `phonetopcd` daemon (milestone M3) will go in `cmd/phonetopcd`, with its internals under `internal/`.

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
confirmation too (`--yes` skips that, for scripts). `--png FILE` also writes the QR code as an
image. The listener closes after one pairing, on Ctrl-C, or after 5 minutes (`--timeout`).

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

### Where credentials live

`$XDG_CONFIG_HOME/phonetopc/` (default `~/.config/phonetopc/`), directory mode 0700:

| File | Content |
|---|---|
| `pc.json` | `{"id", "name"}` of this PC; the name defaults to the host name and may be edited |
| `pc-key.pem` | This PC's ECDSA P-256 key (SEC 1 PEM), 0600. Its certificate is regenerated at each start; only the key's pin matters |
| `devices.json` | Paired phones, exactly as in SECURITY.md §6, 0600. Readable by any language |
| `.lock` | Empty lock file serialising updates of `devices.json` between processes |

Files are replaced atomically (temp file, fsync, rename). The token in `devices.json` is a
credential, like an SSH key.

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
| `--unavailable` | Comma-separated list of sources to report as `available: false` |
| `--device-id` | Device id. Default `mock-0001`; in secure mode it only applies when the state is created (otherwise a random id) |
| `--seed` | Simulation seed |
| `-v` | Log every frame |

In secure mode the mock checks the token in `hello` (`-32010` then close 1008 on failure),
closes connections without a successful `hello` after 10 s, limits failed `hello`s to 5 a minute
per address, and reports the recognised PC in `hello.pc`. It rereads `pcs.json` once a second, so
pairings and revocations made by other `phonetopc-mock` commands apply to running sessions.

The mock also plays the phone's side of pairing:

```sh
bin/phonetopc-mock pair --state /tmp/mock 'phonetopc://pair?v=1&...'   # the QR way: paste the URI
bin/phonetopc-mock pair --state /tmp/mock --discover [--confirm]        # "Find on network" + code
bin/phonetopc-mock pair --state /tmp/mock --pc-addr 127.0.0.1:40123     # the same without mDNS
bin/phonetopc-mock pcs --state /tmp/mock                                # paired PCs
bin/phonetopc-mock revoke --state /tmp/mock arch-desktop                # revoke one
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
bin/phonetopc-mock revoke --state /tmp/mock "$(hostname)"   # running tail ends; phonetopc info says the PC was revoked
```

What the mock simulates:

- The motion sensors model one phone held in a hand. It rolls and pitches gently and turns slowly.
  - The gyro reports the matching angular rate.
  - The accelerometer includes gravity (about 9.81 on z when face up) plus some hand shake.
  - `game_rotation_vector` is the matching unit quaternion.
- Light changes every few seconds.
- Proximity toggles about every 15 s.
- The battery drains by 1% a minute, and starts charging when it reaches 15%.

The mock implements all of draft 2, including the error codes, ping/pong keepalive and
backpressure. It is the reference for the transcripts in [`../spec/conformance`](../spec/conformance).

## Use the CLI

```sh
bin/phonetopc info
bin/phonetopc list
bin/phonetopc get battery
bin/phonetopc tail gyro --rate 100
bin/phonetopc tail accelerometer --min-delta 0.5 --json | jq -c .value
bin/phonetopc tail ui --count 1             # wait for one tile tap
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

With a known address and credentials: `client.Dial(ctx, client.SecureURL("192.168.1.153:8765"),
client.WithPin(pin), client.WithToken(token))`. Dev mode: `client.Dial(ctx, client.URL(addr))`.
A `wss://` URL without a pin, or a pin with a `ws://` URL, is refused.

## Test

```sh
go vet ./... && go test -race ./...
go test ./internal/mockphone -run TestTranscripts -record   # regenerate spec/conformance
```

The tests start the mock in-process and drive it through the client and over raw WebSockets. They
cover every error code. They also validate every frame in both directions against
[`../spec/schemas/phonetopc-1.schema.json`](../spec/schemas/phonetopc-1.schema.json), check
that the committed conformance transcripts still match the mock, and run
[`server-cases.json`](../spec/conformance/server-cases.json) (dev mode) and
[`secure-server-cases.json`](../spec/conformance/secure-server-cases.json) (secure mode) against it.
`internal/security` checks [`vectors.json`](../spec/conformance/vectors/vectors.json).

End-to-end tests in `cmd/phonetopc` pair the CLI with the mock's pairing client (QR and code
comparison), then stream securely, and cover pin mismatch, revocation, re-pairing, expiry and
device selection. The tests that use real mDNS skip themselves where multicast doesn't work
(`go test -short` skips them outright); the same flows are also tested with addresses given
directly.

## Dependencies

| Module | Why |
|---|---|
| `github.com/coder/websocket` | WebSocket client and server |
| `github.com/libp2p/zeroconf/v2` | mDNS/DNS-SD in pure Go (maintained fork of grandcat/zeroconf); brings `miekg/dns` and `golang.org/x/net` |
| `github.com/skip2/go-qrcode` | QR code encoder for the terminal and `--png`; no dependencies |
| `github.com/santhosh-tekuri/jsonschema/v6` | Tests only: schema validation |
