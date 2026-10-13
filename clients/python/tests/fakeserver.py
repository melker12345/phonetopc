"""In-process fake phonetopc/1 server for tests (follows spec/PROTOCOL.md and SECURITY.md).

In dev mode (the default) it serves plaintext ``ws://``. With ``secure=True``
it serves ``wss://`` with the test-vector phone certificate from
spec/conformance/vectors/ (or the PC one, to provoke a pin mismatch), checks
``hello.auth.token`` against ``tokens``, answers ``-32010`` and closes with 1008
on a bad token, and reports ``pc`` in the ``hello`` result (SECURITY.md 5).

Notifications and panels (PANELS.md) work in both modes, allowed by default
(``permissions``; the real phone starts with both off). Panels belong to the
PC (``"dev"`` in dev mode): they survive the owner session as detached panels,
and ``panel_attach`` replays their kept strokes. The ``press``, ``change``,
``submit``, ``draw``, ``set_state`` and ``notification_event`` helpers play
the user on the phone. Params are checked with :mod:`phonetopc.panels`, the
client's own rules; panel-server-cases.json pins both down.

Fingerprint approval (APPROVAL.md) works in secure mode after
:meth:`FakeServer.enable_approval`, with a software P-256 key (needs the
``cryptography`` package). ``approval_mode`` picks the outcome, and
``approval_tamper`` / ``approval_result_hook`` let tests forge bad answers.

Every client message is validated against spec/schemas/phonetopc-1.schema.json
(as a request) and every server message the fake sends is validated too, so the
fake itself can't drift from the spec. A response's ``result`` is additionally
checked against ``$defs/resultByMethod/properties/<method>``, the method being
looked up from the request with the same id. Violations are collected in
``schema_errors`` and checked by the ``server`` fixture at teardown.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import contextlib
import copy
import hashlib
import hmac
import itertools
import json
import math
import ssl
import time
import unicodedata
from collections.abc import Callable
from pathlib import Path
from typing import Any

import jsonschema
from websockets.asyncio.server import Server, ServerConnection, serve

from phonetopc.panels import MAX_PANELS, request_problem, update_problem

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
    {"source": "rotation_vector", "types": ["orientation"], "unit": "", "shape": "quat",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 200, "default_rate_hz": 50,
     "available": True},
    {"source": "magnetometer", "types": ["magnetic_field"], "unit": "uT", "shape": "vec3",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 100, "default_rate_hz": 50,
     "available": True},
    {"source": "compass", "types": ["heading"], "unit": "deg", "shape": "number",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 200, "default_rate_hz": 50,
     "available": True},
    {"source": "barometer", "types": ["pressure"], "unit": "hPa", "shape": "number",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 25, "default_rate_hz": 5,
     "available": True},
    {"source": "proximity", "types": ["distance"], "unit": "cm", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "light", "types": ["illuminance"], "unit": "lx", "shape": "number",
     "mode": "on_change", "available": True},
    {"source": "battery", "types": ["state"], "unit": "%", "shape": "object",
     "mode": "on_change", "available": True},
    {"source": "location", "types": ["position"], "unit": "", "shape": "object",
     "mode": "on_change", "available": True},
    {"source": "ui", "types": ["tile_tap"], "unit": "", "shape": "bool",
     "mode": "on_change", "available": True},
    {"source": "nfc", "types": ["tag"], "unit": "", "shape": "object",
     "mode": "on_change", "available": True},
    {"source": "notification", "types": ["action", "tap", "dismiss"], "unit": "",
     "shape": "object", "mode": "on_change", "available": True},
    {"source": "panel", "types": ["state", "press", "change", "submit", "stroke"], "unit": "",
     "shape": "object", "mode": "on_change", "available": True},
    # Not in the spec's catalog: clients must tolerate it.
    {"source": "thermometer", "types": ["temperature"], "unit": "C", "shape": "number",
     "mode": "continuous", "min_rate_hz": 1, "max_rate_hz": 25, "default_rate_hz": 5,
     "available": True},
]

#: A location/position value (PROTOCOL.md 6).
LOCATION = {"lat": 59.32528, "lon": 18.07109, "accuracy_m": 8.0, "altitude_m": 28.0}
#: An nfc/tag value (PROTOCOL.md 6).
TAG = {
    "id": "04a2246a8e5d80",
    "tech": ["NfcA", "MifareUltralight", "Ndef"],
    "ndef": [{"tnf": 1, "type": "T", "text": "hi", "payload": "02656e6869"}],
}
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
    "not_allowed": -32030,
    "unknown_panel": -32031,
    "too_many_panels": -32032,
    "rate_limited": -32033,
}

#: Methods of PANELS.md, and the permission each needs.
PANEL_METHODS = {
    "notify": "notify",
    "notify_cancel": "notify",
    "panel_show": "panels",
    "panel_attach": "panels",
    "panel_update": "panels",
    "panel_close": "panels",
    "panel_list": "panels",
    "canvas_clear": "panels",
}
#: Notifications one PC may have on the phone (PANELS.md 3.1).
MAX_NOTIFICATIONS = 16


def sample_value(source: str, n: int) -> Any:
    if source in ("gyro", "accelerometer", "magnetometer"):
        return [n * 0.01, -n * 0.02, 0.5]
    if source in ("game_rotation_vector", "rotation_vector"):
        return [0.0, 0.0, 0.0, 1.0]
    if source == "compass":
        return float(n % 360)
    if source == "barometer":
        return 1013.25 + n * 0.01
    if source == "location":
        return LOCATION
    if source == "nfc":
        return TAG
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
    if method in PANEL_METHODS:
        return request_problem(method, params)
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
        #: Token this session authenticated with (secure mode), and its PC.
        self.token: str | None = None
        self.pc: dict[str, str] | None = None
        self.subs: dict[str, dict[str, Any]] = {}
        #: Method of each request still awaiting its response, by id.
        self.methods: dict[tuple[str, Any], str] = {}
        self.tasks: set[asyncio.Task[Any]] = set()

    def spawn(self, coro: Any) -> None:
        task = asyncio.ensure_future(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)


class FakePanel:
    """A panel on the fake phone (PANELS.md 4)."""

    def __init__(self, panel: str, title: str, layout: dict[str, Any], owner: Session) -> None:
        self.id = panel
        self.title = title
        self.layout = copy.deepcopy(layout)
        #: Widgets with an id, by id (the dicts inside ``layout``).
        self.widgets: dict[str, dict[str, Any]] = {}
        stack: list[Any] = [self.layout]
        while stack:
            w = stack.pop()
            if isinstance(w.get("id"), str):
                self.widgets[w["id"]] = w
            stack.extend(w.get("children", []))
        #: The session that gets the events; None while detached.
        self.owner: Session | None = owner
        self.visible = False
        self.next_pseq = 0
        #: Stroke event values kept since each canvas's last clear (7.3).
        self.strokes: list[dict[str, Any]] = []
        self.next_stroke: dict[str, int] = {}

    def values(self) -> dict[str, Any]:
        """Current value of every toggle and slider (panel_attach)."""
        out: dict[str, Any] = {}
        for wid, w in self.widgets.items():
            if w["type"] == "toggle":
                out[wid] = w.get("value", False)
            elif w["type"] == "slider":
                lo, hi = w.get("min", 0), w.get("max", 1)
                out[wid] = min(max(w.get("value", lo), lo), hi)
        return out


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
        self.values: dict[str, Any] = {
            "battery": {"level": 82, "charging": True},
            "location": LOCATION,
        }
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

        # Fingerprint approval (APPROVAL.md), secure mode only; see enable_approval().
        #: "approve", "hang" (never answer), or a ``why`` to answer with
        #: ("denied", "cancelled", "timeout", "busy", "lockout", "unavailable").
        #: ``None``: no approval capability.
        self.approval_mode: str | None = None
        #: Seconds the "user" takes before the answer.
        self.approval_delay = 0.0
        #: Changes the message before it is signed (forges a signature over other data).
        self.approval_tamper: Callable[[bytes], bytes] | None = None
        #: Changes the result before it is sent (e.g. drop or corrupt the signature).
        self.approval_result_hook: Callable[[dict[str, Any]], dict[str, Any]] | None = None
        #: Messages signed so far.
        self.approval_signed: list[bytes] = []
        self._approval_key: Any = None
        self._approval_pending = False

        # Notifications and panels (PANELS.md).
        #: What the "user" allowed (for every PC).
        self.permissions = {"notify": True, "panels": True}
        #: Panels by PC (pc id, or "dev") and panel id, in creation order.
        self.panels: dict[str, dict[str, FakePanel]] = {}
        #: Notification params by PC and notification id, oldest first.
        self.notifications: dict[str, dict[str, dict[str, Any]]] = {}
        #: Answer notify with rate_limited after this many notifications.
        self.notify_limit: int | None = None
        self._notify_count = 0
        self._notify_ids = itertools.count(1)

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

    # -- approval -----------------------------------------------------------

    def enable_approval(self, mode: str = "approve") -> None:
        """Give the phone an approval key (a software one; the real phone's needs a fingerprint)."""
        if self._approval_key is None:
            self.rotate_approval_key()
        self.approval_mode = mode

    def rotate_approval_key(self) -> None:
        """A new approval key, as after fingerprint enrollment changed on the phone."""
        from cryptography.hazmat.primitives.asymmetric import ec

        self._approval_key = ec.generate_private_key(ec.SECP256R1())

    @property
    def approval_info(self) -> dict[str, str]:
        """``hello.approval``: the key (base64url DER SPKI) and its pin."""
        from cryptography.hazmat.primitives import serialization

        spki = self._approval_key.public_key().public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
        )
        return {
            "key": _b64url(spki),
            "pin": "sha256/" + _b64url(hashlib.sha256(spki).digest()),
        }

    def _sign(self, message: bytes) -> str:
        from cryptography.hazmat.primitives import hashes
        from cryptography.hazmat.primitives.asymmetric import ec

        if self.approval_tamper is not None:
            message = self.approval_tamper(message)
        self.approval_signed.append(message)
        return _b64url(self._approval_key.sign(message, ec.ECDSA(hashes.SHA256())))

    async def _request_approval(
        self, session: Session, req_id: Any, params: dict[str, Any]
    ) -> None:
        problem = _approval_params_problem(params)
        if problem:
            await self._error(session, req_id, "invalid_params", problem)
            return
        mode = self.approval_mode
        if mode is None or self._approval_key is None:
            await self._result(session, req_id, {"approved": False, "why": "unavailable"})
            return
        if self._approval_pending or mode == "busy":  # one approval at a time per phone
            await self._result(session, req_id, {"approved": False, "why": "busy"})
            return
        self._approval_pending = True
        try:
            if mode == "hang":
                await asyncio.Event().wait()
            if self.approval_delay:
                await asyncio.sleep(self.approval_delay)
            result: dict[str, Any]
            if mode == "approve":
                assert session.pc is not None
                message = "\n".join(
                    [
                        "phonetopc-approval/1",
                        DEVICE["id"],
                        session.pc["id"],
                        params["nonce"],
                        _b64url(hashlib.sha256(params["reason"].encode()).digest()),
                    ]
                ).encode()
                result = {"approved": True, "signature": self._sign(message)}
            else:
                result = {"approved": False, "why": mode}
        finally:
            self._approval_pending = False
        if self.approval_result_hook is not None:
            result = self.approval_result_hook(result)
        await self._result(session, req_id, result)

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

    # -- the user on the phone (PANELS.md) --------------------------------

    def _pc(self, pc: str | None) -> str:
        if pc is not None:
            return pc
        return PC["id"] if self.secure else "dev"

    def panel(self, panel: str, pc: str | None = None) -> FakePanel:
        return self.panels[self._pc(pc)][panel]

    async def panel_event(
        self, panel: str, type: str, pc: str | None = None, **fields: Any
    ) -> dict[str, Any]:
        """Send a ``panel`` event of ``type`` to the panel's owner; returns its value."""
        p = self.panel(panel, pc)
        value = {"panel": p.id, "pseq": p.next_pseq, **fields}
        p.next_pseq += 1
        if type == "stroke":
            p.strokes.append(value)
        if p.owner is not None:
            await self._deliver(p.owner, "panel", type, value)
        return value

    async def press(self, panel: str, widget: str, pc: str | None = None) -> None:
        await self.panel_event(panel, "press", pc, widget=widget)

    async def change(
        self,
        panel: str,
        widget: str,
        value: Any,
        *,
        final: bool | None = None,
        pc: str | None = None,
    ) -> None:
        self.panel(panel, pc).widgets[widget]["value"] = value
        fields: dict[str, Any] = {"widget": widget, "value": value}
        if final is not None:
            fields["final"] = final
        await self.panel_event(panel, "change", pc, **fields)

    async def submit(self, panel: str, widget: str, text: str, pc: str | None = None) -> None:
        await self.panel_event(panel, "submit", pc, widget=widget, value=text)

    async def draw(
        self,
        panel: str,
        widget: str,
        points: list[tuple[float, float]],
        *,
        chunk: int = 2,
        tool: str = "finger",
        size: tuple[float, float] = (360.0, 640.0),
        pc: str | None = None,
    ) -> int:
        """Draw one stroke through ``points`` (start, moves, end); returns its stroke id."""
        p = self.panel(panel, pc)
        stroke = p.next_stroke.get(widget, 0)
        p.next_stroke[widget] = stroke + 1
        pts = [[x, y, 0.5, 8 * i] for i, (x, y) in enumerate(points)]
        chunks = [pts[i : i + chunk] for i in range(0, len(pts), chunk)]
        common = {"widget": widget, "stroke": stroke, "tool": tool}
        await self.panel_event(
            panel, "stroke", pc, **common, phase="start", points=chunks[0], size=list(size)
        )
        for c in chunks[1:]:
            await self.panel_event(panel, "stroke", pc, **common, phase="move", points=c)
        await self.panel_event(panel, "stroke", pc, **common, phase="end", points=[])
        return stroke

    async def set_state(
        self, panel: str, state: str, *, reason: str | None = None, pc: str | None = None
    ) -> None:
        """``visible``, ``hidden``, or ``closed`` (removes the panel) as on the phone."""
        p = self.panel(panel, pc)
        p.visible = state == "visible"
        fields: dict[str, Any] = {"state": state}
        if reason is not None:
            fields["reason"] = reason
        await self.panel_event(panel, "state", pc, **fields)
        if state == "closed":
            del self.panels[self._pc(pc)][panel]

    async def notification_event(
        self, notification: str, type: str, action: str | None = None, pc: str | None = None
    ) -> None:
        """The user answers a notification: ``type`` is ``action``, ``tap`` or ``dismiss``."""
        key = self._pc(pc)
        self.notifications.get(key, {}).pop(notification, None)
        value: dict[str, Any] = {"notification": notification}
        if action is not None:
            value["action"] = action
        for session in list(self.sessions):
            if self._pc_of(session) == key:
                await self._deliver(session, "notification", type, value)

    async def set_permission(self, feature: str, allowed: bool, pc: str | None = None) -> None:
        """The user toggles *Notifications* (``notify``) or *Screen panels* (``panels``)."""
        self.permissions[feature] = allowed
        if allowed:
            return
        key = self._pc(pc)
        if feature == "notify":
            self.notifications.pop(key, None)
        else:
            for panel in list(self.panels.get(key, {})):
                await self.set_state(panel, "closed", reason="revoked", pc=key)

    async def _deliver(self, session: Session, source: str, type: str, value: Any) -> None:
        for sub_id, sub in list(session.subs.items()):
            if sub["source"] == source and type in sub["types"]:
                await self._send_event(session, sub_id, sub, value, type)

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

    def _event_params(
        self, sub_id: str, sub: dict[str, Any], value: Any, type: str | None = None
    ) -> dict[str, Any]:
        info = next(s for s in SENSORS if s["source"] == sub["source"])
        return {
            "subscription": sub_id,
            "seq": sub["seq"],
            "source": sub["source"],
            "type": type or info["types"][0],
            "value": value,
            "unit": info["unit"],
            "ts": int(time.time() * 1000),
            "ts_mono_ns": time.monotonic_ns(),
            "device": DEVICE["id"],
        }

    async def _send_event(
        self,
        session: Session,
        sub_id: str,
        sub: dict[str, Any],
        value: Any,
        type: str | None = None,
    ) -> None:
        # min_delta filters before seq is assigned (PROTOCOL.md section 5), and
        # only applies to number / vec3 / quat values (section 4.3).
        if "last" in sub and sub.get("min_delta"):
            d = _distance(value, sub["last"])
            if d is not None and d < sub["min_delta"]:
                return
        sub["last"] = value
        params = self._event_params(sub_id, sub, value, type)
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
            for panels in self.panels.values():  # PANELS.md 4.1: now detached
                for panel in panels.values():
                    if panel.owner is session:
                        panel.owner = None

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
                "capabilities": ["events", "get", "notify", "panels"],
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
                session.pc = pc
                result["pc"] = pc
                if self.approval_mode is not None and self._approval_key is not None:
                    result["capabilities"].append("approval")
                    result["approval"] = self.approval_info
            session.hello_done = True
            await self._result(session, req_id, result)
            return
        if method == "list_sensors":
            sensors = copy.deepcopy(SENSORS)
            for s in sensors:
                s["available"] = not self._unavailable(s["source"])
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
        elif method == "request_approval":
            if not self.secure:  # APPROVAL.md section 1: secure mode only
                await self._error(session, req_id, "method_not_found", "no method in dev mode")
                return
            await self._request_approval(session, req_id, params)
        elif method in PANEL_METHODS:
            await self._panel_method(session, req_id, method, params)

    def _pc_of(self, session: Session) -> str:
        """Who owns panels and notifications: the PC, or the one dev-mode PC (PANELS.md 1)."""
        return session.pc["id"] if session.pc is not None else "dev"

    def _unavailable(self, source: str) -> bool:
        if source == "panel":
            return not self.permissions["panels"]
        if source == "notification":
            return not self.permissions["notify"]
        return source in self.unavailable

    async def _panel_method(
        self, session: Session, req_id: Any, method: str, params: dict[str, Any]
    ) -> None:
        # Step 8 of PANELS.md 9: permission, panel exists / limit, widgets, rate limit.
        feature = PANEL_METHODS[method]
        if not self.permissions[feature]:
            await self._error(
                session, req_id, "not_allowed", f"{feature} not allowed", permission=feature
            )
            return
        pc = self._pc_of(session)
        if method == "notify":
            if self.notify_limit is not None and self._notify_count >= self.notify_limit:
                await self._error(session, req_id, "rate_limited", "too many notifications")
                return
            self._notify_count += 1
            notes = self.notifications.setdefault(pc, {})
            nid = params.get("id") or f"n-{next(self._notify_ids)}"
            notes.pop(nid, None)
            notes[nid] = params
            while len(notes) > MAX_NOTIFICATIONS:
                del notes[next(iter(notes))]
            await self._result(session, req_id, {"id": nid})
            return
        if method == "notify_cancel":
            self.notifications.get(pc, {}).pop(params["id"], None)
            await self._result(session, req_id, {})
            return
        panels = self.panels.setdefault(pc, {})
        if method == "panel_list":
            listed = [
                {"panel": p.id, "title": p.title, "attached": p.owner is not None,
                 "visible": p.visible}
                for p in panels.values()
            ]  # fmt: skip
            await self._result(session, req_id, {"panels": listed})
            return
        panel_id = params["panel"]
        if method == "panel_show":
            if panel_id not in panels and len(panels) >= MAX_PANELS:
                await self._error(session, req_id, "too_many_panels", "a PC has at most 8")
                return
            panels[panel_id] = FakePanel(panel_id, params["title"], params["layout"], session)
            await self._result(session, req_id, {"panel": panel_id, "visible": False})
            return
        panel = panels.get(panel_id)
        if panel is None:
            await self._error(session, req_id, "unknown_panel", f"no panel {panel_id!r}")
            return
        if method == "panel_attach":
            panel.owner = session
            after = params.get("after_pseq", -1)
            result = {
                "panel": panel.id,
                "title": panel.title,
                "visible": panel.visible,
                "last_pseq": panel.next_pseq - 1,
                "values": panel.values(),
            }
            await self._result(session, req_id, result)
            for value in list(panel.strokes):  # replayed right after the response (4.4)
                if value["pseq"] > after:
                    await self._deliver(session, "panel", "stroke", {**value, "replayed": True})
        elif method == "panel_update":
            for u in params["updates"]:
                widget = panel.widgets.get(u["widget"])
                problem = (
                    update_problem(widget, u["set"])
                    if widget is not None
                    else f"no widget {u['widget']!r}"
                )
                if problem:
                    await self._error(session, req_id, "invalid_params", problem)
                    return
            for u in params["updates"]:  # all valid: apply them all (atomic, 4.5)
                panel.widgets[u["widget"]].update(u["set"])
            await self._result(session, req_id, {})
        elif method == "panel_close":
            del panels[panel_id]
            await self._result(session, req_id, {})
        elif method == "canvas_clear":
            widget = panel.widgets.get(params["widget"])
            if widget is None or widget["type"] != "canvas":
                await self._error(session, req_id, "invalid_params", "not a canvas")
                return
            panel.strokes = [v for v in panel.strokes if v["widget"] != params["widget"]]
            await self._result(session, req_id, {})

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
        if self._unavailable(source):
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
        types = [params["type"]] if "type" in params else list(info["types"])
        result: dict[str, Any] = {
            "subscription": sub_id,
            "source": source,
            "types": types,
            "mode": info["mode"],
        }
        if self.omit_mode:
            del result["mode"]
        sub: dict[str, Any] = {
            "source": source,
            "types": types,
            "seq": 0,
            "min_delta": params.get("min_delta"),
        }
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


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def _approval_params_problem(params: dict[str, Any]) -> str | None:
    """APPROVAL.md section 4 param rules, or None."""
    reason = params.get("reason")
    if not isinstance(reason, str):
        return "reason must be a string"
    # The phone never trims: it checks and hashes the reason exactly as received.
    if not 1 <= len(reason) <= 200:
        return "reason must be 1-200 code points"
    if reason != reason.strip():
        return "reason must not start or end with whitespace"
    if any(unicodedata.category(c) == "Cc" for c in reason):
        return "reason must not contain control characters"
    bidi = "\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"
    if any(c in bidi for c in reason):
        return "reason must not contain bidirectional controls"
    nonce = params.get("nonce")
    try:
        raw = base64.urlsafe_b64decode(nonce + "=" * (-len(nonce) % 4))  # type: ignore[operator]
    except (TypeError, ValueError, binascii.Error):
        return "nonce must be base64url"
    if len(raw) != 32 or _b64url(raw) != nonce:
        return "nonce must be 32 bytes"
    timeout_ms = params.get("timeout_ms", 30000)
    if not isinstance(timeout_ms, int) or isinstance(timeout_ms, bool):
        return "timeout_ms must be an integer"
    if not 5000 <= timeout_ms <= 120000:
        return "timeout_ms must be 5000-120000"
    return None


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
