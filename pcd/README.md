# pcd: phonetopc on the PC (Go)

The PC side of phonetopc:

| Binary | What |
|---|---|
| `phonetopcd` | The daemon. Keeps one connection to the paired phone and offers it to programs on this PC as plain HTTP ([`../spec/LOCAL-API.md`](../spec/LOCAL-API.md)) |
| `phonetopc` | CLI: pair with a phone, then read sensors, ask for fingerprint approval and post notifications |
| `phonetopc-mock` | A fake phone with synthetic sensor data, for trying things without a phone |

Programs on the PC should use `phonetopcd`'s HTTP API, from any language. The Go client library in `client/` talks
to the phone directly; the CLI and the daemon are built on it.

The protocol is specified in [`../spec/`](../spec/): [`PROTOCOL.md`](../spec/PROTOCOL.md),
[`SECURITY.md`](../spec/SECURITY.md) (pairing, TLS, tokens, the credential store),
[`APPROVAL.md`](../spec/APPROVAL.md) and [`PANELS.md`](../spec/PANELS.md).

## Build

You need Go 1.27 or newer. The binaries are static, with no cgo.

```sh
cd pcd
CGO_ENABLED=0 go build -o bin/ ./cmd/...     # bin/phonetopc, bin/phonetopcd, bin/phonetopc-mock
```

## Pair with a phone

```sh
bin/phonetopc pair
```

This prints a QR code and also advertises the PC on the network. On the phone, open phonetopc → *Pair a PC*, then
either scan the QR code, or tap the PC under *Find on network* and check that both screens show the same 6-digit
code. Confirm on the PC with `y`.

The pairing listener uses TCP port 8766. If a firewall is active, `phonetopc pair` prints the rule to add. Pairing
details are in [`SECURITY.md`](../spec/SECURITY.md) §4.

| `pair` flag | Meaning |
|---|---|
| `--yes` | Accept the QR code without asking (for scripts). Turns off *Find on network* |
| `--png FILE` | Also write the QR code to a PNG file |
| `--timeout D` | Give up after D (default 5m) |
| `--listen ADDR` | Listen address (default `:8766`; a random port if 8766 is busy) |
| `--host H[,H...]` | Addresses to put in the QR code (default: all LAN addresses) |

Pairings are stored in `~/.config/phonetopc/` (`$XDG_CONFIG_HOME`), as described in `SECURITY.md` §6. The token in
`devices.json` is a credential, like an SSH key.

## Run the daemon

```sh
bin/phonetopcd

S=$XDG_RUNTIME_DIR/phonetopc.sock
curl -s --unix-socket $S http://phonetopc/v1/status
curl -sN --unix-socket $S 'http://phonetopc/v1/events?source=gyro&rate=20'
curl -s -H "Authorization: Bearer $(cat $XDG_RUNTIME_DIR/phonetopc.token)" http://127.0.0.1:8767/v1/status
```

It serves on a Unix socket only this user can open, and on `127.0.0.1:8767` with a token. It reconnects by itself
when the phone comes back. The API is in [`LOCAL-API.md`](../spec/LOCAL-API.md).

| Flag | Meaning |
|---|---|
| `--device D` | Paired phone to use, by name or id (`$PHONETOPC_DEVICE`); needed only when several are paired |
| `--addr host:port` | Connect to this address instead of looking for the phone (`$PHONETOPC_ADDR`) |
| `--insecure-dev` | Plaintext `ws://` to a phone in developer mode, or the mock without `--state` (`$PHONETOPC_INSECURE_DEV=1`) |
| `--no-mdns` | Don't look for the phone with mDNS (`$PHONETOPC_NO_MDNS=1`) |
| `--socket PATH` | Unix socket (default `$XDG_RUNTIME_DIR/phonetopc.sock`); `off` turns it off |
| `--listen ADDR` | Loopback TCP address (default `127.0.0.1:8767`); `off` turns it off |
| `--token-file PATH` | Where the TCP token is kept (default `$XDG_RUNTIME_DIR/phonetopc.token`) |
| `-v` | Log every reconnect attempt, not only new errors |
| `--version` | Print the version |

To start it at login, use the systemd user unit in `systemd/`:

```sh
go install ./cmd/phonetopcd          # the unit expects ~/go/bin/phonetopcd
cp systemd/phonetopcd.service ~/.config/systemd/user/
systemctl --user enable --now phonetopcd
journalctl --user -u phonetopcd -f
```

Without systemd, start `phonetopcd` from your desktop's autostart.

## Use the CLI

```sh
bin/phonetopc devices                       # paired phones
bin/phonetopc info                          # the phone's hello information
bin/phonetopc list                          # its sensors
bin/phonetopc get battery
bin/phonetopc tail gyro --rate 100          # until Ctrl-C; seq gaps are reported on stderr
bin/phonetopc tail ui --count 1             # wait for one Quick Settings tile tap
bin/phonetopc approve "Deploy?" && ./deploy.sh
bin/phonetopc notify "Build done" --body "3 files changed"
bin/phonetopc notify "Deploy?" --action yes:Deploy --action no:Later --wait   # prints yes, no, tap or dismiss
bin/phonetopc unpair "Pixel 8"              # forget a phone (also revoke the PC on the phone)
bin/phonetopc pc                            # this PC's id, name and pin
```

| Command | Meaning |
|---|---|
| `pair` | Pair with a phone (see above) |
| `devices` | List paired phones |
| `unpair <device>` | Forget a paired phone |
| `pc` | Show this PC's id, name and pin |
| `info` | Show the phone's hello information |
| `list` | List the phone's sensors |
| `get <source>` | Read the latest value of a source |
| `tail <source>` | Stream events of a source until Ctrl-C |
| `approve "reason"` | Ask for approval with a fingerprint on the phone |
| `approval-key` | Show the phone's accepted and current approval key |
| `notify "title"` | Post a notification on the phone and print its id |

| Flag | Meaning |
|---|---|
| `--device D` | Paired phone, by name or id (`$PHONETOPC_DEVICE`); needed only when several are paired |
| `--addr host:port` | Connect there instead of looking for the phone (`$PHONETOPC_ADDR`) |
| `--insecure-dev` | Plaintext `ws://` to a phone in developer mode, or the mock without `--state`; default address `127.0.0.1:8765` (`$PHONETOPC_INSECURE_DEV=1`) |
| `--no-mdns` | Don't look for the phone with mDNS (`$PHONETOPC_NO_MDNS=1`) |
| `--json` | Print raw JSON |
| `tail --type T` | Only events of type T |
| `tail --rate HZ` | Requested rate for continuous sensors |
| `tail --min-delta D` | Suppress events that changed by less than D |
| `tail --count N` | Exit after N events (0 = until Ctrl-C) |
| `approve --timeout D` | How long the phone waits for the user (default 30s, 5s to 2m) |
| `approval-key --accept` | Accept a changed approval key |
| `notify --body TEXT` | Text under the title (`\n` starts a new line) |
| `notify --id ID` | Replace the earlier notification with this id |
| `notify --action ID:LABEL` | Add a button (up to 3; `LABEL` alone derives the id) |
| `notify --high` | Ask for a heads-up notification |
| `notify --wait` | Wait and print what the user did: the action's id, `tap` or `dismiss` |

Flags go before or after the command.

**`approve`** exits 0 when approved and the phone's signature verified, 1 when not approved (`denied`,
`cancelled`, `timeout`, `busy`, `lockout`, `unavailable`; the reason is printed), and 2 on any error. The reason is
shown on the phone: 1–200 characters, no leading or trailing whitespace, no control characters.

**Notifications and panels** are off on the phone for every PC until the user allows them there; until then the
calls fail with `-32030`. Panels need a program that stays connected; use the daemon's `/v1/panels`.

## Troubleshooting

- **Phone identity changed — re-pair.** The phone presented a different key than the one stored. Nothing was sent
  to it. If you reinstalled the app, run `phonetopc pair` again.
- **This PC was revoked** (`-32010`). The phone's user revoked this PC. Pair again.
- **Phone not found.** The phone's server must be running (*Home → Start server*). mDNS is often blocked; the PC
  also tries the phone's last known addresses, and `--addr host:port` sets one by hand.
- **Approval key changed.** Fingerprints were added or removed on the phone, so it made a new approval key.
  Approvals are refused until you run `phonetopc approval-key --accept`.
- **`not_allowed` / `-32030`.** Allow notifications or panels for this PC in the phone app under *Devices*.

## Run the mock

```sh
bin/phonetopc-mock                          # dev mode: ws://127.0.0.1:8765/v1/ws
bin/phonetopc --insecure-dev info
```

With a state directory it runs in secure mode and can be paired with:

```sh
bin/phonetopc pair                                                      # terminal 1
bin/phonetopc-mock pair --state /tmp/mock 'phonetopc://pair?v=1&...'    # terminal 2: the URI from terminal 1
bin/phonetopc-mock --state /tmp/mock                                    # terminal 2: serve
bin/phonetopc info                                                      # terminal 1
```

`bin/phonetopc-mock -h` lists its commands (`pair`, `pcs`, `revoke`, `approval-key`) and flags, such as
`--approval` (how its "user" answers approvals), `--user-every` (a simulated user for panels and notifications),
`--unavailable` and `--mdns`.

## Development

```sh
go vet ./... && go test -race ./...
go test -short ./...                                        # skip the tests that need real mDNS
go test ./internal/mockphone -run TestTranscripts -record   # regenerate spec/conformance transcripts
```

The tests run the conformance cases and vectors in [`../spec/conformance`](../spec/conformance) against the mock
and the client, and validate every frame against the JSON Schema.
