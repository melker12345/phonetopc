# phonetopc local API (`/v1`, draft 1)

`phonetopcd` keeps one secure connection to the paired phone and offers it to programs on the same PC as a plain
HTTP API. Programs need no pairing, TLS pinning, tokens from `devices.json` or JSON-RPC: a GET streams events, a
POST shows something on the phone. Any language's standard HTTP client is enough.

```
phone ──wss:// + pinned key + token──► phonetopcd ──plain HTTP on this PC──► your program
          (PROTOCOL.md, SECURITY.md)                (this document)
```

Programs may also speak the phone protocol (`PROTOCOL.md`) directly; the daemon is the easy path.

## 1. Where it listens

| Listener | Default | Who can use it |
|---|---|---|
| Unix socket | `$XDG_RUNTIME_DIR/phonetopc.sock` (mode 0600) | This user, no token |
| TCP | `127.0.0.1:8767` (loopback only) | Requests with the token, see below |

**Token.** Other users and programs on the PC can reach `127.0.0.1`, and this API can ask for fingerprint
approvals, so TCP requests need the token in `$XDG_RUNTIME_DIR/phonetopc.token` (mode 0600, kept across restarts
of the daemon):

```
Authorization: Bearer <token>
```

Browsers' `EventSource` and `WebSocket` can't set headers; they may pass `?token=<token>` instead. If a request
has an `Authorization` header, only the header is checked. Requests whose
`Host` is not `localhost`, `127.0.0.1` or `[::1]` are refused with 403 (DNS rebinding). CORS preflights are
answered, so a page that was given the token can use the API.

Without `$XDG_RUNTIME_DIR` the files go to `$TMPDIR/phonetopc-<uid>/`. `phonetopcd --socket`, `--listen` and
`--token-file` change them; `off` turns a listener off.

## 2. Conventions

- Request and response bodies are JSON (`Content-Type: application/json`). Unknown members in a request body are
  an error, so typos don't go unnoticed.
- Success is 200 with a JSON object (`{}` when there is nothing to say).
- Errors have a status and a body
  `{"error": {"code": "<name>", "message": "<what happened and what to do>", "rpc_code": <phone's code>}}`.
  `rpc_code` is present only when the phone refused (`PROTOCOL.md` §7). A phone error not listed below is 502,
  with the phone's error name as `code` (for example `internal_error`), or `phone_error` if it has none.

| Status | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Bad JSON, unknown member, invalid value (checked before asking the phone) |
| 400 | `invalid_params` | The phone found a parameter invalid |
| 401 | `unauthorized` | TCP without the right token |
| 403 | `forbidden` | TCP with a `Host` that isn't loopback |
| 403 | `not_allowed` | The phone's user hasn't allowed notifications or panels for this PC |
| 404 | `not_found` | No such endpoint |
| 404 | `unknown_source`, `unknown_panel` | |
| 405 | `method_not_allowed` | The endpoint exists, with other methods (`Allow` header) |
| 409 | `source_unavailable`, `no_value`, `too_many_panels` | |
| 409 | `approval_key_changed` | See §5 |
| 409 | `approval_unavailable` | The phone is in developer mode, which has no approvals |
| 429 | `rate_limited` | Too many notifications |
| 500 | `internal_error` | A bug in the daemon |
| 501 | `method_not_found` | The phone app is older than the daemon and lacks the method |
| 502 | `approval_invalid` | The phone said approved but the signature didn't verify. **Not an approval** |
| 502 | `phone_error` | Any other failure talking to the phone |
| 503 | `phone_unavailable` | The phone isn't connected right now; `message` says why |
| 503 | `unauthorized` | The phone revoked this PC (`rpc_code` -32010); pair again with `phonetopc pair` |
| 504 | `no_answer` | No answer to an approval in time |

## 3. Status and sensors

### `GET /v1/status`

Always 200, also while the phone is away.

```json
{"connected": true, "since": 1760000000123,
 "device": {"id": "635f3a01db1859bc", "name": "Galaxy S24", "manufacturer": "samsung", "model": "SM-S921B", "platform": "android", "platform_version": "34"},
 "capabilities": ["events", "get", "approval", "notify", "panels"],
 "streams": 2, "panels": ["deck"]}
```

While disconnected, `connected` is `false`, `error` says why (for example *this PC was revoked on the phone — run
`phonetopc pair`*), and `device` and `capabilities` are absent. `since` is when it last connected or disconnected.
The daemon reconnects by itself, at most 10 s after the phone is back.

### `GET /v1/sensors`

The phone's `list_sensors` result: `{"sensors": [{"source": "gyro", "types": ["rotation"], "unit": "rad/s", …}, …]}`
(`PROTOCOL.md` §4.2, §6).

### `GET /v1/sensors/{source}`

The latest reading (`PROTOCOL.md` §4.5). `?type=` picks a type other than the source's first.

```json
{"source": "battery", "type": "state", "value": {"level": 82, "charging": true}, "unit": "%",
 "ts": 1760000000123, "ts_mono_ns": 81234567890123, "device": "635f3a01db1859bc"}
```

## 4. Event streams: `GET /v1/events`

```
GET /v1/events?source=gyro&rate=100
GET /v1/events?source=ui,nfc
GET /v1/events?source=panel&source=notification
```

| Query | Meaning |
|---|---|
| `source` | Required. One or more sources, comma-separated or repeated |
| `type` | Only this event type |
| `rate` | Requested rate in Hz for continuous sensors (> 0) |
| `min_delta` | Suppress events that changed by less than this (≥ 0) |

`type`, `rate` and `min_delta` apply to every source of the stream; open several streams for different settings.

**Formats**, chosen by the request:

| Request | Response |
|---|---|
| `Upgrade: websocket` | A WebSocket; one event per text message |
| `Accept: text/event-stream` (what `EventSource` sends) | Server-Sent Events: `data: <event>` per event, and a `: ping` comment every 15 s |
| Anything else (`curl`, `fetch`, …) | `application/x-ndjson`: one event per line |

**Events** are the phone's events (`PROTOCOL.md` §5) without the `subscription` member:

```json
{"seq": 1042, "source": "gyro", "type": "rotation", "value": [0.01, -0.32, 0.05], "unit": "rad/s",
 "ts": 1760000000123, "ts_mono_ns": 81234567890123, "device": "635f3a01db1859bc"}
```

`seq` counts per source, starting at 0; a gap means events were dropped. Streams with the same source and options
share one subscription on the phone, so they see the same `seq`s.

**Daemon events.** The daemon adds events of source `phonetopcd` to every stream:

| `type` | `value` | When |
|---|---|---|
| `disconnected` | `{"error": "…"}` | The phone connection was lost, or wasn't there when the stream opened |
| `connected` | the `device` object | The phone is back. Its subscriptions are made again, so `seq` starts at 0 again |
| `error` | `{"error": "…"}` | The stream ends right after this, e.g. because a source can't be subscribed again after a reconnect |

**Lifetime.** A stream survives the phone going away and coming back. It can be opened while the phone is away:
it starts with `disconnected` and gets events once the phone connects. Errors that are known at once (unknown
source, bad query) are an HTTP error instead of a stream. A stream ends when the client closes it, after an
`error` event, or when the daemon stops.

**Backpressure.** Each stream queues up to 1024 events. A stream that falls behind loses events of continuous
sources (seen as `seq` gaps); rather than lose an event of an `on_change` source (taps, panel input, …) the
daemon ends the stream with an `error` event.

## 5. Fingerprint approval: `POST /v1/approvals`

```json
{"reason": "Deploy to production?", "timeout_ms": 30000}
```

`reason` is shown on the phone and signed exactly as given (`APPROVAL.md` §4: 1–200 characters, no leading or
trailing whitespace, no control characters). `timeout_ms` is optional: default 30000, 5000 to 120000. The
request waits for the user, then answers 200:

```json
{"approved": true, "why": "approved"}
{"approved": false, "why": "denied"}
```

`why` is `approved`, or one of `denied`, `cancelled`, `timeout`, `busy` (another approval is pending), `lockout`,
`unavailable`. `approved` is `true` only after the daemon verified the phone's signature over a fresh nonce, the
reason and both ids with the trusted approval key. Errors: 409 `approval_key_changed` when the phone's approval key
changed (typically fingerprints were added or removed; accept it with `phonetopc approval-key --accept`; nothing
was sent to the phone), and 502 `approval_invalid` for a signature that didn't verify.

## 6. Notifications

### `POST /v1/notifications`

The body is the phone's `notify` params (`PANELS.md` §3.1); the answer has the notification's id.

```json
{"title": "Deploy?", "body": "main → production", "id": "deploy",
 "actions": [{"id": "yes", "label": "Deploy"}, {"id": "no", "label": "Later"}], "priority": "high"}
→ {"id": "deploy"}
```

What the user does arrives on `GET /v1/events?source=notification` (open it first): `type` `action` (with
`value.action`), `tap` or `dismiss`, and `value.notification` the id (`PANELS.md` §3.2).

### `DELETE /v1/notifications/{id}`

Removes it. `{}`, also when it's gone already.

## 7. Panels

A panel is a screen of widgets on the phone (`PANELS.md` §4, §5). Its input arrives on
`GET /v1/events?source=panel`; **open that stream before showing the panel**, since input without a listener is
lost (canvas strokes excepted).

| Request | Body | Answer |
|---|---|---|
| `PUT /v1/panels/{id}` | `{"title", "layout", "keep_awake"?, "orientation"?}` | `{"panel", "visible"}`. Shows it, or replaces the panel with that id |
| `PATCH /v1/panels/{id}` | `{"updates": [{"widget": "status", "set": {"text": "done", "tone": "good"}}]}` | `{}`. All or nothing (`PANELS.md` §4.5) |
| `DELETE /v1/panels/{id}` | | `{}` |
| `GET /v1/panels` | | `{"panels": [{"panel", "title", "attached", "visible"}]}` |
| `POST /v1/panels/{id}/canvases/{widget}/clear` | | `{}` |

```json
PUT /v1/panels/deck
{"title": "Stream deck", "keep_awake": true,
 "layout": {"type": "grid", "cols": 2, "children": [
   {"type": "button", "id": "mute", "label": "Mute"},
   {"type": "slider", "id": "vol", "label": "Volume", "min": 0, "max": 100, "value": 40}]}}
```

The panels belong to the daemon's connection, so every stream with `source=panel` sees the input of every panel
shown through the daemon. When the phone connection drops and comes back, the daemon takes its panels over again
(`panel_attach`), and the phone replays the canvas strokes drawn meanwhile. A panel the user closes, or one
removed with `DELETE`, is forgotten.

## 8. Examples

Shell, over the socket:

```sh
S=$XDG_RUNTIME_DIR/phonetopc.sock
curl -s --unix-socket $S http://phonetopc/v1/sensors/battery
curl -sN --unix-socket $S 'http://phonetopc/v1/events?source=gyro&rate=20'
curl -s --unix-socket $S http://phonetopc/v1/notifications -d '{"title":"Build done"}'
curl -s --unix-socket $S http://phonetopc/v1/approvals -d '{"reason":"Deploy?"}' | grep -q '"approved":true' && ./deploy.sh

# lock the PC when the Quick Settings tile is tapped
curl -sN --unix-socket $S 'http://phonetopc/v1/events?source=ui' |
  while read -r ev; do case $ev in *tile_tap*) loginctl lock-session;; esac; done
```

Python, standard library only:

```python
import json, os, urllib.request

TOKEN = open(os.environ["XDG_RUNTIME_DIR"] + "/phonetopc.token").read().strip()
req = urllib.request.Request("http://127.0.0.1:8767/v1/events?source=gyro&rate=20",
                             headers={"Authorization": "Bearer " + TOKEN})
for line in urllib.request.urlopen(req):
    ev = json.loads(line)
    print(ev["source"], ev["value"])
```

JavaScript (Node 22+, Deno, Bun):

```js
import fs from "node:fs";
const token = (await fs.promises.readFile(`${process.env.XDG_RUNTIME_DIR}/phonetopc.token`, "utf8")).trim();
const headers = { Authorization: `Bearer ${token}` };
await fetch("http://127.0.0.1:8767/v1/notifications", {
  method: "POST", headers, body: JSON.stringify({ title: "Build done" }),
});
const ws = new WebSocket(`ws://127.0.0.1:8767/v1/events?source=ui&token=${token}`);
ws.onmessage = (m) => console.log(JSON.parse(m.data));
```

Go, standard library only:

```go
tok, _ := os.ReadFile(os.Getenv("XDG_RUNTIME_DIR") + "/phonetopc.token")
req, _ := http.NewRequest("GET", "http://127.0.0.1:8767/v1/events?source=gyro&rate=20", nil)
req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
res, err := http.DefaultClient.Do(req)
if err != nil { log.Fatal(err) }
sc := bufio.NewScanner(res.Body)
for sc.Scan() {
	fmt.Println(sc.Text()) // one JSON event per line
}
```
