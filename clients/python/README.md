# phonetopc for Python

The official Python client for [phonetopc](../../README.md): stream Android phone sensor events to your PC over the `phonetopc/1` WebSocket JSON-RPC protocol ([spec](../../spec/PROTOCOL.md), [security spec](../../spec/SECURITY.md), [approval spec](../../spec/APPROVAL.md) and [panels spec](../../spec/PANELS.md), draft 4).

- **Secure by default:** TLS pinned to the phone's key, plus a per-PC token, using the pairing made by `phonetopc pair`
- **Fingerprint approval:** ask for a fingerprint on the phone before your program does something, with the phone's signature verified on the PC
- **Notifications and panels:** post notifications on the phone, or put buttons, toggles, sliders, text and a drawing canvas on its screen and get the user's input back
- asyncio API with typed events, plus a small blocking wrapper
- One dependency: [`websockets`](https://websockets.readthedocs.io/); TLS pinning uses only the standard library
- Optional mDNS discovery with [`zeroconf`](https://pypi.org/project/zeroconf/), optional approval verification with [`cryptography`](https://pypi.org/project/cryptography/)
- Python 3.9+, fully typed (`py.typed`)

## Install

From a checkout of the repo:

```sh
pip install -e clients/python                  # or:
pip install -e 'clients/python[discovery]'     # also find the phone via mDNS when its IP changes
pip install -e 'clients/python[approval]'      # fingerprint approval (request_approval)
```

## Quickstart

Pair the phone once with the Go CLI (see [`pcd/`](../../pcd)): run `phonetopc pair` and scan the QR code with the phone app. Then:

```python
import asyncio
import phonetopc


async def main():
    async with phonetopc.connect(client_name="demo") as phone:  # the paired phone
        print(phone.device)  # from the hello handshake
        battery = await phone.get("battery")
        print(battery.value)  # {'level': 82, 'charging': True}
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
python -m phonetopc approval-key [--accept]         # show (or trust) the phone's approval key
```

More complete scripts are in [`examples/`](../../examples).

## Secure mode and dev mode

The phone serves exactly one of two modes ([SECURITY.md](../../spec/SECURITY.md) section 3):

- **Secure mode** (the default): `wss://`. The client checks that the phone's TLS public key matches the pin stored at pairing time, *before sending anything*, and then authenticates with the stored token in `hello`. There is no fallback to plaintext or to any other check: a device with a different key is never talked to, and if no address reaches the paired key, `PinMismatch` is raised.
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

The pin is always checked, so connecting to an address "just to see" can't leak the token to a different phone. A device with a different key at one address (say, a stale DHCP lease now held by someone else) is skipped like an unreachable address; `PinMismatch` is raised only if no address works.

Over USB: `adb forward tcp:8765 tcp:8765`, then `connect("127.0.0.1")` (secure, the paired phone), or `connect(dev=True)` for a phone in dev mode.

When something is wrong, the exception says what to do:

| Situation | Exception |
|---|---|
| Nothing paired yet | `NotPaired`: "no paired phone: run `phonetopc pair`" |
| Several paired phones, none chosen | `AmbiguousDevice`: "pass device=" |
| The phone's key changed (app reinstalled or reset, or another device at that address), and no address had the paired key | `PinMismatch`: "phone identity changed ... re-pair with `phonetopc pair`". Nothing was sent to it |
| The phone revoked this PC | `Unauthorized` (`-32010`): "run `phonetopc pair`" |
| No address worked | `PhoneUnreachable` (an `OSError`), listing each address tried and why, with hints such as "if the phone is in dev mode, pass dev=True" |

### The credential store

`phonetopc pair` saves pairings in `$XDG_CONFIG_HOME/phonetopc/devices.json` (default `~/.config/phonetopc/devices.json`), in the format of [SECURITY.md](../../spec/SECURITY.md) section 6. The Python client only reads pairings and never creates them. It writes to the file only to update a device's `addresses` (and its `port` when mDNS reports a new one) and to record its approval key (see [fingerprint approval](#fingerprint-approval)). Each write takes an exclusive `flock` on `.lock` in the same directory (skipped where `fcntl` doesn't exist), re-reads the file, and replaces it atomically (temp file + rename) with mode `0600`, keeping every other field, including ones this client doesn't know.

The file holds secret tokens, so it should be mode `0600`. If other users can read it, the client still works but issues an `InsecurePermissionsWarning`. Fix it with `chmod 600 ~/.config/phonetopc/devices.json`.

`phonetopc.load_devices()` returns the pairings as `PairedDevice` objects (`id`, `name`, `pin`, `token`, `port`, `addresses`, `paired_at`, `approval_key`). The token is left out of their `repr`.

## Fingerprint approval

`request_approval` asks the user to confirm something with a fingerprint on the phone ([APPROVAL.md](../../spec/APPROVAL.md)). It needs secure mode and `pip install 'phonetopc[approval]'`.

```python
import phonetopc.sync


def approved(reason: str) -> bool:
    with phonetopc.sync.connect() as phone:  # the paired phone
        return bool(phone.request_approval(reason))


if approved("Run deploy.sh on arch-desktop?"):
    ...
```

Or with asyncio: `result = await phone.request_approval("Run deploy?", timeout=30.0)`. The result is an `ApprovalResult`: `result.approved` (and `bool(result)`), and `result.why` when not approved: `"denied"`, `"cancelled"`, `"timeout"`, `"busy"` (another approval is pending), `"lockout"` (too many failed fingerprints) or `"unavailable"` (no fingerprint enrolled, or the phone can't do approvals). [`examples/approve.py`](../../examples/approve.py) runs a command only when approved.

A "yes" counts only if it is proven. The phone signs its answer with a key in its Keystore that works only right after a fingerprint, and the client checks that signature itself, over a fresh random nonce, the exact `reason` it sent and the ids of this session, all from its own copies. So `result.approved` is `True` only after that check passed; an `approved: true` without a valid signature raises `ApprovalVerificationError` instead.

`reason` is shown on the phone: 1-200 characters after trimming, without control characters (it is sent trimmed). `timeout` is how long the phone waits for the user, 5-120 seconds; the client waits 5 seconds longer for the answer, then raises `RequestTimeout`.

**The approval key.** The first time a phone offers approvals, the client stores the pin of its approval key as `approval_key` in `devices.json`. The phone replaces that key when fingerprints are added or removed. Until you accept the new one, `request_approval` raises `ApprovalKeyChanged` ("fingerprints changed on the phone? run `phonetopc approval-key --accept`") and sends nothing. `python -m phonetopc approval-key --accept` and `await phone.accept_approval_key()` do the same as the Go command. Re-pairing clears the stored key.

| Situation | Result |
|---|---|
| Approved, signature verified | `ApprovalResult(approved=True)` |
| Denied, cancelled, timed out on the phone, busy, lockout, unavailable | `ApprovalResult(approved=False, why=...)` |
| `approved: true` with a missing or invalid signature | `ApprovalVerificationError` |
| Approval key differs from the accepted one | `ApprovalKeyChanged` (nothing sent) |
| `cryptography` not installed | `ApprovalDependencyMissing`, an `ImportError`: "pip install 'phonetopc[approval]'" (nothing sent) |
| Dev mode | `ApprovalNotSupported` (nothing sent) |
| No answer within `timeout` + 5 s | `RequestTimeout` |

## Notifications and panels

A PC program can also use the phone as a screen ([PANELS.md](../../spec/PANELS.md)). Both are **off by default** for every PC: the user turns on *Notifications* and *Screen panels* for this PC in the phone app. Until then the calls raise `NotAllowed`, whose message says which setting to turn on.

```python
import asyncio
import phonetopc
from phonetopc import ui


async def main():
    async with phonetopc.connect() as phone:
        await phone.notify("Build finished", "3 files changed.\nAll tests pass.")

        panel = await phone.show_panel(
            "deck",
            "Stream deck",
            ui.column(
                ui.text("Idle", id="status"),
                ui.grid(2, ui.button("mute", "🔇 Mute"), ui.toggle("mic", "Mic", value=True)),
                ui.slider("vol", label="Volume", min=0, max=100, step=1, value=40),
            ),
            keep_awake=True,
        )
        async for ev in panel:
            if isinstance(ev, phonetopc.PressEvent) and ev.widget == "mute":
                await panel.update("status", text="Muted", tone="bad")
            elif isinstance(ev, phonetopc.ChangeEvent):  # toggle: bool, slider: number
                print(ev.widget, ev.value, ev.final)


asyncio.run(main())
```

**Notifications.** `await phone.notify(title, body=None, *, id=None, actions=None, priority=None)` returns the notification's id; posting the same `id` again replaces it, `await phone.notify_cancel(id)` removes it. `actions` are up to 3 buttons, `{"open": "Open", "ok": "OK"}`. What the user does comes as events of the `notification` source: `async with phone.subscribe("notification") as events:` gives events of type `action` (`value == {"notification": id, "action": "open"}`), `tap` or `dismiss`.

**Layouts** are JSON widget trees; `phonetopc.ui` builds them: `column`, `row`, `grid(cols, ...)` hold widgets; `text`, `button`, `toggle`, `slider`, `progress`, `text_input` and `canvas` are the rest. Interactive widgets need an id (their first argument). `weight=` shares space in rows and columns, `tone=` (`"good"`, `"warn"`, `"bad"`, ...) colors text, buttons and progress bars. `show_panel` checks the whole layout before sending and raises `ValueError` with the reason; pass `validate=False` to send widget types this client doesn't know yet.

**`Panel`** (from `show_panel` or `attach_panel`) is an async iterator of typed events, all with `panel`, `pseq`, `type`, `widget`, `replayed` and the raw `event`:

| Event | When | Extra fields |
|---|---|---|
| `StateEvent` | The panel became visible or hidden, or was closed on the phone (iteration ends after it) | `state` (`"visible"`, `"hidden"`, `"closed"`), `reason` (`"user"`, `"revoked"`) |
| `PressEvent` | A button was pressed | |
| `ChangeEvent` | A toggle or slider changed | `value` (bool or number), `final` (slider released) |
| `SubmitEvent` | Text was sent from a `text_input` | `value` |
| `StrokeEvent` | A chunk of a canvas stroke, while it is drawn | `stroke`, `phase` (`"start"`, `"move"`, `"end"`, `"cancel"`), `tool`, `points` (`Point(x, y, pressure, t)`, x/y in 0-1), `size` (on `start`) |

`await panel.update(widget, **props)` and `await panel.update_many({widget: props})` change widgets in place (checked against the layout first); `await panel.clear_canvas(widget)`; `await panel.close()` (also on leaving `async with panel:`). `panel.visible` follows the state events. `await phone.list_panels()` lists this PC's panels; a 9th panel raises `TooManyPanels`.

**Reconnecting.** Panels belong to the PC, not the connection: when the connection drops, the phone keeps the panel (greyed out, for 10 minutes) and its canvas keeps taking ink. `panel.last_pseq` is the number of the last event you got; on a new connection `await phone.attach_panel("pad", after_pseq=old.last_pseq)` takes the panel over and replays the strokes drawn since (`replayed=True`). `after_pseq=-1` replays everything still on the canvases, for a program that restarted. `panel.attach_result.values` has the current toggle and slider values. A panel that is gone raises `UnknownPanel`.

The client subscribes to the `panel` source once per connection, before its first `show_panel`, and hands each event to its `Panel`, so no event is missed between the response and the first event. See [`examples/`](../../examples): a stream deck, a drawing pad mirrored on the PC, and a notification when a command finishes.

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
| `await request_approval(reason, timeout=30.0)` | Fingerprint approval, returns an `ApprovalResult` (see [above](#fingerprint-approval)) |
| `await notify(title, body=None, *, id=None, actions=None, priority=None)`, `await notify_cancel(id)` | Post or remove a notification (see [notifications and panels](#notifications-and-panels)) |
| `await show_panel(panel, title, layout, *, keep_awake=None, orientation=None, validate=True)`, `await attach_panel(panel, *, after_pseq=-1)`, `await list_panels()` | Show, take over or list panels; the first two return a `Panel` |
| `approval_key`, `await accept_approval_key()` | Pin of the phone's approval key in this session (`None` if it has none); trust that key after it changed |
| `await call(method, params=None, timeout=...)` | Raw JSON-RPC call, for methods newer than this client |
| `closed`, `closed_reason`, `await wait_closed()`, `await close()` | Connection state |

Requests can run concurrently: one reader task matches responses to requests by `id` and routes events to their subscription.

### `Subscription`

An async context manager and async iterator of `Event`. Leaving the `async with` block sends `unsubscribe`.

For an `on_change` source with state (battery, light, proximity, location) the server sends the current value first, as `seq` 0, so you learn the state without waiting for a change. `ui` sends nothing until the tile is tapped, `nfc` nothing until a tag is scanned.

- `id`, `mode` (`"continuous"` or `"on_change"`), `rate_hz` (effective rate reported by the server, continuous sensors only), `info` (`SubscribeResult`)
- `dropped`: total events lost. That's `dropped_remote` (gaps in `seq`, meaning the server dropped them) plus `dropped_local` (discarded because the local queue was full)
- `received`, `qsize()`, `closed`, `closed_reason`

The local queue holds `queue_size` events. When it's full:

- **continuous sensors** (gyro, accelerometer, ...): the oldest event is dropped and counted. Fresh data matters more than complete data.
- **on_change sensors** (battery, ui, location, nfc, ...): the protocol never drops these, so the client doesn't either. Iteration raises `SubscriptionOverflow` after the queued events, and the subscription is closed.

Pass `overflow="drop_oldest"` or `overflow="error"` to choose the policy yourself. With `"auto"` the client goes by the `mode` the server reports in the `subscribe` result. (Draft 0 servers didn't send `mode`; then a reported `rate_hz` means continuous.)

When the connection is lost, pending calls raise `ConnectionClosed` and every subscription's iterator ends; check `phone.closed_reason` or `events.closed_reason` to see why.

### `Event`

A frozen dataclass: `source`, `type`, `value`, `unit`, `ts` (Unix ms), `ts_mono_ns` (phone monotonic clock, use it for deltas), `device`, `subscription`, `seq`, and `.datetime` (`ts` as an aware UTC `datetime`).

`value` is the decoded JSON of [PROTOCOL.md §6](../../spec/PROTOCOL.md#6-sensor-catalog): a `float` (`compass` in degrees, `barometer` in hPa, `light`, ...), a list for `vec3`/`quat` sources, `True` for `ui`, and a `dict` for `battery`, `location` (`lat`, `lon`, `accuracy_m`, optionally `altitude_m`, `speed_mps`, `bearing_deg`) and `nfc` (`id`, `tech`, optionally `ndef` records).

`DeviceInfo`, `ServerInfo`, `HelloResult`, `PCInfo`, `ApprovalInfo` (`hello.approval`), `SensorInfo` and `SubscribeResult` are frozen dataclasses too. Unknown fields from newer servers are kept in `.raw`.

### Errors

All exceptions derive from `phonetopc.Error`.

| Exception | When |
|---|---|
| `PhonetopcError(code, message, data)` | Any JSON-RPC error response. The subclass is picked by code: |
| ↳ `UnknownSource` (-32001), `SourceUnavailable` (-32002), `HelloRequired` (-32003), `UnsupportedProtocol` (-32004, `.supported`), `UnknownSubscription` (-32005), `NoValue` (-32006), `Unauthorized` (-32010), `NotAllowed` (-32030, `.permission`), `UnknownPanel` (-32031), `TooManyPanels` (-32032), `RateLimited` (-32033) | phonetopc errors |
| ↳ `ParseError`, `InvalidRequest`, `MethodNotFound`, `InvalidParams`, `InternalError` | standard JSON-RPC errors |
| `ConnectionClosed` (`.code`, `.reason`) | The connection is closed or was lost |
| `RequestTimeout` (also a `TimeoutError`) | No response within `request_timeout` |
| `SubscriptionOverflow` | An on_change subscription's (or a panel's) queue overflowed |
| `ProtocolError` | The server sent a malformed message |
| `NotPaired`, `AmbiguousDevice` | No pairing fits, or several do (both are also `LookupError`s) |
| `PinMismatch` (`.expected`, `.actual`, `.address`, `.attempts`) | The phone's TLS key isn't the paired one at any address tried |
| `PhoneUnreachable` (`.attempts`) | Secure mode: no address worked (also an `OSError`) |
| `CredentialStoreError` | `devices.json` is unreadable, malformed or of an unknown version |
| `ApprovalError` | Base of: `ApprovalKeyChanged` (`.stored`, `.actual`), `ApprovalVerificationError`, `ApprovalNotSupported` |
| `ApprovalDependencyMissing` | `cryptography` is missing (also an `ImportError`) |

In dev mode, failing to open the socket raises the usual `OSError` / `TimeoutError`.

### Blocking API

For scripts that don't want asyncio, `phonetopc.sync` runs the same client on a background thread:

```python
import phonetopc.sync
from phonetopc import ui

with phonetopc.sync.connect() as phone:  # same arguments as phonetopc.connect
    print(phone.get("battery").value)
    if phone.request_approval("Print gyro readings?"):
        print("approved")
    phone.notify("Hello from the PC")
    with phone.show_panel("hi", "Hi", ui.button("ok", "OK")) as panel:  # closed on exit
        for ev in panel:  # a SyncPanel: update(), clear_canvas(), close() block too
            break
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

The tests never touch your real `devices.json` (each test gets an empty `XDG_CONFIG_HOME`) and never use real mDNS, except `test_real_mdns`: it runs when `zeroconf` is installed and multicast works, and `PHONETOPC_TEST_MDNS=1` turns "multicast doesn't work here" from a skip into a failure. Secure-mode tests use the test-vector certificates in [`spec/conformance/vectors/`](../../spec/conformance/vectors), and pins are checked against `vectors.json`. Approval verification is checked against `approval.json` (the valid signature and every tampered variant), and the fake phone signs approvals with a software key. Tests that need `cryptography` (in the `dev` extra) are skipped without it.

The tests validate every request the client sends against [`spec/schemas/phonetopc-1.schema.json`](../../spec/schemas/phonetopc-1.schema.json), and replay any transcripts in `spec/conformance/*.jsonl` through the client's message handling.
