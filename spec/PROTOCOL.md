# phonetopc protocol — version `phonetopc/1` (draft 1)

This document is normative. The key words MUST, SHOULD and MAY are used as in RFC 2119.
JSON Schemas for every message live in [`schemas/`](schemas/).

Draft 1 covers milestone M1 from [`plan.md`](../plan.md): plaintext dev mode, discovery by manual address,
sensor events. Pairing, TLS, tokens and commands are added in later drafts without breaking these messages.

**Changes in draft 1** (clarifications found while building the Go mock, the Android app and the Python client):
`subscribe` result gains `mode`; ordering guarantee between a `subscribe` response and its events; `on_change`
subscriptions start with the current value; `seq` and `min_delta` semantics made precise; client notifications
are ignored; `get` documents `type`; `unsubscribe` result and per-method results added to the schema.

## 1. Transport

- The phone daemon is the **server**. Clients connect to it.
- Endpoint: `ws://<phone-ip>:<port>/v1/ws`. Default port: **8765**.
- Draft 1 is **plaintext dev mode only**: no TLS, no authentication. The phone app MUST make dev mode an explicit, off-by-default setting.
- Each WebSocket text frame carries exactly one UTF-8 JSON object. Batching multiple messages into one frame (JSON-RPC batch arrays) is NOT supported; a server MUST answer a batch array with error `-32600`.
- Binary frames are reserved. Servers MUST close the connection with code 1003 on receiving one.
- The PC daemon `phonetopcd` exposes the **same protocol** to local clients (Unix socket / localhost WS) in a later milestone.

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

## 4. Methods

### 4.1 `hello`

```jsonc
// request
{"jsonrpc":"2.0","id":1,"method":"hello","params":{
  "protocol":"phonetopc/1",
  "client":{"name":"my-script","version":"0.1.0"}}}
// response
{"jsonrpc":"2.0","id":1,"result":{
  "protocol":"phonetopc/1",
  "server":{"name":"phonetopc-android","version":"0.1.0"},
  "device":{"id":"a1b2c3d4","name":"Galaxy S24","manufacturer":"samsung","model":"SM-S921B","platform":"android","platform_version":"34"},
  "capabilities":["events","get"]}}
```

- If `protocol` is not supported, the server returns error `-32004` (`unsupported_protocol`) with `data.supported` listing the versions it does support.
- `device.id` is stable across restarts and reinstalls where the platform allows (a random ID generated on first run and stored).
- `capabilities` lists optional feature groups. This draft defines `events` and `get`. Later drafts add `commands`, `biometric`, etc.

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
- **Initial value:** for an `on_change` source the server SHOULD send the current value as the first event (seq 0) right after the response, if one is known, so clients don't wait for a change to learn the state. Event-like sources with no state (`ui`) send nothing until something happens.

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
| `game_rotation_vector` | `orientation` | `quat` | `[x, y, z, w]` unit quaternion, no magnetometer | `""` | continuous |
| `proximity` | `distance` | `number` | distance; many phones report only `0` or max | `cm` | on_change |
| `light` | `illuminance` | `number` | ambient light | `lx` | on_change |
| `battery` | `state` | `object` | `{"level": 0–100, "charging": bool}`; `charging` is true while plugged in and charging or full | `%` | on_change |
| `ui` | `tile_tap` | `bool` | always `true`; fired by the Quick Settings tile | `""` | on_change |

Axes follow the [Android sensor coordinate system](https://developer.android.com/develop/sensors-and-location/sensors/sensors_overview#sensors-coords) (device frame, not rotated with screen orientation).

New sources and types MAY be added in any draft. Clients MUST tolerate unknown sources in `list_sensors`.

## 7. Errors

Standard JSON-RPC codes apply (`-32700` parse error, `-32600` invalid request, `-32601` method not found, `-32602` invalid params, `-32603` internal error). Application codes:

| Code | Name | When |
|---|---|---|
| `-32001` | `unknown_source` | `source` not in the catalog |
| `-32002` | `source_unavailable` | Known source, but absent or disabled on this device |
| `-32003` | `hello_required` | Request sent before a successful `hello` |
| `-32004` | `unsupported_protocol` | `hello.protocol` not supported; `data.supported` lists versions |
| `-32005` | `unknown_subscription` | `unsubscribe` with an id not in this session |
| `-32006` | `no_value` | `get` has nothing to return yet |

Every error has `message` (human-readable) and SHOULD have `data.name`: the application names above, or `parse_error`, `invalid_request`, `method_not_found`, `invalid_params`, `internal_error` for the standard codes.

## 8. Discovery

This draft uses a manual address. Servers SHOULD additionally advertise `_phonetopc._tcp` via mDNS with TXT records `id=<device.id>`, `proto=1`, `dev=1` (dev mode). Clients MUST NOT rely on mDNS being available.

## 9. Known gaps (later drafts)

- A server can't yet tell a client that a subscribed source became unavailable (e.g. the user disabled it in the app). Today the subscription just stops producing events.
- Pairing, TLS, tokens, `command` and biometric challenge-response (see `plan.md` §4).
