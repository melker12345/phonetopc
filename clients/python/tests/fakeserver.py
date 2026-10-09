"""In-process fake phonetopc/1 server for tests (follows spec/PROTOCOL.md and SECURITY.md).

In dev mode (the default) it serves plaintext ``ws://``. With ``secure=True``
it serves ``wss://`` with the test-vector phone certificate from
spec/conformance/vectors/ (or the PC one, to provoke a pin mismatch), checks
``hello.auth.token`` against ``tokens``, answers ``-32010`` and closes with 1008
on a bad token, and reports ``pc`` in the ``hello`` result (SECURITY.md 5).

Every client message is validated against spec/schemas/phonetopc-1.schema.json
(as a request) and every server message the fake sends is validated too, so the
fake itself can't drift from the spec. A response's ``result`` is additionally
checked against ``$defs/resultByMethod/properties/<method>``, the method being
looked up from the request with the same id. Violations are collected in
``schema_errors`` and checked by the ``server`` fixture at teardown.
"""

from __future__ import annotations

import asyncio
import contextlib
import copy
import hmac
import itertools
import json
import math
import ssl
import time
from pathlib import Path
from typing import Any

import jsonschema
from websockets.asyncio.server import Server, ServerConnection, serve

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMA_PATH = REPO_ROOT / "spec" / "schemas" / "phonetopc-1.schema.json"
SCHEMA: dict[str, Any] = json.loads(SCHEMA_PATH.read_text())
VECTORS_DIR = REPO_ROOT / "spec" / "conformance" / "vectors"
VECTORS: dict[str, Any] = json.loads((VECTORS_DIR / "vectors.json").read_text())
#: Pins of the test-vector certificates, by name ("phone", "pc").
PINS = {v["cert"].removesuffix("-cert.pem"): v["pin"] for v in VECTORS["pin"]}

#: The token and PC of the default pairing of a secure FakeServer.
TOKEN = "dGVzdC10b2tlbi10ZXN0LXRva2VuLXRlc3QtdG9rZW4"
PC = {"id": "9d1c0ffee0ddba11", "name": "pytest-pc"}

_validator_cls = jsonschema.validators.validator_for(SCHEMA)
#: Validates any phonetopc/1 message.
MESSAGE_VALIDATOR = _validator_cls(SCHEMA)
#: Validates a message that must be a client request specifically.
REQUEST_VALIDATOR = _validator_cls({**SCHEMA, "oneOf": [{"$ref": "#/$defs/request"}]})
#: Validates the ``result`` of a response, by the method of the request it answers.
RESULT_VALIDATORS = {
    method: _validator_cls(
        {
            **{k: v for k, v in SCHEMA.items() if k != "oneOf"},
            "$ref": f"#/$defs/resultByMethod/properties/{method}",
        }
    )
    for method in SCHEMA["$defs"]["resultByMethod"]["properties"]
}

DEVICE = {
    "id": "a1b2c3d4",
    "name": "Fake Phone",
    "manufacturer": "acme",
    "model": "FP-1",
    "platform": "android",
    "platform_version": "34",
}

# fmt: off
SENSORS: list[dict[str, Any]] = [
    {"source": "accelerometer", "types": ["motion"], "unit": "m/s^2", "shape": "vec3",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 500, "default_rate_hz": 50,
     "available": True},
    {"source": "gyro", "types": ["rotation"], "unit": "rad/s", "shape": "vec3",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 1000, "default_rate_hz": 50,
     "available": True},
    {"source": "game_rotation_vector", "types": ["orientation"], "unit": "", "shape": "quat",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 200, "default_rate_hz": 50,
     "available": True},
    {"source": "proximity", "types": ["distance"], "unit": "cm", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "light", "types": ["illuminance"], "unit": "lx", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "battery", "types": ["state"], "unit": "%", "shape": "object",
     "mode": "on_change", "available": True},
    {"source": "ui", "types": ["tile_tap"], "unit": "", "shape": "bool",
     "mode": "on_change", "available": True},
    # Not in the spec's catalog: clients must tolerate it.
    {"source": "barometer", "types": ["pressure"], "unit": "hPa", "shape": "number",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 25, "default_rate_hz": 5,
     "available": True},
]
# fmt: on

ERRORS = {
    "unknown_source": -32001,
    "source_unavailable": -32002,
    "hello_required": -32003,
    "unsupported_protocol": -32004,
    "unknown_subscription": -32005,
    "no_value": -32006,
    "invalid_params": -32602,
    "method_not_found": -32601,
    "invalid_request": -32600,
    "parse_error": -32700,
    "unauthorized": -32010,
}


def sample_value(source: str, n: int) -> Any:
    if source in ("gyro", "accelerometer"):
        return [n * 0.01, -n * 0.02, 0.5]
    if source == "game_rotation_vector":
        return [0.0, 0.0, 0.0, 1.0]
    if source == "battery":
        return {"level": 82, "charging": True}
    if source == "ui":
        return True
    return float(n)


def _is_num(x: Any) -> bool:
    return isinstance(x, (int, float)) and not isinstance(x, bool)


def _distance(a: Any, b: Any) -> float | None:
    """min_delta distance, or None where min_delta doesn't apply (bool, object)."""
    if _is_num(a) and _is_num(b):
        return float(abs(a - b))
    vectors = isinstance(a, list) and isinstance(b, list) and len(a) == len(b)
    if vectors and all(map(_is_num, a + b)):
        return math.dist(a, b)
    return None


#: Required and optional string params, per method.
_STR_PARAMS: dict[str, tuple[tuple[str, ...], tuple[str, ...]]] = {
    "hello": (("protocol",), ()),
    "subscribe": (("source",), ("type",)),
    "unsubscribe": (("subscription",), ()),
    "get": (("source",), ("type",)),
}


def _params_problem(method: str, params: Any) -> str | None:
    """Why ``params`` are invalid (-32602), or None. Unknown params are ignored."""
    if not isinstance(params, dict):
        return "params must be an object"
    required, optional = _STR_PARAMS.get(method, ((), ()))
    for key in required:
        if not isinstance(params.get(key), str):
            return f"{key} must be a string"
    for key in optional:
        if key in params and not isinstance(params[key], str):
            return f"{key} must be a string"
    if method == "subscribe":
        if "rate_hz" in params and not (_is_num(params["rate_hz"]) and params["rate_hz"] > 0):
            return "rate_hz must be a number > 0"
        if "min_delta" in params and not (
            _is_num(params["min_delta"]) and params["min_delta"] >= 0
        ):
            return "min_delta must be a number >= 0"
    return None


def _reject_constant(name: str) -> Any:
    raise ValueError(f"{name} is not strict JSON")


def _id_key(req_id: Any) -> tuple[str, Any]:
    # 1 and "1" are different ids.
    return (type(req_id).__name__, req_id)


#: Largest magnitude of an integer request id (2^53 - 1, PROTOCOL.md section 2).
MAX_SAFE_ID = 2**53 - 1


def _valid_id(req_id: Any) -> bool:
    """PROTOCOL.md section 2: a string or an integer within +-(2^53 - 1).

    Not a bool, float (``1.0``, ``1e3``) or null.
    """
    if isinstance(req_id, str):
        return True
    return isinstance(req_id, int) and not isinstance(req_id, bool) and abs(req_id) <= MAX_SAFE_ID


class Session:
    def __init__(self, server: FakeServer, ws: ServerConnection) -> None:
        self.server = server
        self.ws = ws
        self.hello_done = False
        #: Token this session authenticated with (secure mode).
        self.token: str | None = None
        self.subs: dict[str, dict[str, Any]] = {}
        #: Method of each request still awaiting its response, by id.
        self.methods: dict[tuple[str, Any], str] = {}
        self.tasks: set[asyncio.Task[Any]] = set()

    def spawn(self, coro: Any) -> None:
        task = asyncio.ensure_future(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)


class FakeServer:
    def __init__(self, *, secure: bool = False, cert: str = "phone") -> None:
        #: Serve wss:// with the test-vector certificate ``cert`` ("phone" or "pc").
        self.secure = secure
        self.cert = cert
        #: Accepted tokens and the PC each one belongs to (secure mode).
        self.tokens: dict[str, dict[str, str]] = {TOKEN: dict(PC)}
        #: Paths of the HTTP upgrade requests received (to prove a client sent nothing).
        self.http_requests: list[str] = []
        self.supported_protocols = ["phonetopc/1"]
        #: Catalog sources reported as available: false (-> source_unavailable).
        self.unavailable: set[str] = {"game_rotation_vector"}
        #: Seconds to wait before answering, per method.
        self.delays: dict[str, float] = {}
        #: Seconds to wait before answering, per "get" source (overrides delays).
        self.get_delays: dict[str, float] = {}
        #: Values for "get"; missing on_change sources answer no_value.
        self.values: dict[str, Any] = {"battery": {"level": 82, "charging": True}}
        #: Send the first event(s) of a new subscription *before* its response
        #: (off-spec since draft 1, which requires the response first).
        self.events_before_response = 0
        #: seq numbers that are counted but not sent (simulated server drops).
        self.skip_seqs: set[int] = set()
        #: Stream continuous sensors automatically at their rate.
        self.auto_stream = True
        #: Forget subscriptions on unsubscribe (-> unknown_subscription).
        self.forget_subscriptions = False
        #: Send the current value (from ``values``) as seq 0 when subscribing to
        #: an on_change source, as PROTOCOL.md 4.3 recommends.
        self.initial_values = True
        #: Leave "mode" out of subscribe results, like a draft 0 server.
        #: (Off-spec for draft 1, so the schema check must be relaxed.)
        self.omit_mode = False
        #: Close the connection instead of answering these methods.
        self.close_on: set[str] = set()

        self.received: list[dict[str, Any]] = []
        self.sent: list[dict[str, Any]] = []
        self.schema_errors: list[str] = []
        self.sessions: list[Session] = []
        self._sub_ids = itertools.count(1)
        self._server: Server | None = None
        self.port = 0

    # -- lifecycle --------------------------------------------------------

    async def start(self) -> None:
        ctx = None
        if self.secure:
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            ctx.load_cert_chain(
                VECTORS_DIR / f"{self.cert}-cert.pem", VECTORS_DIR / f"{self.cert}-key.pem"
            )
        self._server = await serve(
            self._handler, "127.0.0.1", 0, ssl=ctx, process_request=self._on_http
        )
        sock = next(iter(self._server.sockets))
        self.port = sock.getsockname()[1]

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()

    @property
    def addr(self) -> str:
        return f"127.0.0.1:{self.port}"

    @property
    def pin(self) -> str:
        return PINS[self.cert]

    def _on_http(self, connection: ServerConnection, request: Any) -> None:
        self.http_requests.append(request.path)

    async def revoke(self, token: str) -> None:
        """Forget ``token`` and close its sessions with 1008, like the phone app does."""
        self.tokens.pop(token, None)
        for session in self.sessions:
            if session.token == token:
                await session.ws.close(1008, "revoked")

    # -- helpers for tests ------------------------------------------------

    def requests(self, method: str) -> list[dict[str, Any]]:
        return [m for m in self.received if isinstance(m, dict) and m.get("method") == method]

    async def wait_for_request(self, method: str, count: int = 1, timeout: float = 2.0) -> None:
        deadline = time.monotonic() + timeout
        while len(self.requests(method)) < count:
            if time.monotonic() > deadline:
                raise AssertionError(f"server never got {count} x {method!r}")
            await asyncio.sleep(0.005)

    async def emit(self, source: str, value: Any = None, count: int = 1) -> None:
        """Push ``count`` events to every subscription of ``source``."""
        for session in self.sessions:
            for sub_id, sub in list(session.subs.items()):
                if sub["source"] == source:
                    for _ in range(count):
                        v = value if value is not None else sample_value(source, sub["seq"])
                        await self._send_event(session, sub_id, sub, v)

    async def send_raw(self, data: str | bytes) -> None:
        for session in self.sessions:
            await session.ws.send(data)

    async def drop_connections(self, code: int = 1011, reason: str = "") -> None:
        for session in self.sessions:
            await session.ws.close(code, reason)

    # -- protocol ---------------------------------------------------------

    async def _send(self, session: Session, msg: Any) -> None:
        for err in MESSAGE_VALIDATOR.iter_errors(msg):
            self.schema_errors.append(f"server sent invalid message {msg}: {err.message}")
        if isinstance(msg, dict) and "id" in msg:
            # Responses don't name their method: find it via the request id.
            method = session.methods.pop(_id_key(msg["id"]), None)
            validator = RESULT_VALIDATORS.get(method) if method else None
            if validator is not None and "result" in msg:
                for err in validator.iter_errors(msg["result"]):
                    self.schema_errors.append(
                        f"server sent invalid {method} result {msg['result']}: {err.message}"
                    )
        self.sent.append(msg)
        await session.ws.send(json.dumps(msg))

    async def _result(self, session: Session, req_id: Any, result: Any) -> None:
        await self._send(session, {"jsonrpc": "2.0", "id": req_id, "result": result})

    async def _error(
        self, session: Session, req_id: Any, name: str, message: str, **data: Any
    ) -> None:
        err = {"code": ERRORS[name], "message": message, "data": {"name": name, **data}}
        await self._send(session, {"jsonrpc": "2.0", "id": req_id, "error": err})

    def _event_params(self, sub_id: str, sub: dict[str, Any], value: Any) -> dict[str, Any]:
        info = next(s for s in SENSORS if s["source"] == sub["source"])
        return {
            "subscription": sub_id,
            "seq": sub["seq"],
            "source": sub["source"],
            "type": info["types"][0],
            "value": value,
            "unit": info["unit"],
            "ts": int(time.time() * 1000),
            "ts_mono_ns": time.monotonic_ns(),
            "device": DEVICE["id"],
        }

    async def _send_event(
        self, session: Session, sub_id: str, sub: dict[str, Any], value: Any
    ) -> None:
        # min_delta filters before seq is assigned (PROTOCOL.md section 5), and
        # only applies to number / vec3 / quat values (section 4.3).
        if "last" in sub and sub.get("min_delta"):
            d = _distance(value, sub["last"])
            if d is not None and d < sub["min_delta"]:
                return
        sub["last"] = value
        params = self._event_params(sub_id, sub, value)
        sub["seq"] += 1
        if params["seq"] in self.skip_seqs:  # dropped "for backpressure": a seq gap
            return
        await self._send(session, {"jsonrpc": "2.0", "method": "event", "params": params})

    async def _stream(self, session: Session, sub_id: str, sub: dict[str, Any]) -> None:
        period = 1.0 / sub["rate_hz"]
        with contextlib.suppress(Exception):
            while sub_id in session.subs:
                value = sample_value(sub["source"], sub["seq"])
                await self._send_event(session, sub_id, sub, value)
                await asyncio.sleep(period)

    async def _handler(self, ws: ServerConnection) -> None:
        session = Session(self, ws)
        self.sessions.append(session)
        try:
            async for raw in ws:
                if isinstance(raw, bytes):
                    await ws.close(1003, "binary frames are reserved")
                    return
                try:
                    msg = json.loads(raw, parse_constant=_reject_constant)
                except ValueError:
                    await self._error(session, None, "parse_error", "parse error")
                    continue
                self.received.append(msg)
                for err in REQUEST_VALIDATOR.iter_errors(msg):
                    self.schema_errors.append(f"client sent invalid request {msg}: {err.message}")
                if not isinstance(msg, dict):
                    await self._error(session, None, "invalid_request", "not a JSON object")
                    continue
                # The order of checks of PROTOCOL.md section 2 from here on.
                if "id" not in msg:
                    continue  # client notification: ignored, never answered
                if not _valid_id(msg["id"]):
                    await self._error(session, None, "invalid_request", "invalid id")
                    continue
                if msg.get("jsonrpc") != "2.0" or not isinstance(msg.get("method"), str):
                    await self._error(session, msg["id"], "invalid_request", "invalid request")
                    continue
                session.methods[_id_key(msg["id"])] = msg["method"]
                session.spawn(self._dispatch(session, msg))
        except Exception:
            pass
        finally:
            session.subs.clear()
            for task in list(session.tasks):
                task.cancel()

    async def _dispatch(self, session: Session, msg: dict[str, Any]) -> None:
        method = msg.get("method")
        req_id = msg.get("id")
        params = msg.get("params")
        if params is None:  # absent, or an explicit null (PROTOCOL.md section 2)
            params = {}
        if method in self.close_on:
            await session.ws.close(1011, "simulated crash")
            return
        delay = self.delays.get(str(method), 0.0)
        if method == "get" and isinstance(params, dict):
            delay = self.get_delays.get(params.get("source"), delay)
        if delay:
            await asyncio.sleep(delay)

        # Hello gating comes first, even for unknown methods (section 3).
        if method != "hello" and not session.hello_done:
            await self._error(session, req_id, "hello_required", "say hello first")
            return
        if method not in RESULT_VALIDATORS:  # i.e. not one of the spec's methods
            await self._error(session, req_id, "method_not_found", f"no method {method!r}")
            return
        problem = _params_problem(method, params)
        if problem:
            await self._error(session, req_id, "invalid_params", problem)
            return
        assert isinstance(params, dict)

        if method == "hello":
            if params.get("protocol") not in self.supported_protocols:
                await self._error(
                    session,
                    req_id,
                    "unsupported_protocol",
                    "unsupported protocol",
                    supported=self.supported_protocols,
                )
                return
            result: dict[str, Any] = {
                "protocol": "phonetopc/1",
                "server": {"name": "fake", "version": "0.0.1"},
                "device": DEVICE,
                "capabilities": ["events", "get"],
            }
            if self.secure:  # SECURITY.md section 5; auth is ignored in dev mode
                auth = params.get("auth")
                token = auth.get("token") if isinstance(auth, dict) else None
                pc = self._check_token(token)
                if pc is None:
                    await self._error(session, req_id, "unauthorized", "unknown or revoked token")
                    await session.ws.close(1008, "unauthorized")
                    return
                session.token = token
                result["pc"] = pc
            session.hello_done = True
            await self._result(session, req_id, result)
            return
        if method == "list_sensors":
            sensors = copy.deepcopy(SENSORS)
            for s in sensors:
                s["available"] = s["source"] not in self.unavailable
            await self._result(session, req_id, {"sensors": sensors})
        elif method == "subscribe":
            await self._subscribe(session, req_id, params)
        elif method == "unsubscribe":
            sub_id = params.get("subscription")
            if sub_id not in session.subs or self.forget_subscriptions:
                await self._error(session, req_id, "unknown_subscription", "no such subscription")
                return
            del session.subs[sub_id]
            await self._result(session, req_id, {})
        elif method == "get":
            await self._get(session, req_id, params)

    def _check_token(self, token: Any) -> dict[str, str] | None:
        if not isinstance(token, str):
            return None
        found = None
        for known, pc in self.tokens.items():  # constant time per token, no early exit
            if hmac.compare_digest(known.encode(), token.encode()):
                found = pc
        return found

    def _sensor(self, source: Any) -> dict[str, Any] | None:
        return next((s for s in SENSORS if s["source"] == source), None)

    async def _check_source(self, session: Session, req_id: Any, params: dict[str, Any]) -> bool:
        """Catalog checks (step 8): unknown source, then unknown type, then availability."""
        source = params["source"]
        info = self._sensor(source)
        if info is None:
            await self._error(session, req_id, "unknown_source", f"unknown source {source!r}")
            return False
        if "type" in params and params["type"] not in info["types"]:
            await self._error(session, req_id, "invalid_params", "unknown type")
            return False
        if source in self.unavailable:
            await self._error(session, req_id, "source_unavailable", f"{source} unavailable")
            return False
        return True

    async def _subscribe(self, session: Session, req_id: Any, params: dict[str, Any]) -> None:
        if not await self._check_source(session, req_id, params):
            return
        source = params["source"]
        info = self._sensor(source)
        assert info is not None
        sub_id = f"s-{next(self._sub_ids)}"
        result: dict[str, Any] = {
            "subscription": sub_id,
            "source": source,
            "types": [params.get("type", info["types"][0])],
            "mode": info["mode"],
        }
        if self.omit_mode:
            del result["mode"]
        sub: dict[str, Any] = {"source": source, "seq": 0, "min_delta": params.get("min_delta")}
        if info["mode"] == "continuous":
            rate = params.get("rate_hz", info["default_rate_hz"])
            rate = min(max(rate, info["min_rate_hz"]), info["max_rate_hz"])
            result["rate_hz"] = rate
            sub["rate_hz"] = rate
        # rate_hz is ignored for on_change sources, and left out of the result.
        for _ in range(self.events_before_response):
            await self._send_event(session, sub_id, sub, sample_value(source, sub["seq"]))
        # Registered before the response so an unsubscribe can't race it. No
        # event can be sent for it until the response is out: streaming and
        # the initial value start below, and tests emit() only once the
        # client has the response (the response must come first, 4.3).
        session.subs[sub_id] = sub
        await self._result(session, req_id, result)
        if info["mode"] == "continuous":
            if self.auto_stream:
                session.spawn(self._stream(session, sub_id, sub))
        elif self.initial_values and source in self.values:
            # The current value of a stateful on_change source comes as seq 0.
            await self._send_event(session, sub_id, sub, self.values[source])

    async def _get(self, session: Session, req_id: Any, params: dict[str, Any]) -> None:
        if not await self._check_source(session, req_id, params):
            return
        source = params["source"]
        info = self._sensor(source)
        assert info is not None
        if info["mode"] == "continuous":
            value = sample_value(source, 0)
        elif source in self.values:
            value = self.values[source]
        else:
            await self._error(session, req_id, "no_value", f"no {source} value yet")
            return
        await self._result(
            session,
            req_id,
            {
                "source": source,
                "type": params.get("type", info["types"][0]),
                "value": value,
                "unit": info["unit"],
                "ts": int(time.time() * 1000),
                "ts_mono_ns": time.monotonic_ns(),
                "device": DEVICE["id"],
            },
        )


def device_entry(server: FakeServer | None = None, **fields: Any) -> dict[str, Any]:
    """A devices.json entry for ``server`` (the fake phone), as `phonetopc pair` writes it."""
    entry: dict[str, Any] = {
        "id": DEVICE["id"],
        "name": "Fake Phone",
        "pin": PINS["phone"],
        "token": TOKEN,
        "port": server.port if server else 8765,
        "addresses": ["127.0.0.1"],
        "paired_at": "2026-10-09T16:40:00Z",
    }
    entry.update(fields)
    return entry
