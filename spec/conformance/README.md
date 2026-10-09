# Conformance material

Conformance material for `phonetopc/1` (draft 2) lives here.
[`../PROTOCOL.md`](../PROTOCOL.md) is still the normative reference.

- [`server-cases.json`](server-cases.json): a language-neutral suite of single-request cases that
  every **server** implementation runs in its own tests. It pins down how a server answers
  malformed, out-of-order and invalid requests (the order of checks in §2).
- [`secure-server-cases.json`](secure-server-cases.json): the same kind of suite for **secure
  mode** ([`../SECURITY.md`](../SECURITY.md) §5): authenticated `hello`, `-32010` followed by close
  1008, and the hello timeout. See [Secure server cases](#secure-server-cases-secure-server-casesjson).
- [`vectors/`](vectors/): test vectors for pins, base64url, the SAS commitment and the SAS code
  (`vectors.json`), with the test certificates and keys they refer to.
- `*.jsonl` **transcripts**: recorded sessions with the mock phone (`pcd/cmd/phonetopc-mock`),
  for checking a client or server against concrete wire traffic.

## Server cases (`server-cases.json`)

The file is a JSON array of cases:

```json
{
  "name": "hello_required_before_bad_params",
  "description": "Step 5 before step 7: ...",
  "hello": false,
  "setup": [ {"send": ..., "expect": ...} ],
  "send": {"jsonrpc": "2.0", "id": 1, "method": "subscribe", "params": ["gyro"]},
  "expect": {"error": -32003, "name": "hello_required", "id": 1}
}
```

| Member | Meaning |
|---|---|
| `name` | Unique identifier (snake_case), used as the test name |
| `description` | What the case checks, usually naming the §2 check step(s) involved |
| `hello` | If true, the runner first completes a successful handshake (see below) |
| `setup` | Optional. Steps (`{"send", "expect"}`, same rules as below) run in order before `send` |
| `send` | The frame to send (see below) |
| `expect` | What the server must do in answer to `send` (see below) |

**`send`** is one of:

- `{"raw": "<text>"}`: send `<text>` verbatim as a text frame. Used for anything that isn't strict
  JSON, and for JSON whose exact spelling matters (`1.0`, `1e3`, integers beyond 2^53).
- `{"binary": "<base64>"}`: send the decoded bytes as a binary frame.
- any other JSON value: serialize it (any spelling) and send it as a text frame. An object is
  only treated as `raw`/`binary` if that is its single member.

**`expect`** is one of:

| Form | The server must |
|---|---|
| `{"error": <code>, "name": <data.name>, "id": <id>}` | reply with an error response with `error.code` = `<code>`, `error.data.name` = `<name>` and `id` equal to `<id>` (`null` when the request id was invalid or unknown). An optional `"data": {...}` lists further members `error.data` must have with exactly these values (e.g. `supported`) |
| `{"result": "any", "id": <id>}` | reply with a success response (`result` present, no `error`) with that `id`. The result's content is not checked here; the JSON Schema and the transcripts cover it |
| `{"no_reply": true}` | not reply at all. The runner checks this by sending the probe `{"jsonrpc":"2.0","id":"conformance-probe","method":"list_sensors"}` and requiring that the next response has id `"conformance-probe"` (before hello it is a `-32003` error, which is fine) |
| `{"close": <code>}` | close the WebSocket with that close code, without sending a response first |

**Runner rules.**

- Every case uses a fresh connection.
- With `"hello": true` the runner first sends
  `{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"}}}`
  and requires a success response with that id.
- "The next response" is the next frame with an `id` member; `event` notifications in between are
  skipped (a case may create a subscription).
- Ids are compared as JSON values: `1` and `"1"` differ, and integers must match exactly.
- Error `message` texts are not checked.

**Server fixture.** The cases assume the server under test is configured like this:

- It runs in dev mode (`ws://`, no authentication). Secure mode has its own cases below.
- It supports exactly one protocol version, `phonetopc/1`.
- All catalog sources of §6 are known. `proximity` is unavailable (`available: false`); all
  others are available.
- `battery` has a current value. `ui` has none (no tile tap has happened yet).
- No background activity is needed; cases never wait for events.

**Choices the cases make.** Where §2 leaves a detail open, the cases take the reading most
consistent with the order of checks:

- An `error` at step 4 echoes the request `id` if the `id` itself is valid (e.g. a wrong
  `jsonrpc` with `"id": 3` gets `"id": 3`); only an invalid `id` gets `null`.
- `"method": ""` is a string, so it passes step 4 and is then an unknown method (`-32601`, or
  `-32003` before hello).
- Step 7 covers everything that can be checked without the catalog: params shape, required and
  typed members, `rate_hz > 0`, `min_delta ≥ 0`. Step 8 then checks, in this order: unknown
  `source` (`-32001`), a `type` the source doesn't have (`-32602`), unavailable source (`-32002`),
  then the method's own state (`-32005`, `-32006`).

## Secure server cases (`secure-server-cases.json`)

A JSON object with a `fixture` and a `cases` array. Cases have the format of `server-cases.json`
above, with these differences:

- **Transport.** The runner connects to `wss://…/v1/ws` and verifies that the server's pin equals
  `fixture.pin` (the pin of `vectors/phone-cert.pem`). Every case uses a fresh connection.
- **`"hello": true`** sends the conformance hello with
  `"auth":{"token":"<fixture.pcs[0].token>"}` added and requires a success response.
- **`send`** may also be `{"wait": true}`: send nothing. Used with `{"close": 1008}`: the server
  must close the connection within the hello timeout (plus a few seconds of slack).
- **`expect`** gains two optional members:

| Member | Meaning |
|---|---|
| `"result_has": {...}` | With `"result"`: each listed member of `result` must equal the given JSON value (e.g. `pc`) |
| `"close_after": <code>` | With `"error"`: right after the error response, the server closes the WebSocket with this code and sends nothing else |

**Server fixture.**

- Secure mode, serving the key `fixture.key` (`vectors/phone-key.pem`), so its pin is `fixture.pin`.
  The certificate itself may be any self-signed one for that key.
- Exactly the PCs in `fixture.pcs` are paired, with those ids, names and tokens. The tokens in
  `fixture.revoked_tokens` belonged to PCs that have been revoked: they must not be accepted.
- The hello timeout is `fixture.hello_timeout_s` (10 s, the SHOULD of §5). A runner MAY configure a
  shorter one to keep the suite fast; the `no_hello_timeout` case only needs *some* timeout.
- The per-address limit on failed hellos (a SHOULD of §5) must be off, or allow at least 20 failures
  per minute: the cases fail many hellos from one address.
- Otherwise as for `server-cases.json` (catalog, availability, values).

**Choices the cases make.** §5 doesn't say where the token check sits in the order of checks of
`PROTOCOL.md` §2. The cases take this order for `hello` in secure mode:

1. Steps 1–6 as usual; before a successful `hello` other requests still get `-32003`, without
   closing.
2. Step 7, the params shape: params must be an object and `protocol` a present string, else `-32602`
   (no close).
3. Then authentication: `auth` absent, `null`, not an object, without `token`, with a `token` that is
   not a non-empty string, or with an unknown or revoked token: `-32010`, then close 1008.
4. Then the protocol version: `-32004` with `data.supported` (no close).

So an unauthenticated client learns nothing but `-32010`. SECURITY.md draft 3 settled this order
and adds: any failing `hello` after a successful one (`-32010`, `-32602` or `-32004`) closes the
connection with 1008 after the error response (`rehello_*` cases).

## Transcripts

### Format

Each `*.jsonl` file holds one JSON object per line, in wire order:

```json
{"dir":"c2s","msg":{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1"}}}
{"dir":"s2c","msg":{"jsonrpc":"2.0","id":1,"result":{"protocol":"phonetopc/1","...":"..."}}}
```

- `dir` is `"c2s"` (client to server) or `"s2c"` (server to client).
- `msg` is the JSON content of one WebSocket text frame.

Some frames can't be written as `msg`. They use one of these members instead:

| Member | Meaning |
|---|---|
| `"raw": "<text>"` | A text frame that is not valid JSON, so it can't be embedded as `msg` (used for the parse-error case) |
| `"binary": "<base64>"` | A binary frame |
| `"close": {"code": 1003, "reason": "..."}` | A WebSocket close frame |

A session ends when the client closes the socket normally (1000). That final close is not recorded.

### What varies between runs

The following members are not stable from run to run. Don't compare them literally:

- `ts` and `ts_mono_ns` (wall clock and monotonic time).
- `value` of readings (synthetic sensor data).
- The number of `event` notifications. For continuous sources this depends on timing, so a few more
  events may appear, for example between an `unsubscribe` request and its response. The `seq`
  numbers still start at 0 and increase by 1.

Everything else is deterministic and should match: ids, methods, results, error codes, `data.name`
and the order of requests and responses. Error `message` texts are human-readable and only
informative.

### Files

| File | Covers |
|---|---|
| `happy_path.jsonl` | `hello`, `list_sensors`, `subscribe` to gyro at 10 Hz, three events, `get` battery, `unsubscribe` |
| `on_change.jsonl` | String ids; the current value sent on subscribing to `light`; `ui/tile_tap` events; `get ui` |
| `rate_and_min_delta.jsonl` | `rate_hz` clamping (0.5 → 1, 1000 → 200); a `min_delta` that only lets the first event through |
| `error_hello.jsonl` | `-32003 hello_required`, `-32004 unsupported_protocol` with `data.supported` |
| `error_methods.jsonl` | `-32601`, `-32001`, `-32002` (proximity is unavailable in this session), `-32602`, `-32005`, `-32006` |
| `error_framing.jsonl` | `-32700` parse error, `-32600` for a batch array and a wrong `jsonrpc`; a client notification (no `id`) that gets no reply; close 1003 for a binary frame |

Some `c2s` messages in the error transcripts are deliberately invalid, and they don't validate
against the schema. Examples are an unknown method, `rate_hz: 0`, a batch array, and a
notification without `id`. Every `s2c` message validates against
[`../schemas/phonetopc-1.schema.json`](../schemas/phonetopc-1.schema.json), and every `result`
also validates against `$defs/resultByMethod/properties/<method>` for the method of the request
with the same `id`.

### Optional behaviour the mock shows

Where the spec says SHOULD or MAY, the transcripts show the mock's choice:

- Subscribing to `light`, `proximity` or `battery` sends the current value right away as `seq` 0
  (SHOULD), the same way Android reports on-change sensors when a listener registers. `ui` sends
  nothing until a tap.
- `get` on a continuous source samples it on the spot instead of returning `-32006`. `get ui`
  before any tap returns `-32006`.
- Every error carries `data.name`, including the standard JSON-RPC codes.
- A client message without `id` is ignored even if it would be malformed as a request (wrong
  `jsonrpc`, no `method`): there is no reply at all, not even an error.

### Regenerating

The transcripts are generated by code, never edited by hand. From `pcd/`:

```sh
go test ./internal/mockphone -run TestTranscripts -record   # rewrite the files
go test ./internal/mockphone -run Transcript                # check they still match the mock
```
