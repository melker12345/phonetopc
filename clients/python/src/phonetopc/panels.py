"""Notifications and panels: the phone as a screen for the PC (spec/PANELS.md).

:meth:`Phone.show_panel <phonetopc.Phone.show_panel>` returns a :class:`Panel`,
an async iterator of typed :class:`PanelEvent` objects (press, change, submit,
stroke, state). Build layouts with :mod:`phonetopc.ui`.

The ``*_problem`` functions implement the params rules of PANELS.md sections
3-5 and 8 that can be checked from a request alone. The client runs them
before sending, so mistakes surface as :class:`ValueError` with a clear
message instead of a round trip to the phone.
"""

from __future__ import annotations

import asyncio
import collections
import math
import re
import unicodedata
from collections.abc import AsyncIterator, Mapping
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Literal, NamedTuple, cast

from .errors import ProtocolError, SubscriptionOverflow
from .models import Event

if TYPE_CHECKING:
    from .client import Phone

__all__ = [
    "MAX_DEPTH",
    "MAX_PANELS",
    "MAX_WIDGETS",
    "WIDGET_TYPES",
    "AttachResult",
    "ChangeEvent",
    "Panel",
    "PanelEvent",
    "PanelInfo",
    "Point",
    "PressEvent",
    "StateEvent",
    "StrokeEvent",
    "SubmitEvent",
    "layout_problem",
    "panel_event",
    "request_problem",
    "update_problem",
]

#: Panels one PC may have on the phone (section 4.1).
MAX_PANELS = 8
#: Widgets in one layout, and levels of nesting (section 5).
MAX_WIDGETS = 256
MAX_DEPTH = 8
MAX_CHILDREN = 64
MAX_UPDATES = 64

WIDGET_TYPES = (
    "column",
    "row",
    "grid",
    "text",
    "button",
    "toggle",
    "slider",
    "progress",
    "text_input",
    "canvas",
)
_CONTAINERS = ("column", "row", "grid")
#: Widgets that produce events: they need an id, and may be disabled.
_INTERACTIVE = ("button", "toggle", "slider", "text_input", "canvas")
_TONED = ("text", "button", "progress")
_TONES = ("default", "accent", "good", "warn", "bad")

_ID = re.compile(r"[A-Za-z0-9_.-]{1,64}")
_COLOR = re.compile(r"#[0-9A-Fa-f]{6}")
#: Bidirectional control characters: they can make shown text read differently
#: from its bytes, so titles, labels and approval reasons must not contain them.
_BIDI = frozenset(
    "\u061c"  # ARABIC LETTER MARK
    "\u200e"  # LEFT-TO-RIGHT MARK
    "\u200f"  # RIGHT-TO-LEFT MARK
    "\u202a"  # LEFT-TO-RIGHT EMBEDDING
    "\u202b"  # RIGHT-TO-LEFT EMBEDDING
    "\u202c"  # POP DIRECTIONAL FORMATTING
    "\u202d"  # LEFT-TO-RIGHT OVERRIDE
    "\u202e"  # RIGHT-TO-LEFT OVERRIDE
    "\u2066"  # LEFT-TO-RIGHT ISOLATE
    "\u2067"  # RIGHT-TO-LEFT ISOLATE
    "\u2068"  # FIRST STRONG ISOLATE
    "\u2069"  # POP DIRECTIONAL ISOLATE
)


# -- value rules (section 8) ------------------------------------------------------


def _is_num(x: Any) -> bool:
    return isinstance(x, (int, float)) and not isinstance(x, bool) and math.isfinite(x)


def _is_int(x: Any) -> bool:
    return isinstance(x, int) and not isinstance(x, bool)


def _id_problem(value: Any, what: str) -> str | None:
    if not isinstance(value, str) or not _ID.fullmatch(value):
        return f"{what} must be 1-64 characters of A-Z a-z 0-9 _ . - (got {value!r})"
    return None


def _text_problem(value: Any, what: str, lo: int, hi: int, *, newlines: bool = False) -> str | None:
    """Display text: code points counted, no control or bidirectional control characters."""
    if not isinstance(value, str):
        return f"{what} must be a string"
    if not lo <= len(value) <= hi:
        return f"{what} must be {lo}-{hi} characters, got {len(value)}"
    for c in value:
        if c == "\n" and newlines:
            continue
        if unicodedata.category(c) == "Cc":
            nl = " other than newline" if newlines else ""
            return f"{what} must not contain control characters{nl} ({c!r})"
        if c in _BIDI:
            return f"{what} must not contain bidirectional controls (U+{ord(c):04X})"
    return None


def _first(*problems: str | None) -> str | None:
    return next((p for p in problems if p is not None), None)


# -- layouts (section 5) ------------------------------------------------------------


def _widget_problem(w: Mapping[str, Any]) -> str | None:
    """Rules for one widget's own members (not its children). ``type`` must be valid."""
    t = w["type"]
    where = f"{t} {w['id']!r}" if isinstance(w.get("id"), str) else t
    if "id" in w or t in _INTERACTIVE:
        if "id" not in w:
            return f"a {t} needs an id"
        p = _id_problem(w["id"], f"{where}: id")
        if p:
            return p
    if "weight" in w and not (_is_num(w["weight"]) and w["weight"] > 0):
        return f"{where}: weight must be a number > 0"
    if t in _INTERACTIVE and "enabled" in w and not isinstance(w["enabled"], bool):
        return f"{where}: enabled must be a boolean"
    if t in _TONED and "tone" in w and w["tone"] not in _TONES:
        return f"{where}: tone must be one of {', '.join(_TONES)}"

    def opt(key: str, check: Any) -> str | None:
        return check(w[key]) if key in w else None

    def boolean(key: str) -> str | None:
        return opt(key, lambda v: None if isinstance(v, bool) else f"{where}: {key} must be a bool")

    def line(key: str, lo: int, hi: int) -> str | None:
        return opt(key, lambda v: _text_problem(v, f"{where}: {key}", lo, hi))

    def color(key: str) -> str | None:
        return opt(
            key,
            lambda v: (
                None
                if isinstance(v, str) and _COLOR.fullmatch(v)
                else f"{where}: {key} must be a color #rrggbb, got {v!r}"
            ),
        )

    if t == "grid":
        cols: Any = w.get("cols")
        if not (_is_int(cols) and 1 <= cols <= 8):
            return f"{where}: cols must be an integer 1-8, got {cols!r}"
    elif t == "text":
        if "text" not in w:
            return f"{where}: text is required"
        return _first(
            _text_problem(w["text"], f"{where}: text", 0, 4000, newlines=True),
            opt("size", lambda v: None if v in ("small", "body", "title", "display") else
                f"{where}: size must be small, body, title or display"),
            opt("align", lambda v: None if v in ("start", "center", "end") else
                f"{where}: align must be start, center or end"),
        )  # fmt: skip
    elif t == "button":
        if "label" not in w:
            return f"{where}: label is required"
        return line("label", 1, 100)
    elif t == "toggle":
        if "label" not in w:
            return f"{where}: label is required"
        return _first(line("label", 1, 100), boolean("value"))
    elif t == "slider":
        for key in ("min", "max", "value"):
            if key in w and not _is_num(w[key]):
                return f"{where}: {key} must be a number"
        lo, hi = w.get("min", 0), w.get("max", 1)
        if not lo < hi:
            return f"{where}: min must be less than max ({lo} >= {hi})"
        if "step" in w and not (_is_num(w["step"]) and w["step"] > 0):
            return f"{where}: step must be a number > 0"
        return line("label", 0, 100)
    elif t == "progress":
        value = w.get("value")
        if value is not None and not (_is_num(value) and 0 <= value <= 1):
            return f"{where}: value must be a number in [0, 1] or null"
        return line("label", 0, 100)
    elif t == "text_input":
        max_length = w.get("max_length", 1000)
        if not (_is_int(max_length) and 1 <= int(max_length) <= 4000):
            return f"{where}: max_length must be an integer 1-4000"
        return _first(
            line("label", 0, 100),
            line("placeholder", 0, 100),
            opt("value", lambda v: _text_problem(v, f"{where}: value", 0, max_length,
                                                 newlines=True)),
            boolean("multiline"),
            boolean("clear_on_submit"),
        )  # fmt: skip
    elif t == "canvas":
        width = w.get("width", 4)
        if not (_is_num(width) and 0.5 <= width <= 64):
            return f"{where}: width must be a number 0.5-64"
        return _first(boolean("ink"), color("color"), color("background"))
    return None


def layout_problem(layout: Any) -> str | None:
    """Why ``layout`` breaks a rule of PANELS.md section 5, or ``None`` if it is valid."""
    ids: set[str] = set()
    count = 0

    def visit(w: Any, depth: int) -> str | None:
        nonlocal count
        if not isinstance(w, dict):
            return f"a widget must be a JSON object, got {type(w).__name__}"
        count += 1
        if count > MAX_WIDGETS:
            return f"a layout has at most {MAX_WIDGETS} widgets"
        if depth > MAX_DEPTH:
            return f"a layout is at most {MAX_DEPTH} levels deep"
        t = w.get("type")
        if t not in WIDGET_TYPES:
            return f"unknown widget type {t!r}"
        p = _widget_problem(w)
        if p:
            return p
        if isinstance(w.get("id"), str):
            if w["id"] in ids:
                return f"widget id {w['id']!r} is used twice"
            ids.add(w["id"])
        if t in _CONTAINERS:
            children = w.get("children")
            if not isinstance(children, list) or not 1 <= len(children) <= MAX_CHILDREN:
                return f"a {t} needs 1-{MAX_CHILDREN} children"
            for child in children:
                p = visit(child, depth + 1)
                if p:
                    return p
        return None

    return visit(layout, 1)


def update_problem(widget: Mapping[str, Any], props: Mapping[str, Any]) -> str | None:
    """Why setting ``props`` on ``widget`` (its JSON object) breaks its type's rules."""
    for key in ("type", "id", "children"):
        if key in props:
            return f"{key} can't be changed by panel_update; call panel_show again"
    return _widget_problem({**widget, **props})


def _notify_problem(p: Mapping[str, Any]) -> str | None:
    problem = _first(
        _text_problem(p.get("title"), "title", 1, 100),
        _text_problem(p["body"], "body", 0, 1000, newlines=True) if "body" in p else None,
        _id_problem(p["id"], "id") if "id" in p else None,
    )
    if problem:
        return problem
    if "priority" in p and p["priority"] not in ("default", "high"):
        return "priority must be default or high"
    actions = p.get("actions", [])
    if not isinstance(actions, list) or len(actions) > 3:
        return "actions must be a list of at most 3 actions"
    seen = set()
    for a in actions:
        if not isinstance(a, dict):
            return "each action must be an object with id and label"
        problem = _first(
            _id_problem(a.get("id"), "action id"),
            _text_problem(a.get("label"), "action label", 1, 30),
        )
        if problem:
            return problem
        if a["id"] in seen:
            return f"action id {a['id']!r} is used twice"
        seen.add(a["id"])
    return None


def request_problem(method: str, params: Mapping[str, Any]) -> str | None:
    """Why ``params`` of a notification or panel ``method`` are invalid (``-32602``).

    Only what can be checked from the request alone (step 7 of PROTOCOL.md
    section 2): not whether a panel or widget exists. ``None`` for other methods.
    """
    p = params
    if method == "notify":
        return _notify_problem(p)
    if method == "notify_cancel":
        return _id_problem(p.get("id"), "id")
    if method == "panel_list":
        return None
    if method not in (
        "panel_show",
        "panel_attach",
        "panel_update",
        "panel_close",
        "canvas_clear",
    ):
        return None
    problem = _id_problem(p.get("panel"), "panel")
    if problem:
        return problem
    if method == "panel_show":
        if "layout" not in p:
            return "layout is required"
        if "keep_awake" in p and not isinstance(p["keep_awake"], bool):
            return "keep_awake must be a boolean"
        if "orientation" in p and p["orientation"] not in ("any", "portrait", "landscape"):
            return "orientation must be any, portrait or landscape"
        return _first(_text_problem(p.get("title"), "title", 1, 60), layout_problem(p["layout"]))
    if method == "panel_attach":
        after = p.get("after_pseq", -1)
        if not (_is_int(after) and after >= -1):
            return "after_pseq must be an integer >= -1"
    elif method == "panel_update":
        updates = p.get("updates")
        if not isinstance(updates, list) or not 1 <= len(updates) <= MAX_UPDATES:
            return f"updates must be a list of 1-{MAX_UPDATES} updates"
        for u in updates:
            if not isinstance(u, dict) or not isinstance(u.get("set"), dict):
                return 'each update must be {"widget": <id>, "set": {...}}'
            problem = _id_problem(u.get("widget"), "widget")
            if problem:
                return problem
            for key in ("type", "id", "children"):
                if key in u["set"]:
                    return f"{key} can't be changed by panel_update; call panel_show again"
    elif method == "canvas_clear":
        return _id_problem(p.get("widget"), "widget")
    return None


# -- results and events -------------------------------------------------------------


@dataclass(frozen=True)
class PanelInfo:
    """One entry of ``panel_list``."""

    panel: str
    title: str
    attached: bool
    visible: bool

    @classmethod
    def from_json(cls, data: Any) -> PanelInfo:
        if not isinstance(data, dict):
            raise ProtocolError(f"panel_list: malformed entry {data!r}")
        try:
            return cls(
                panel=str(data["panel"]),
                title=str(data["title"]),
                attached=bool(data["attached"]),
                visible=bool(data["visible"]),
            )
        except KeyError as exc:
            raise ProtocolError(f"panel_list: entry without {exc}") from None


@dataclass(frozen=True)
class AttachResult:
    """Result of ``panel_attach`` (section 4.4).

    ``values`` holds the current value of every toggle and slider, by widget
    id; ``last_pseq`` is the panel's latest event number (-1: none yet).
    """

    panel: str
    title: str
    visible: bool
    last_pseq: int
    values: Mapping[str, bool | float]

    @classmethod
    def from_json(cls, data: Any) -> AttachResult:
        if not isinstance(data, dict):
            raise ProtocolError(f"panel_attach: malformed result {data!r}")
        last: Any = data.get("last_pseq")
        values = data.get("values")
        if not _is_int(last) or not isinstance(values, dict):
            raise ProtocolError(f"panel_attach: malformed result {data!r}")
        return cls(
            panel=str(data.get("panel")),
            title=str(data.get("title")),
            visible=bool(data.get("visible")),
            last_pseq=last,
            values=dict(values),
        )


class Point(NamedTuple):
    """One point of a stroke: position in [0, 1] of the canvas, pressure, ms since start."""

    x: float
    y: float
    pressure: float
    t: int


@dataclass(frozen=True)
class PanelEvent:
    """An event of a panel (section 6). Subclasses add the members of each ``type``."""

    panel: str
    pseq: int
    """Number of this event in its panel; survives reconnects (section 6)."""
    type: str
    widget: str | None
    replayed: bool
    """True for strokes replayed by ``panel_attach`` (section 4.4)."""
    event: Event = field(repr=False, compare=False)
    """The underlying ``event`` notification."""


@dataclass(frozen=True)
class StateEvent(PanelEvent):
    """The panel became ``visible`` or ``hidden``, or was ``closed`` (``reason`` user/revoked)."""

    state: str = ""
    reason: str | None = None


@dataclass(frozen=True)
class PressEvent(PanelEvent):
    """A button was pressed."""


@dataclass(frozen=True)
class ChangeEvent(PanelEvent):
    """A toggle (``value`` bool) or slider (``value`` number) changed.

    ``final`` is true when a slider is released.
    """

    value: bool | float = False
    final: bool = False


@dataclass(frozen=True)
class SubmitEvent(PanelEvent):
    """Text submitted from a ``text_input``."""

    value: str = ""


@dataclass(frozen=True)
class StrokeEvent(PanelEvent):
    """A chunk of a canvas stroke (section 7).

    ``phase`` is ``start``, ``move``, ``end`` or ``cancel``; ``size`` (canvas
    width and height in dp) comes with ``start`` only.
    """

    stroke: int = 0
    phase: str = ""
    tool: str = "unknown"
    points: tuple[Point, ...] = ()
    size: tuple[float, float] | None = None


def panel_event(ev: Event) -> PanelEvent:
    """Parse a ``panel`` source event into its typed form."""
    v = ev.value
    if not isinstance(v, dict) or not isinstance(v.get("panel"), str) or not _is_int(v.get("pseq")):
        raise ProtocolError(f"panel event without panel/pseq: {v!r}")
    base: dict[str, Any] = {
        "panel": v["panel"],
        "pseq": v["pseq"],
        "type": ev.type,
        "widget": v.get("widget") if isinstance(v.get("widget"), str) else None,
        "replayed": v.get("replayed") is True,
        "event": ev,
    }
    if ev.type == "state":
        reason = v.get("reason")
        return StateEvent(
            **base, state=str(v.get("state")), reason=reason if isinstance(reason, str) else None
        )
    if ev.type == "press":
        return PressEvent(**base)
    if ev.type == "change":
        value = v.get("value")
        if not isinstance(value, bool) and not _is_num(value):
            raise ProtocolError(f"change event with value {value!r}")
        return ChangeEvent(**base, value=cast("bool | float", value), final=v.get("final") is True)
    if ev.type == "submit":
        return SubmitEvent(**base, value=str(v.get("value", "")))
    if ev.type == "stroke":
        try:
            points = tuple(
                Point(float(p[0]), float(p[1]), float(p[2]), int(p[3])) for p in v["points"]
            )
            size = v.get("size")
            return StrokeEvent(
                **base,
                stroke=int(v["stroke"]),
                phase=str(v["phase"]),
                tool=str(v.get("tool", "unknown")),
                points=points,
                size=(float(size[0]), float(size[1])) if size is not None else None,
            )
        except (KeyError, IndexError, TypeError, ValueError) as exc:
            raise ProtocolError(f"malformed stroke event {v!r}: {exc!r}") from None
    return PanelEvent(**base)  # a type from a later draft


# -- Panel ----------------------------------------------------------------------------

DEFAULT_PANEL_QUEUE = 4096


class Panel:
    """A panel on the phone, owned by this connection. Get one from
    :meth:`Phone.show_panel <phonetopc.Phone.show_panel>` or
    :meth:`Phone.attach_panel <phonetopc.Phone.attach_panel>`.

    Iterate it for its events::

        async for ev in panel:
            if isinstance(ev, phonetopc.PressEvent) and ev.widget == "mute":
                ...

    Iteration ends when the panel is closed (by :meth:`close`, by the user on
    the phone: a final ``StateEvent`` with ``state == "closed"``, or by
    another ``show_panel`` with the same id) or the connection ends.
    ``last_pseq`` is the ``pseq`` of the last event iteration returned: pass it
    to :meth:`Phone.attach_panel` after a reconnect to get the strokes you
    missed.
    """

    def __init__(
        self,
        phone: Phone,
        panel: str,
        title: str,
        layout: Mapping[str, Any] | None,
        *,
        visible: bool = False,
        last_pseq: int = -1,
        queue_size: int = DEFAULT_PANEL_QUEUE,
    ) -> None:
        self._phone = phone
        self.id = panel
        self.title = title
        #: The layout sent with ``show_panel`` (``None`` after ``attach_panel``),
        #: kept up to date by :meth:`update`. Used to check updates locally.
        self.layout = layout
        #: Whether the panel is on screen, as far as this client knows.
        self.visible = visible
        #: ``pseq`` of the last event returned by iteration (-1: none yet).
        self.last_pseq = last_pseq
        self.queue_size = queue_size
        #: Why iteration ended (``None`` while open or after a normal close).
        self.closed_reason: BaseException | None = None
        #: The ``panel_attach`` result, for a panel from ``attach_panel``.
        self.attach_result: AttachResult | None = None
        self._queue: collections.deque[PanelEvent] = collections.deque()
        self._has_data = asyncio.Event()
        self._ended = False
        self._raise: BaseException | None = None

    @property
    def closed(self) -> bool:
        return self._ended

    async def update(self, widget: str, **props: Any) -> None:
        """Change properties of one widget, e.g. ``update("msg", text="done", tone="good")``."""
        await self.update_many({widget: props})

    async def update_many(self, updates: Mapping[str, Mapping[str, Any]]) -> None:
        """Change several widgets at once, atomically: ``{widget_id: {prop: value}}``."""
        items: list[dict[str, Any]] = [{"widget": w, "set": dict(p)} for w, p in updates.items()]
        widgets = _index(self.layout) if self.layout is not None else None
        if widgets is not None:
            for item in items:
                w = widgets.get(item["widget"])
                if w is None:
                    raise ValueError(f"panel {self.id!r} has no widget {item['widget']!r}")
                problem = update_problem(w, item["set"])
                if problem:
                    raise ValueError(problem)
        await self._phone._panel_call("panel_update", {"panel": self.id, "updates": items})
        if widgets is not None:
            for item in items:
                widgets[item["widget"]].update(item["set"])

    async def clear_canvas(self, widget: str) -> None:
        """Erase a canvas, and the strokes the phone keeps for it."""
        await self._phone._panel_call("canvas_clear", {"panel": self.id, "widget": widget})

    async def close(self) -> None:
        """Remove the panel from the phone and end iteration."""
        try:
            if not self._ended and not self._phone.closed:
                await self._phone._panel_call("panel_close", {"panel": self.id})
        finally:
            self._phone._forget_panel(self)
            self._end(None)

    async def __aenter__(self) -> Panel:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    def __aiter__(self) -> AsyncIterator[PanelEvent]:
        return self

    async def __anext__(self) -> PanelEvent:
        while not self._queue:
            if self._ended:
                if self._raise is not None:
                    raise self._raise
                raise StopAsyncIteration
            self._has_data.clear()
            await self._has_data.wait()
        ev = self._queue.popleft()
        self.last_pseq = max(self.last_pseq, ev.pseq)
        return ev

    def __repr__(self) -> str:
        state = " closed" if self._ended else (" visible" if self.visible else "")
        return f"<Panel {self.id!r} title={self.title!r} last_pseq={self.last_pseq}{state}>"

    # -- called by Phone (inside the reader task) --------------------------

    def _push(self, ev: PanelEvent) -> None:
        if self._ended:
            return
        if isinstance(ev, StateEvent):
            self.visible = ev.state == "visible"
        if len(self._queue) >= self.queue_size:
            self._phone._forget_panel(self)
            self._end(
                SubscriptionOverflow(
                    f"panel {self.id!r} overflowed its queue of {self.queue_size} events"
                ),
                raise_it=True,
            )
            return
        self._queue.append(ev)
        self._has_data.set()
        if isinstance(ev, StateEvent) and ev.state == "closed":
            self._phone._forget_panel(self)
            self._end(None)

    def _end(self, reason: BaseException | None, *, raise_it: bool = False) -> None:
        if self._ended:
            return
        self._ended = True
        self.closed_reason = reason
        if raise_it:
            self._raise = reason
        self._has_data.set()


def _index(layout: Mapping[str, Any]) -> dict[str, dict[str, Any]]:
    """Widgets with an id, by id (the dicts themselves, so updates can be applied)."""
    found: dict[str, dict[str, Any]] = {}
    stack: list[Any] = [layout]
    while stack:
        w = stack.pop()
        if isinstance(w, dict):
            if isinstance(w.get("id"), str):
                found[w["id"]] = w
            stack.extend(w.get("children") or [])
    return found


Orientation = Literal["any", "portrait", "landscape"]
