# pcd: phonetopc on the PC (Go)

The Go side of phonetopc. It implements protocol `phonetopc/1` (draft 0, see
[`../spec/PROTOCOL.md`](../spec/PROTOCOL.md)).

| Path | What |
|---|---|
| `cmd/phonetopc-mock` | Mock phone server with synthetic sensor data, for working without a phone |
| `cmd/phonetopc` | CLI that talks to a phone or the mock: `info`, `list`, `get`, `tail` |
| `client` | Importable Go client library |
| `internal/protocol` | Wire types, error codes and the sensor catalog, shared by everything here |
| `internal/mockphone` | The mock server implementation, also used in-process by tests |

The `phonetopcd` daemon (milestone M3) will go in `cmd/phonetopcd`, with its internals under `internal/`.

## Build

You need Go 1.27 or newer. The binaries are static, with no cgo.

```sh
cd pcd
CGO_ENABLED=0 go build -o bin/ ./cmd/...     # bin/phonetopc-mock, bin/phonetopc
```

## Run the mock

```sh
bin/phonetopc-mock                          # ws://127.0.0.1:8765/v1/ws
bin/phonetopc-mock --addr 0.0.0.0:8765 --tap-every 3s --unavailable light -v
```

| Flag | Meaning |
|---|---|
| `--addr` | Listen address. Default `127.0.0.1:8765` |
| `--tap-every` | Interval between synthetic `ui/tile_tap` events. Default `10s`; `0` turns them off |
| `--unavailable` | Comma-separated list of sources to report as `available: false` |
| `--device-id` | Device id. Default `mock-0001` |
| `--seed` | Simulation seed |
| `-v` | Log every frame |

What the mock simulates:

- The motion sensors model one phone held in a hand. It rolls and pitches gently and turns slowly.
  - The gyro reports the matching angular rate.
  - The accelerometer includes gravity (about 9.81 on z when face up) plus some hand shake.
  - `game_rotation_vector` is the matching unit quaternion.
- Light changes every few seconds.
- Proximity toggles about every 15 s.
- The battery drains by 1% a minute, and starts charging when it reaches 15%.

The mock implements all of draft 0, including the error codes, ping/pong keepalive and
backpressure. It is the reference for the transcripts in [`../spec/conformance`](../spec/conformance).

## Use the CLI

```sh
export PHONETOPC_ADDR=127.0.0.1:8765        # or pass --addr host:port
bin/phonetopc info
bin/phonetopc list
bin/phonetopc get battery
bin/phonetopc tail gyro --rate 100
bin/phonetopc tail accelerometer --min-delta 0.5 --json | jq -c .value
bin/phonetopc tail ui --count 1             # wait for one tile tap
```

`tail` prints one line per event and keeps going until you press Ctrl-C. It reports `seq` gaps
(dropped events) on stderr.

## Use the client library

```go
c, err := client.Dial(ctx, client.URL("127.0.0.1:8765"))
if err != nil { ... }
defer c.Close()
sub, err := c.Subscribe(ctx, "gyro", client.SubscribeOptions{RateHz: 100})
for ev := range sub.Events() {
	v, _ := ev.Vec3()
	fmt.Println(ev.Seq, v)
}
```

## Test

```sh
go vet ./... && go test -race ./...
go test ./internal/mockphone -run TestTranscripts -record   # regenerate spec/conformance
```

The tests start the mock in-process and drive it through the client and over raw WebSockets. They
cover every error code. They also validate every frame in both directions against
[`../spec/schemas/phonetopc-1.schema.json`](../spec/schemas/phonetopc-1.schema.json), and check
that the committed conformance transcripts still match the mock.
