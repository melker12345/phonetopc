# phonetopc protocol — version `phonetopc/1` (draft 4)

This document is normative. The key words MUST, SHOULD and MAY are used as in RFC 2119.

This document covers sessions, methods, events and sensors. [`SECURITY.md`](SECURITY.md) covers pairing, TLS and
tokens. [`APPROVAL.md`](APPROVAL.md) covers fingerprint approval (`request_approval`).
[`PANELS.md`](PANELS.md) covers notifications and panels, PC → phone output (`notify`, `panel_show`, …).
The four documents share one draft number: this is draft 4 of all of them.

The prose of these documents is the normative reference. The JSON Schema
[`schemas/phonetopc-1.schema.json`](schemas/phonetopc-1.schema.json) describes the session messages (every
request, response and `event` on `/v1/ws`), not the pairing messages (`pair_*`, `SECURITY.md` §4). It and the cases
in [`conformance/`](conformance/) follow the prose; where they disagree with it, the prose wins and the schema or
case is a bug.

**Changes in draft 4:** notifications and panels (`notify`, `notify_cancel`, `panel_show`, `panel_attach`,
`panel_update`, `panel_close`, `panel_list`, `canvas_clear`), capabilities `notify` and `panels`, sources
`notification` and `panel`, errors `-32030`–`-32033`; see `PANELS.md`.

**Changes in draft 3:** `request_approval`, capability `approval` and `hello.approval`; see `APPROVAL.md`.
Sources `rotation_vector`, `magnetometer`, `compass`, `barometer`, `location` and `nfc` (§6); `proximity` clarified.
Resource limits (§3). Pairing and secure sessions made precise; see the draft 3 notes in `SECURITY.md`.

**Changes in draft 2:** secure mode (`wss://` with a pinned certificate) is the default; `hello` gains `auth`
and its result gains `pc`; new error `-32010`; mDNS TXT gains `tls=1`. See `SECURITY.md`.

**Changes in draft 1** (clarifications found while building the Go mock, the Android app and the Python client):
`subscribe` result gains `mode`; ordering guarantee between a `subscribe` response and its events; `on_change`
subscriptions start with the current value; `seq` and `min_delta` semantics made precise; client notifications
are ignored; `get` documents `type`; `unsubscribe` result and per-method results added to the schema.

## 1. Transport

- The phone daemon is the **server**. Clients connect to it.
- Endpoint: `wss://<phone-ip>:<port>/v1/ws` in **secure mode** (the default), or `ws://…` in **dev mode**. Default port: **8765**. See `SECURITY.md` §3.
- Dev mode has no TLS and no authentication. The phone app MUST make dev mode an explicit, off-by-default setting.
- Each WebSocket text frame carries exactly one UTF-8 JSON object. Batching multiple messages into one frame (JSON-RPC batch arrays) is NOT supported; a server MUST answer a batch array with error `-32600`.
- Binary frames are reserved. Servers MUST close the connection with code 1003 on receiving one.

## 2. Message format: JSON-RPC 2.0

All messages follow [JSON-RPC 2.0](https://www.jsonrpc.org/specification).

- **Requests** (client → server) have `jsonrpc`, `id`, `method`, and optional `params` (object).
- **Responses** (server → client) have `jsonrpc`, `id`, and exactly one of `result` or `error`.
- **Notifications** (server → client) have `jsonrpc`, `method`, `params`, and no `id`. This draft defines one: `event`.
- A JSON object from the client **without** an `id` member is a JSON-RPC notification, even if it is otherwise malformed. This draft defines no client notifications, so servers MUST ignore them and MUST NOT reply. (A client that wants an answer sends an `id`.)
- `params` MUST be an object if present. An explicit `"params": null` is treated as absent. Any other type gets `-32602`.
- Frames that are not strict JSON (e.g. bare words, trailing garbage) get `-32700` with `"id": null`. A JSON value that isn't an object gets `-32600` with `"id": null`.
- **Order of checks.** A server processes each frame in this order and answers with the first error that applies: (1) not strict JSON → `-32700`; (2) not a JSON object (array, scalar) → `-32600`; (3) no `id` member → ignore silently; (4) invalid `id`, `jsonrpc` ≠ `"2.0"` or `method` not a string → `-32600`; (5) no successful `hello` yet and `method` ≠ `hello` → `-32003`; (6) unknown method → `-32601`; (7) invalid `params` → `-32602`; (8) method-specific errors.
  - Errors from step 4 echo the request's `id` when the `id` itself is valid; only an invalid `id` is answered with `"id": null`.
  - `"method": ""` is a string, so it passes step 4 and fails at step 5 or 6.
  - Step 7 covers everything checkable without the sensor catalog: required members, types, `rate_hz > 0`, `min_delta ≥ 0`. So `{"source":"nope","rate_hz":0}` gets `-32602`.
  - Within step 8: unknown source (`-32001`), then a `type` the source doesn't have (`-32602`), then unavailable source (`-32002`), then `-32005` / `-32006`.
  - [`conformance/server-cases.json`](conformance/server-cases.json) pins all of this down case by case.
- Clients MUST ignore unknown fields. Servers MUST ignore unknown fields in `params`.
- `id` MUST be an integer that fits in a signed 53-bit range (so every JSON library represents it exactly) or a string. Servers SHOULD reject other forms (fractions, exponents, out-of-range integers, `null`) with `-32600`. Rejecting `null` is a deliberate tightening of JSON-RPC 2.0, which merely discourages it. Responses can arrive interleaved with events; clients correlate by `id`.

## 3. Session lifecycle

1. Client opens the WebSocket.
2. Client MUST send `hello` as its first request. Any other request before a successful `hello` gets error `-32003` (`hello_required`), including unknown methods. Sending `hello` again later is allowed and has no effect on subscriptions; a failed `hello` does not undo an earlier successful one.
3. Client calls `list_sensors`, `subscribe`, `get`, `unsubscribe` as needed.
4. Closing the socket ends the session. All subscriptions of that session are dropped.

Servers SHOULD send WebSocket pings every 15 s and close sessions that don't answer within 30 s. Clients SHOULD reply to pings (most libraries do automatically).

**Resource limits.** A server MAY close with 1008 a connection that sends more than 16 frames before a successful `hello`, or that has more than 256 responses it hasn't been able to send (a client that writes but never reads). After asking to close, a server ignores further frames and MAY drop a connection that hasn't taken its close frame within about 1 s.

## 4. Methods

### 4.1 `hello`

```jsonc
// request
{"jsonrpc":"2.0","id":1,"method":"hello","params":{
  "protocol":"phonetopc/1",
  "client":{"name":"my-script","version":"0.1.0"},
  "auth":{"token":"…"}}}            // secure mode only, see SECURITY.md §5
// response
{"jsonrpc":"2.0","id":1,"result":{
  "protocol":"phonetopc/1",
  "server":{"name":"phonetopc-android","version":"0.1.0"},
  "device":{"id":"a1b2c3d4","name":"Galaxy S24","manufacturer":"samsung","model":"SM-S921B","platform":"android","platform_version":"34"},
  "capabilities":["events","get"],
  "pc":{"id":"9d1c…","name":"arch-desktop"}}}   // secure mode only
```

- If `protocol` is not supported, the server returns error `-32004` (`unsupported_protocol`) with `data.supported` listing the versions it does support.
- `device.id` is stable across restarts and reinstalls where the platform allows (a random ID generated on first run and stored).
- `capabilities` lists optional feature groups. This draft defines `events`, `get`, `approval` (`APPROVAL.md`), `notify` and `panels` (`PANELS.md`).

### 4.2 `list_sensors`

```jsonc
{"jsonrpc":"2.0","id":2,"method":"list_sensors"}
{"jsonrpc":"2.0","id":2,"result":{"sensors":[
  {"source":"gyro","types":["rotation"],"unit":"rad/s","shape":"vec3",
   "mode":"continuous","min_rate_hz":1,"max_rate_hz":200,"default_rate_hz":50,"available":true},
  {"source":"proximity","types":["distance"],"unit":"cm","shape":"number",
   "mode":"on_change","available":true}
]}}
```

- `mode` is `continuous` (sampled at a rate) or `on_change` (emitted when the value changes).
- `min_rate_hz`, `max_rate_hz`, `default_rate_hz` are present only for `continuous` sensors.
- `available: false` means the source is known but this device lacks it, or the user disabled it in the app.

### 4.3 `subscribe`

```jsonc
{"jsonrpc":"2.0","id":3,"method":"subscribe","params":{"source":"gyro","type":"rotation","rate_hz":100}}
{"jsonrpc":"2.0","id":3,"result":{"subscription":"s-7f3a","source":"gyro","types":["rotation"],"mode":"continuous","rate_hz":100}}
```

| Param | Required | Meaning |
|---|---|---|
| `source` | yes | Sensor source name (§6) |
| `type` | no | Restrict to one event type. Omitted means all types of the source |
| `rate_hz` | no | Must be > 0 for any source (else `-32602`). Continuous sensors: defaults to `default_rate_hz`, clamped to `[min_rate_hz, max_rate_hz]`, and the result reports the effective rate. `on_change` sensors: ignored, and the result has no `rate_hz` |
| `min_delta` | no | Must be ≥ 0. Suppress events whose value differs from the last *sent* value by less than this: absolute difference for `number`, Euclidean distance for `vec3`/`quat`. Ignored for `bool` and `object` values |

The result has `subscription`, `source`, `types` (the types this subscription delivers), `mode` (copied from `list_sensors`), and `rate_hz` for continuous sensors.

- **Ordering:** the server MUST send the `subscribe` response before the first `event` of that subscription. Clients MAY discard events for subscription ids they don't know.
- **Initial value:** for an `on_change` source the server SHOULD send the current value as the first event (seq 0) right after the response, if one is known, so clients don't wait for a change to learn the state. Event-like sources with no state (`ui`, `nfc`, `notification`, `panel`) send nothing until something happens.

- The server MAY deliver events at a slightly different rate than requested; Android treats rates as hints.
- A session MAY hold several subscriptions to the same source. Each gets its own `subscription` id and `seq` counter.
- Errors: `-32001` unknown source, `-32002` source unavailable, `-32602` invalid params (e.g. unknown `type`).

### 4.4 `unsubscribe`

```jsonc
{"jsonrpc":"2.0","id":4,"method":"unsubscribe","params":{"subscription":"s-7f3a"}}
{"jsonrpc":"2.0","id":4,"result":{}}
```

Error `-32005` if the subscription doesn't exist in this session. Events for the subscription MAY still arrive between the request and the response; clients SHOULD discard them. After the response (success or `-32005`), no more events with that id are sent.

### 4.5 `get`

One-shot read of the latest value. Params: `source` (required) and `type` (optional; defaults to the source's first type).

```jsonc
{"jsonrpc":"2.0","id":5,"method":"get","params":{"source":"battery"}}
{"jsonrpc":"2.0","id":5,"result":{"source":"battery","type":"state",
  "value":{"level":82,"charging":true},"unit":"%","ts":1760000000123,"ts_mono_ns":81234567890123,"device":"a1b2c3d4"}}
```

`result` is a reading: an event object without `subscription` and `seq`, with the timestamps of when that value was measured. For a sensor with no value yet the server MAY briefly start the sensor to get one, or return error `-32006` (`no_value`).

## 5. Events

```jsonc
{"jsonrpc":"2.0","method":"event","params":{
  "subscription":"s-7f3a","seq":1042,
  "source":"gyro","type":"rotation","value":[0.01,-0.32,0.05],"unit":"rad/s",
  "ts":1760000000123,"ts_mono_ns":81234567890123,"device":"a1b2c3d4"}}
```

| Field | Type | Meaning |
|---|---|---|
| `subscription` | string | Subscription that produced this event |
| `seq` | integer | Starts at 0 per subscription. Assigned to every event that passes the subscription's own filters (rate downsampling, `type`, `min_delta`, and for `on_change` sources suppression of readings that repeat or predate the last sent value); filtered-out readings don't consume a number. Events then dropped for backpressure keep their number, so **a gap means events were dropped** |
| `source`, `type` | string | See §6 |
| `value` | per §6 | |
| `unit` | string | See §6. `""` when unitless |
| `ts` | integer | Wall-clock Unix time in milliseconds when the reading was taken |
| `ts_mono_ns` | integer | Monotonic nanoseconds (Android `SensorEvent.timestamp`, i.e. `elapsedRealtimeNanos`). Only differences between values from the same device are meaningful |
| `device` | string | `device.id` from `hello` |

**Backpressure.** If a client can't keep up, the server MAY drop events of `continuous` sources (visible as `seq` gaps). It MUST NOT drop events of `on_change` sources; if their queue overflows it closes the connection with code 1008. Servers SHOULD queue the two kinds separately so a backlog of continuous events can't trigger that close.

## 6. Sensor catalog

| `source` | `type` | `shape` | `value` | `unit` | `mode` |
|---|---|---|---|---|---|
| `accelerometer` | `motion` | `vec3` | `[x, y, z]` including gravity | `m/s^2` | continuous |
| `gyro` | `rotation` | `vec3` | `[x, y, z]` angular speed | `rad/s` | continuous |
| `game_rotation_vector` | `orientation` | `quat` | `[x, y, z, w]` unit quaternion, no magnetometer: relative to an arbitrary heading that drifts slowly, but undisturbed by magnets and steel | `""` | continuous |
| `rotation_vector` | `orientation` | `quat` | `[x, y, z, w]` unit quaternion in the earth frame (x east, y magnetic north, z up); uses the magnetometer, so it has an absolute heading but is disturbed near magnets and steel | `""` | continuous |
| `magnetometer` | `magnetic_field` | `vec3` | `[x, y, z]` calibrated magnetic field | `uT` | continuous |
| `compass` | `heading` | `number` | azimuth in `[0, 360)`: the direction the top edge of the phone (its +y axis) points, clockwise from magnetic north, meant for a phone lying flat; derived from `rotation_vector` | `deg` | continuous |
| `barometer` | `pressure` | `number` | air pressure | `hPa` | continuous |
| `proximity` | `distance` | `number` | the earpiece sensor that turns the screen off during calls: an object within a few cm of the top of the screen (range ~5 cm), many phones report only `0` (near) or their max (far). It is **not** the distance between phone and PC | `cm` | on_change |
| `light` | `illuminance` | `number` | ambient light | `lx` | on_change |
| `battery` | `state` | `object` | `{"level": 0–100, "charging": bool}`; `charging` is true while plugged in and charging or full | `%` | on_change |
| `location` | `position` | `object` | a position fix, see below | `""` | on_change |
| `ui` | `tile_tap` | `bool` | always `true`; fired by the Quick Settings tile | `""` | on_change |
| `nfc` | `tag` | `object` | a scanned NFC tag, see below | `""` | on_change |
| `notification` | `action`, `tap`, `dismiss` | `object` | the user acted on a notification this PC posted; `PANELS.md` §3.2 | `""` | on_change |
| `panel` | `state`, `press`, `change`, `submit`, `stroke` | `object` | input on a panel this session owns; `PANELS.md` §6 | `""` | on_change |

Axes follow the [Android sensor coordinate system](https://developer.android.com/develop/sensors-and-location/sensors/sensors_overview#sensors-coords) (device frame, not rotated with screen orientation).

**`location`** is a state like `battery`: a subscription starts with the current fix if one is known. Its value is

```jsonc
{"lat": 59.3293, "lon": 18.0686, "accuracy_m": 12.0,   // degrees (WGS 84); horizontal accuracy radius, 68% confidence
 "altitude_m": 31.5, "speed_mps": 0.4, "bearing_deg": 271.0}   // optional
```

`altitude_m` (above the WGS 84 ellipsoid), `speed_mps` (over ground) and `bearing_deg` (direction of travel, clockwise from
true north, `[0, 360)`) are omitted when the fix doesn't have them. A phone reports `location` as unavailable while
the user hasn't granted location access to the app or has turned location off. Expect about one fix per second
while subscribed.

**`nfc`** is event-like, like `ui`: nothing is sent until a tag is scanned, and each scan is one event, also when
the same tag is scanned again. Its value is

```jsonc
{"id": "04a2246a8e5d80",            // tag UID, lowercase hex; may be empty or random per scan on some tags
 "tech": ["NfcA", "MifareUltralight", "Ndef"],   // technologies the tag supports (Android class names)
 "ndef": [                           // optional: the tag's NDEF records, if it has an NDEF message
   {"tnf": 1, "type": "U", "uri": "https://example.com/", "payload": "046578616d706c652e636f6d2f"},
   {"tnf": 1, "type": "T", "text": "hello", "payload": "02656e68656c6c6f"}]}
```

Each NDEF record has `tnf` (the 3-bit Type Name Format, 0–7), `type` (the record type as UTF-8 text, or lowercase
hex if it isn't printable UTF-8; `""` when empty) and `payload` (lowercase hex). Well-known text records (`tnf` 1,
`type` `"T"`) also have `text`, the decoded text without the language code; well-known URI records (`tnf` 1,
`type` `"U"`) and absolute-URI records (`tnf` 3) also have `uri`. A server MAY bound what it reports: the phone app
sends at most 8 records and at most 4096 bytes of each payload (decoding `text`/`uri` from what it kept).
Phones scan while their screen is on and unlocked; `available` only says that the phone has NFC, which may be
switched off in its system settings. Tag contents are not authenticated: anyone can write a tag or clone a UID, and on
Android other apps on the phone can hand the app a made-up tag, so treat `nfc` values as untrusted input.

`notification` and `panel` are available only while the user allows notifications or panels for this PC
(`PANELS.md` §1), and their events go only to sessions of that PC.

New sources and types MAY be added in any draft. Clients MUST tolerate unknown sources in `list_sensors`.

## 7. Errors

This section lists every error code of the spec; other documents explain theirs but defer to this list.
Standard JSON-RPC codes apply (`-32700` parse error, `-32600` invalid request, `-32601` method not found, `-32602` invalid params, `-32603` internal error). Application codes on `/v1/ws`:

| Code | Name | When |
|---|---|---|
| `-32001` | `unknown_source` | `source` not in the catalog |
| `-32002` | `source_unavailable` | Known source, but absent or disabled on this device |
| `-32003` | `hello_required` | Request sent before a successful `hello` |
| `-32004` | `unsupported_protocol` | `hello.protocol` not supported; `data.supported` lists versions |
| `-32005` | `unknown_subscription` | `unsubscribe` with an id not in this session |
| `-32006` | `no_value` | `get` has nothing to return yet |
| `-32010` | `unauthorized` | Secure mode: missing, unknown or revoked token (`SECURITY.md` §5) |
| `-32030` | `not_allowed` | Notifications or panels not allowed for this PC (`PANELS.md` §9) |
| `-32031` | `unknown_panel` | No panel with that id (`PANELS.md` §9) |
| `-32032` | `too_many_panels` | The PC already has 8 panels (`PANELS.md` §9) |
| `-32033` | `rate_limited` | Too many notifications (`PANELS.md` §9) |

`request_approval` adds no codes: its user-level outcomes are results (`APPROVAL.md` §4.1, §6).

Pairing (`SECURITY.md` §4) runs on its own connection, `/v1/pair` on the PC, with protocol `phonetopc-pair/1`. It
uses the standard codes, `-32004` as above, and these codes, which occur only there:

| Code | Name | When |
|---|---|---|
| `-32020` | `bad_secret` | `pair_qr` secret doesn't match (`SECURITY.md` §4.2) |
| `-32021` | `rejected` | The PC user declined (`SECURITY.md` §4.2–§4.4) |
| `-32022` | `out_of_order` | A pairing method sent at the wrong step (`SECURITY.md` §4.1) |
| `-32023` | `sas_mismatch` | A user said the codes differ, or the commitment check failed (`SECURITY.md` §4.3) |
| `-32024` | `expired` | The pairing listener timed out (`SECURITY.md` §4.1) |

Every error has `message` (human-readable) and SHOULD have `data.name`: the application names above, or `parse_error`, `invalid_request`, `method_not_found`, `invalid_params`, `internal_error` for the standard codes.

## 8. Discovery

This draft uses a manual address. Servers SHOULD additionally advertise `_phonetopc._tcp` via mDNS with TXT records `id=<device.id>`, `proto=1`, and `tls=1` (secure mode) or `dev=1` (dev mode). Clients MUST NOT rely on mDNS being available.

## 9. Known gaps (later drafts)

- A server can't yet tell a client that a subscribed source became unavailable (e.g. the user disabled it in the app). Today the subscription just stops producing events.
- `command` (vibrate, …) for PC → phone actions other than approval, notifications and panels.
- A PC daemon, `phonetopcd`, that keeps one connection to the phone and offers this same protocol to local
  programs (Unix socket or localhost WebSocket). Until then, every program connects to the phone itself.
