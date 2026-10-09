# phonetopc for Python

The official Python client for [phonetopc](../../README.md): stream Android phone sensor events to your PC over the `phonetopc/1` WebSocket JSON-RPC protocol ([spec](../../spec/PROTOCOL.md)).

- asyncio API with typed events, plus a small blocking wrapper
- One dependency: [`websockets`](https://websockets.readthedocs.io/)
- Python 3.9+, fully typed (`py.typed`)

## Install

From a checkout of the repo:

```sh
pip install -e clients/python
```

## Quickstart

Turn on dev mode in the phone app, note the phone's IP, then:

```python
import asyncio
import phonetopc

async def main():
    async with phonetopc.connect("192.168.1.20", client_name="demo") as phone:
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
export PHONETOPC_ADDR=192.168.1.20
python -m phonetopc info                  # device and server
python -m phonetopc list                  # sensors on this phone
python -m phonetopc tail gyro --rate 50   # stream events (--json for JSON lines, -n N to stop)
python -m phonetopc get battery
```

More complete scripts are in [`examples/`](../../examples).

## Addresses

`connect(addr)` and `--addr` accept:

| You write | Connects to |
|---|---|
| `None` / nothing | `$PHONETOPC_ADDR`, else `127.0.0.1:8765` |
| `192.168.1.20` | `ws://192.168.1.20:8765/v1/ws` |
| `192.168.1.20:9000` | `ws://192.168.1.20:9000/v1/ws` |
| `[fe80::1]:8765` or `fe80::1` | `ws://[fe80::1]:8765/v1/ws` |
| `ws://host:port/path` | used as is (an empty path becomes `/v1/ws`) |

Over USB: `adb reverse tcp:8765 tcp:8765`, then connect to `127.0.0.1`.

## API overview

### `phonetopc.connect(addr=None, *, client_name=..., request_timeout=10.0, open_timeout=10.0, queue_size=256, **ws_kwargs)`

Opens the WebSocket and runs `hello`. Use it as `async with connect(...) as phone:` or `phone = await connect(...)` followed by `await phone.close()`. Extra keyword arguments go to `websockets.asyncio.client.connect`.

### `Phone`

| Member | |
|---|---|
| `device`, `server`, `capabilities`, `hello` | From the `hello` handshake (`DeviceInfo`, `ServerInfo`, `tuple[str, ...]`, `HelloResult`) |
| `await list_sensors()` | `list[SensorInfo]`, including sources this client doesn't know about |
| `await get(source, type=None)` | One-shot read, returns an `Event` (with `subscription`/`seq` set to `None`) |
| `subscribe(source, *, type=None, rate_hz=None, min_delta=None, queue_size=None, overflow="auto")` | Returns a `Subscription`; `async with` it to start |
| `await call(method, params=None, timeout=...)` | Raw JSON-RPC call, for methods newer than this client |
| `closed`, `closed_reason`, `await wait_closed()`, `await close()` | Connection state |

Requests can run concurrently: one reader task matches responses to requests by `id` and routes events to their subscription.

### `Subscription`

An async context manager and async iterator of `Event`. Leaving the `async with` block sends `unsubscribe`.

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

`DeviceInfo`, `ServerInfo`, `HelloResult`, `SensorInfo` and `SubscribeResult` are frozen dataclasses too. Unknown fields from newer servers are kept in `.raw`.

### Errors

All exceptions derive from `phonetopc.Error`.

| Exception | When |
|---|---|
| `PhonetopcError(code, message, data)` | Any JSON-RPC error response. The subclass is picked by code: |
| ↳ `UnknownSource` (-32001), `SourceUnavailable` (-32002), `HelloRequired` (-32003), `UnsupportedProtocol` (-32004, `.supported`), `UnknownSubscription` (-32005), `NoValue` (-32006) | phonetopc errors |
| ↳ `ParseError`, `InvalidRequest`, `MethodNotFound`, `InvalidParams`, `InternalError` | standard JSON-RPC errors |
| `ConnectionClosed` (`.code`, `.reason`) | The connection is closed or was lost |
| `RequestTimeout` (also a `TimeoutError`) | No response within `request_timeout` |
| `SubscriptionOverflow` | An on_change subscription's queue overflowed |
| `ProtocolError` | The server sent a malformed message |

Failing to open the socket raises the usual `OSError` / `TimeoutError`.

### Blocking API

For scripts that don't want asyncio, `phonetopc.sync` runs the same client on a background thread:

```python
import phonetopc.sync

with phonetopc.sync.connect("192.168.1.20") as phone:
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
pytest                 # runs against an in-process fake server
ruff check src tests
mypy                   # strict
```

The tests validate every request the client sends against [`spec/schemas/phonetopc-1.schema.json`](../../spec/schemas/phonetopc-1.schema.json), and replay any transcripts in `spec/conformance/*.jsonl` through the client's message handling.
