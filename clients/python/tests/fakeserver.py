"""In-process fake phonetopc/1 server for tests (follows spec/PROTOCOL.md draft 0).

Every client message is validated against spec/schemas/phonetopc-1.schema.json
(as a request) and every server message the fake sends is validated too, so the
fake itself can't drift from the spec. Violations are collected in
``schema_errors`` and checked by the ``server`` fixture at teardown.
"""

from __future__ import annotations

import asyncio
import contextlib
import copy
import itertools
import json
import time
from pathlib import Path
from typing import Any

import jsonschema
from websockets.asyncio.server import Server, ServerConnection, serve

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMA_PATH = REPO_ROOT / "spec" / "schemas" / "phonetopc-1.schema.json"
SCHEMA: dict[str, Any] = json.loads(SCHEMA_PATH.read_text())

_validator_cls = jsonschema.validators.validator_for(SCHEMA)
#: Validates any phonetopc/1 message.
MESSAGE_VALIDATOR = _validator_cls(SCHEMA)
#: Validates a message that must be a client request specifically.
REQUEST_VALIDATOR = _validator_cls({**SCHEMA, "oneOf": [{"$ref": "#/$defs/request"}]})

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
     "available": False},
    {"source": "proximity", "types": ["distance"], "unit": "cm", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "light", "types": ["illuminance"], "unit": "lx", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "battery", "types": ["state"], "unit": "%", "shape": "object",
     "mode": "on_change", "available": True},
    {"source": "ui", "types": ["tile_tap"], "unit": "", "shape": "bool",
     "mode": "on_change", "available": True},
    # Not in the draft-0 catalog: clients must tolerate it.
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


class Session:
    def __init__(self, server: FakeServer, ws: ServerConnection) -> None:
        self.server = server
        self.ws = ws
        self.hello_done = False
        self.subs: dict[str, dict[str, Any]] = {}
        self.tasks: set[asyncio.Task[Any]] = set()

    def spawn(self, coro: Any) -> None:
        task = asyncio.ensure_future(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)


class FakeServer:
    def __init__(self) -> None:
        self.supported_protocols = ["phonetopc/1"]
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
        self._server = await serve(self._handler, "127.0.0.1", 0)
        sock = next(iter(self._server.sockets))
        self.port = sock.getsockname()[1]

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()

    @property
    def addr(self) -> str:
        return f"127.0.0.1:{self.port}"

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

    async def _send(self, session: Session, msg: Any, *, validate: bool = True) -> None:
        if validate:
            for err in MESSAGE_VALIDATOR.iter_errors(msg):
                self.schema_errors.append(f"server sent invalid message {msg}: {err.message}")
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
        params = self._event_params(sub_id, sub, value)
        sub["seq"] += 1
        if params["seq"] in self.skip_seqs:
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
                    msg = json.loads(raw)
                except ValueError:
                    await self._send(
                        session,
                        {
                            "jsonrpc": "2.0",
                            "id": None,
                            "error": {"code": -32700, "message": "parse error"},
                        },
                    )
                    continue
                self.received.append(msg)
                for err in REQUEST_VALIDATOR.iter_errors(msg):
                    self.schema_errors.append(f"client sent invalid request {msg}: {err.message}")
                if isinstance(msg, list):
                    await self._error(session, None, "invalid_request", "batches not supported")
                    continue
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
        params = msg.get("params") or {}
        if method in self.close_on:
            await session.ws.close(1011, "simulated crash")
            return
        delay = self.delays.get(str(method), 0.0)
        if method == "get":
            delay = self.get_delays.get(params.get("source"), delay)
        if delay:
            await asyncio.sleep(delay)

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
            session.hello_done = True
            await self._result(
                session,
                req_id,
                {
                    "protocol": "phonetopc/1",
                    "server": {"name": "fake", "version": "0.0.1"},
                    "device": DEVICE,
                    "capabilities": ["events", "get"],
                },
            )
            return
        if not session.hello_done:
            await self._error(session, req_id, "hello_required", "say hello first")
            return
        if method == "list_sensors":
            await self._result(session, req_id, {"sensors": copy.deepcopy(SENSORS)})
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
        else:
            await self._error(session, req_id, "method_not_found", f"no method {method!r}")

    def _sensor(self, source: Any) -> dict[str, Any] | None:
        return next((s for s in SENSORS if s["source"] == source), None)

    async def _check_source(self, session: Session, req_id: Any, source: Any) -> bool:
        info = self._sensor(source)
        if info is None:
            await self._error(session, req_id, "unknown_source", f"unknown source {source!r}")
            return False
        if not info["available"]:
            await self._error(session, req_id, "source_unavailable", f"{source} unavailable")
            return False
        return True

    async def _subscribe(self, session: Session, req_id: Any, params: dict[str, Any]) -> None:
        source = params.get("source")
        if not await self._check_source(session, req_id, source):
            return
        info = self._sensor(source)
        assert info is not None
        if "type" in params and params["type"] not in info["types"]:
            await self._error(session, req_id, "invalid_params", "unknown type")
            return
        sub_id = f"s-{next(self._sub_ids)}"
        result: dict[str, Any] = {
            "subscription": sub_id,
            "source": source,
            "types": [params.get("type", info["types"][0])],
            "mode": info["mode"],
        }
        if self.omit_mode:
            del result["mode"]
        sub: dict[str, Any] = {"source": source, "seq": 0}
        if info["mode"] == "continuous":
            rate = params.get("rate_hz", info["default_rate_hz"])
            rate = min(max(rate, info["min_rate_hz"]), info["max_rate_hz"])
            result["rate_hz"] = rate
            sub["rate_hz"] = rate
        session.subs[sub_id] = sub
        for _ in range(self.events_before_response):
            await self._send_event(session, sub_id, sub, sample_value(source, sub["seq"]))
        await self._result(session, req_id, result)
        if info["mode"] == "continuous" and self.auto_stream:
            session.spawn(self._stream(session, sub_id, sub))

    async def _get(self, session: Session, req_id: Any, params: dict[str, Any]) -> None:
        source = params.get("source")
        if not await self._check_source(session, req_id, source):
            return
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
