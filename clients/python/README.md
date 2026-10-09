# phonetopc for Python

The official Python client for [phonetopc](../../README.md): stream Android phone sensor events to your PC over the `phonetopc/1` WebSocket JSON-RPC protocol ([spec](../../spec/PROTOCOL.md) and [security spec](../../spec/SECURITY.md), draft 2).

- **Secure by default:** TLS pinned to the phone's key, plus a per-PC token, using the pairing made by `phonetopc pair`
- asyncio API with typed events, plus a small blocking wrapper
- One dependency: [`websockets`](https://websockets.readthedocs.io/); TLS pinning uses only the standard library
- Optional mDNS discovery with [`zeroconf`](https://pypi.org/project/zeroconf/)
- Python 3.9+, fully typed (`py.typed`)

## Install

From a checkout of the repo:

```sh
pip install -e clients/python                  # or:
pip install -e 'clients/python[discovery]'     # also find the phone via mDNS when its IP changes
```

## Quickstart

Pair the phone once with the Go CLI (see [`pcd/`](../../pcd)): run `phonetopc pair` and scan the QR code with the phone app. Then:

```python
import asyncio
import phonetopc

async def main():
    async with phonetopc.connect(client_name="demo") as phone:  # the paired phone
        print(phone.device)                       # from the hello handshake
        battery = await phone.get("battery")
        print(battery.value)                      # {'level': 82, 'charging': True}
        async with phone.subscribe("gyro", rate_hz=100) as events:
            async for ev in events:
                print(ev.seq, ev.value, ev.unit)  # 17 [0.01, -0.32, 0.05] rad/s

asyncio.run(main())
```

Or from the shell:

```sh
python -m phonetopc info                  # device, server and connection security
python -m phonetopc list                  # sensors on this phone
python -m phonetopc tail gyro --rate 50   # stream events (--json for JSON lines, -n N to stop)
python -m phonetopc get battery
python -m phonetopc list --device "Pixel 8"         # pick one of several paired phones
python -m phonetopc list --dev --addr 192.168.1.20  # a phone in dev mode (plaintext)
```

More complete scripts are in [`examples/`](../../examples).

## Secure mode and dev mode

The phone serves exactly one of two modes ([SECURITY.md](../../spec/SECURITY.md) section 3):

- **Secure mode** (the default): `wss://`. The client checks that the phone's TLS public key matches the pin stored at pairing time, *before sending anything*, and then authenticates with the stored token in `hello`. There is no fallback to plaintext or to any other check: a phone with a different key raises `PinMismatch`.
- **Dev mode**: plaintext `ws://`, no authentication, for development. It is an explicit, off-by-default setting in the phone app. Only use it on a network you trust. Select it with `dev=True` (`--dev`) or a `ws://` URL. The mock phone (`phonetopc-mock`) speaks dev mode too.

### Which phone, at which address

`connect(addr=None, *, device=None, dev=False)` decides like this. `addr` defaults to `$PHONETOPC_ADDR`, which follows the same rules.

| You write | Mode | Connects to |
|---|---|---|
| `connect()` | secure | your only paired phone. Several paired: pass `device=` |
| `connect(device="Pixel 8")` | secure | the paired phone with that name or id |
| `connect("192.168.1.20")`, `"192.168.1.20:9000"`, `"[fe80::1]:8765"` or `"wss://host:port/v1/ws"` | secure | that address, expecting the paired phone last seen there; else the one named by `device=`; else your only paired phone |
| `connect("ws://192.168.1.20:8765")` | dev | that URL, in plaintext (an empty path becomes `/v1/ws`) |
| `connect("192.168.1.20", dev=True)` | dev | `ws://192.168.1.20:8765/v1/ws` |
| `connect(dev=True)` | dev | `ws://127.0.0.1:8765/v1/ws` |

Without an address, a secure connection finds the phone like this:

1. If [`zeroconf`](https://pypi.org/project/zeroconf/) is installed (`pip install 'phonetopc[discovery]'`), browse mDNS for `_phonetopc._tcp` with TXT `id` equal to the device id, for up to 2 seconds.
2. Then try the stored `addresses` in order. `open_timeout` applies to each.

When the phone answers somewhere new (a new DHCP lease, say), the client moves that address to the front of its stored `addresses`. Loopback addresses (adb forward) are never stored.

The pin is always checked, so connecting to an address "just to see" can't leak the token to a different phone.

Over USB: `adb forward tcp:8765 tcp:8765`, then `connect("127.0.0.1")` (secure, the paired phone), or `connect(dev=True)` for a phone in dev mode.

When something is wrong, the exception says what to do:

| Situation | Exception |
|---|---|
| Nothing paired yet | `NotPaired`: "no paired phone: run `phonetopc pair`" |
| Several paired phones, none chosen | `AmbiguousDevice`: "pass device=" |
| The phone's key changed (app reinstalled or reset, or another device at that address) | `PinMismatch`: "phone identity changed ... re-pair with `phonetopc pair`". Nothing was sent to it |
| The phone revoked this PC | `Unauthorized` (`-32010`): "run `phonetopc pair`" |
| No address worked | `PhoneUnreachable` (an `OSError`), listing each address tried and why, with hints such as "if the phone is in dev mode, pass dev=True" |

### The credential store

`phonetopc pair` saves pairings in `$XDG_CONFIG_HOME/phonetopc/devices.json` (default `~/.config/phonetopc/devices.json`), in the format of [SECURITY.md](../../spec/SECURITY.md) section 6. The Python client only reads pairings and never creates them. It writes to the file only to update a device's `addresses` (and its `port` when mDNS reports a new one). Those writes are atomic (temp file + rename), keep every other field, and always use mode `0600`.

The file holds secret tokens, so it should be mode `0600`. If other users can read it, the client still works but issues an `InsecurePermissionsWarning`. Fix it with `chmod 600 ~/.config/phonetopc/devices.json`.

`phonetopc.load_devices()` returns the pairings as `PairedDevice` objects (`id`, `name`, `pin`, `token`, `port`, `addresses`, `paired_at`). The token is left out of their `repr`.

## API overview

### `phonetopc.connect(addr=None, *, device=None, dev=False, client_name=..., request_timeout=10.0, open_timeout=10.0, queue_size=256, **ws_kwargs)`

Opens the WebSocket (see [which phone, at which address](#which-phone-at-which-address)) and runs `hello`. Use it as `async with connect(...) as phone:` or `phone = await connect(...)` followed by `await phone.close()`. Extra keyword arguments go to `websockets.asyncio.client.connect`, except `ssl`, which secure mode sets itself.

### `Phone`

| Member | |
|---|---|
| `device`, `server`, `capabilities`, `hello` | From the `hello` handshake (`DeviceInfo`, `ServerInfo`, `tuple[str, ...]`, `HelloResult`) |
| `secure`, `paired_device`, `pc`, `url` | `True` for pinned TLS + token; the `PairedDevice` used (`None` in dev mode); how the phone knows this PC (`PCInfo` from `hello`, secure mode only); the URL connected to |
| `await list_sensors()` | `list[SensorInfo]`, including sources this client doesn't know about |
| `await get(source, type=None)` | One-shot read, returns an `Event` (with `subscription`/`seq` set to `None`) |
| `subscribe(source, *, type=None, rate_hz=None, min_delta=None, queue_size=None, overflow="auto")` | Returns a `Subscription`; `async with` it to start |
| `await call(method, params=None, timeout=...)` | Raw JSON-RPC call, for methods newer than this client |
| `closed`, `closed_reason`, `await wait_closed()`, `await close()` | Connection state |

Requests can run concurrently: one reader task matches responses to requests by `id` and routes events to their subscription.

### `Subscription`

An async context manager and async iterator of `Event`. Leaving the `async with` block sends `unsubscribe`.

For an `on_change` source with state (battery, light, proximity) the server sends the current value first, as `seq` 0, so you learn the state without waiting for a change. `ui` sends nothing until the tile is tapped.

- `id`, `mode` (`"continuous"` or `"on_change"`), `rate_hz` (effective rate reported by the server, continuous sensors only), `info` (`SubscribeResult`)
- `dropped`: total events lost. That's `dropped_remote` (gaps in `seq`, meaning the server dropped them) plus `dropped_local` (discarded because the local queue was full)
- `received`, `qsize()`, `closed`, `closed_reason`

The local queue holds `queue_size` events. When it's full:

- **continuous sensors** (gyro, accelerometer, ...): the oldest event is dropped and counted. Fresh data matters more than complete data.
- **on_change sensors** (battery, ui, ...): the protocol never drops these, so the client doesn't either. Iteration raises `SubscriptionOverflow` after the queued events, and the subscription is closed.

Pass `overflow="drop_oldest"` or `overflow="error"` to choose the policy yourself. With `"auto"` the client goes by the `mode` the server reports in the `subscribe` result. (Draft 0 servers didn't send `mode`; then a reported `rate_hz` means continuous.)

When the connection is lost, pending calls raise `ConnectionClosed` and every subscription's iterator ends; check `phone.closed_reason` or `events.closed_reason` to see why.

### `Event`

A frozen dataclass: `source`, `type`, `value`, `unit`, `ts` (Unix ms), `ts_mono_ns` (phone monotonic clock, use it for deltas), `device`, `subscription`, `seq`, and `.datetime` (`ts` as an aware UTC `datetime`).

`DeviceInfo`, `ServerInfo`, `HelloResult`, `PCInfo`, `SensorInfo` and `SubscribeResult` are frozen dataclasses too. Unknown fields from newer servers are kept in `.raw`.

### Errors

All exceptions derive from `phonetopc.Error`.

| Exception | When |
|---|---|
| `PhonetopcError(code, message, data)` | Any JSON-RPC error response. The subclass is picked by code: |
| ↳ `UnknownSource` (-32001), `SourceUnavailable` (-32002), `HelloRequired` (-32003), `UnsupportedProtocol` (-32004, `.supported`), `UnknownSubscription` (-32005), `NoValue` (-32006), `Unauthorized` (-32010) | phonetopc errors |
| ↳ `ParseError`, `InvalidRequest`, `MethodNotFound`, `InvalidParams`, `InternalError` | standard JSON-RPC errors |
| `ConnectionClosed` (`.code`, `.reason`) | The connection is closed or was lost |
| `RequestTimeout` (also a `TimeoutError`) | No response within `request_timeout` |
| `SubscriptionOverflow` | An on_change subscription's queue overflowed |
| `ProtocolError` | The server sent a malformed message |
| `NotPaired`, `AmbiguousDevice` | No pairing fits, or several do (both are also `LookupError`s) |
| `PinMismatch` (`.expected`, `.actual`, `.address`) | The phone's TLS key isn't the paired one |
| `PhoneUnreachable` (`.attempts`) | Secure mode: no address worked (also an `OSError`) |
| `CredentialStoreError` | `devices.json` is unreadable, malformed or of an unknown version |

In dev mode, failing to open the socket raises the usual `OSError` / `TimeoutError`.

### Blocking API

For scripts that don't want asyncio, `phonetopc.sync` runs the same client on a background thread:

```python
import phonetopc.sync

with phonetopc.sync.connect() as phone:   # same arguments as phonetopc.connect
    print(phone.get("battery").value)
    with phone.subscribe("gyro", rate_hz=50) as events:
        for ev in events:
            print(ev.value)
```

## Development

```sh
cd clients/python
python -m venv .venv && . .venv/bin/activate
pip install -e '.[dev]'
pytest                 # runs against an in-process fake server (dev and secure mode)
ruff check src tests
mypy                   # strict
```

The tests never touch your real `devices.json` (each test gets an empty `XDG_CONFIG_HOME`) and never use real mDNS, except `test_real_mdns`: it runs when `zeroconf` is installed and multicast works, and `PHONETOPC_TEST_MDNS=1` turns "multicast doesn't work here" from a skip into a failure. Secure-mode tests use the test-vector certificates in [`spec/conformance/vectors/`](../../spec/conformance/vectors), and pins are checked against `vectors.json`.

The tests validate every request the client sends against [`spec/schemas/phonetopc-1.schema.json`](../../spec/schemas/phonetopc-1.schema.json), and replay any transcripts in `spec/conformance/*.jsonl` through the client's message handling.
