"""Blocking wrapper for scripts that don't want asyncio.

The asyncio client runs on an event loop in a private daemon thread; every
method here submits a coroutine to it and waits for the result::

    import phonetopc.sync

    with phonetopc.sync.connect() as phone:  # the phone paired with `phonetopc pair`
        print(phone.get("battery").value)
        with phone.subscribe("gyro", rate_hz=50) as events:
            for ev in events:
                print(ev.value)

:class:`SyncPhone`, :class:`SyncSubscription` and :class:`SyncPanel` mirror
every public member of :class:`~phonetopc.Phone`,
:class:`~phonetopc.Subscription` and :class:`~phonetopc.Panel`: attributes and
properties pass through (read-only, except ``SyncPhone.request_timeout`` and
``SyncPhone.queue_size``), coroutines become blocking calls, and async
iteration / ``async with`` become plain iteration / ``with``. The wrapped
async object is available as ``.phone`` / ``.subscription`` / ``.panel``.
``tests/test_sync_parity.py`` keeps the two APIs in step.
"""

from __future__ import annotations

import asyncio
import threading
from collections.abc import Coroutine, Iterable, Iterator, Mapping
from typing import Any, Literal, TypeVar

from ._version import __version__
from .approval import DEFAULT_APPROVAL_TIMEOUT, ApprovalResult
from .client import (
    _UNSET,
    DEFAULT_QUEUE_SIZE,
    DEFAULT_REQUEST_TIMEOUT,
    Overflow,
    Phone,
    Subscription,
)
from .client import connect as _async_connect
from .errors import ConnectionClosed
from .models import DeviceInfo, Event, HelloResult, PCInfo, SensorInfo, ServerInfo, SubscribeResult
from .panels import AttachResult, Orientation, Panel, PanelEvent, PanelInfo
from .store import PairedDevice

__all__ = ["SyncPanel", "SyncPhone", "SyncSubscription", "connect"]

T = TypeVar("T")


class _LoopThread:
    def __init__(self) -> None:
        self.loop = asyncio.new_event_loop()
        self._thread = threading.Thread(
            target=self.loop.run_forever, name="phonetopc-loop", daemon=True
        )
        self._thread.start()

    def run(self, coro: Coroutine[Any, Any, T]) -> T:
        if threading.current_thread() is self._thread:
            coro.close()
            raise RuntimeError("phonetopc.sync called from its own event loop thread")
        return asyncio.run_coroutine_threadsafe(coro, self.loop).result()

    def stop(self) -> None:
        if self.loop.is_closed():
            return
        self.loop.call_soon_threadsafe(self.loop.stop)
        self._thread.join(timeout=5)
        if not self._thread.is_alive():
            self.loop.close()


class SyncSubscription:
    """Blocking counterpart of :class:`~phonetopc.client.Subscription`."""

    def __init__(self, runner: _LoopThread, sub: Subscription) -> None:
        self._runner = runner
        self._sub = sub

    @property
    def subscription(self) -> Subscription:
        """The underlying async :class:`~phonetopc.client.Subscription`."""
        return self._sub

    @property
    def id(self) -> str | None:
        """Server-assigned subscription id (``None`` until started)."""
        return self._sub.id

    @property
    def rate_hz(self) -> float | None:
        """Effective rate reported by the server, if any."""
        return self._sub.rate_hz

    @property
    def mode(self) -> str | None:
        """Sensor mode (``"continuous"`` / ``"on_change"``); ``None`` until started."""
        return self._sub.mode

    @property
    def dropped(self) -> int:
        """Total events lost: server-side (``seq`` gaps) plus local queue overflow."""
        return self._sub.dropped

    @property
    def dropped_remote(self) -> int:
        """Events the server skipped (``seq`` gaps)."""
        return self._sub.dropped_remote

    @property
    def dropped_local(self) -> int:
        """Events discarded locally because the queue was full."""
        return self._sub.dropped_local

    @property
    def received(self) -> int:
        """Events received from the server for this subscription."""
        return self._sub.received

    @property
    def closed(self) -> bool:
        return self._sub.closed

    @property
    def closed_reason(self) -> BaseException | None:
        """Why iteration ended (``None`` while open or after a normal close)."""
        return self._sub.closed_reason

    @property
    def info(self) -> SubscribeResult | None:
        """Server's answer to ``subscribe``; ``None`` until started."""
        return self._sub.info

    @property
    def source(self) -> str:
        return self._sub.source

    @property
    def type(self) -> str | None:
        return self._sub.type

    @property
    def requested_rate_hz(self) -> float | None:
        """The ``rate_hz`` asked for (see :attr:`rate_hz` for what the server chose)."""
        return self._sub.requested_rate_hz

    @property
    def min_delta(self) -> float | None:
        return self._sub.min_delta

    @property
    def queue_size(self) -> int:
        return self._sub.queue_size

    @property
    def overflow(self) -> Overflow:
        """The overflow policy in effect (``"auto"`` is resolved once started)."""
        return self._sub.overflow

    def qsize(self) -> int:
        """Number of events waiting to be consumed."""
        return self._sub.qsize()

    def start(self) -> SyncSubscription:
        """Send ``subscribe``. Called by ``with``; returns ``self``."""
        self._runner.run(self._sub.start())
        return self

    def close(self) -> None:
        """Send ``unsubscribe`` (if still connected) and end iteration."""
        self._runner.run(self._sub.close())

    def __enter__(self) -> SyncSubscription:
        return self.start()

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __iter__(self) -> Iterator[Event]:
        return self

    def __next__(self) -> Event:
        ev = self._runner.run(_next_or_none(self._sub))
        if ev is None:
            raise StopIteration
        return ev

    def __repr__(self) -> str:
        return f"<Sync{repr(self._sub)[1:]}"


async def _next_or_none(sub: Subscription) -> Event | None:
    # StopAsyncIteration can't cross run_coroutine_threadsafe cleanly.
    try:
        return await sub.__anext__()
    except StopAsyncIteration:
        return None


class SyncPanel:
    """Blocking counterpart of :class:`~phonetopc.panels.Panel`; iterate it for events."""

    def __init__(self, runner: _LoopThread, panel: Panel) -> None:
        self._runner = runner
        self._panel = panel

    @property
    def panel(self) -> Panel:
        """The underlying async :class:`~phonetopc.panels.Panel`."""
        return self._panel

    @property
    def id(self) -> str:
        return self._panel.id

    @property
    def title(self) -> str:
        return self._panel.title

    @property
    def layout(self) -> Mapping[str, Any] | None:
        """The layout sent with ``show_panel`` (``None`` after ``attach_panel``)."""
        return self._panel.layout

    @property
    def last_pseq(self) -> int:
        """``pseq`` of the last event returned by iteration (-1: none yet)."""
        return self._panel.last_pseq

    @property
    def visible(self) -> bool:
        """Whether the panel is on screen, as far as this client knows."""
        return self._panel.visible

    @property
    def queue_size(self) -> int:
        return self._panel.queue_size

    @property
    def closed(self) -> bool:
        return self._panel.closed

    @property
    def closed_reason(self) -> BaseException | None:
        """Why iteration ended (``None`` while open or after a normal close)."""
        return self._panel.closed_reason

    @property
    def attach_result(self) -> AttachResult | None:
        """The ``panel_attach`` result, for a panel from ``attach_panel``."""
        return self._panel.attach_result

    def update(self, widget: str, **props: Any) -> None:
        """Blocking :meth:`Panel.update <phonetopc.Panel.update>`."""
        self._runner.run(self._panel.update(widget, **props))

    def update_many(self, updates: Mapping[str, Mapping[str, Any]]) -> None:
        """Blocking :meth:`Panel.update_many <phonetopc.Panel.update_many>`."""
        self._runner.run(self._panel.update_many(updates))

    def clear_canvas(self, widget: str) -> None:
        """Blocking :meth:`Panel.clear_canvas <phonetopc.Panel.clear_canvas>`."""
        self._runner.run(self._panel.clear_canvas(widget))

    def close(self) -> None:
        """Remove the panel from the phone and end iteration."""
        self._runner.run(self._panel.close())

    def __enter__(self) -> SyncPanel:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __iter__(self) -> Iterator[PanelEvent]:
        return self

    def __next__(self) -> PanelEvent:
        ev = self._runner.run(_next_panel_event(self._panel))
        if ev is None:
            raise StopIteration
        return ev

    def __repr__(self) -> str:
        return f"<Sync{repr(self._panel)[1:]}"


async def _next_panel_event(panel: Panel) -> PanelEvent | None:
    try:
        return await panel.__anext__()
    except StopAsyncIteration:
        return None


class SyncPhone:
    """Blocking counterpart of :class:`~phonetopc.client.Phone`."""

    def __init__(self, runner: _LoopThread, phone: Phone) -> None:
        self._runner = runner
        self._phone = phone

    @property
    def phone(self) -> Phone:
        """The underlying async :class:`~phonetopc.client.Phone`."""
        return self._phone

    @property
    def hello(self) -> HelloResult:
        return self._phone.hello

    @property
    def device(self) -> DeviceInfo:
        """:class:`~phonetopc.models.DeviceInfo` from ``hello``."""
        return self._phone.device

    @property
    def server(self) -> ServerInfo:
        """:class:`~phonetopc.models.ServerInfo` from ``hello``."""
        return self._phone.server

    @property
    def capabilities(self) -> tuple[str, ...]:
        return self._phone.capabilities

    @property
    def pc(self) -> PCInfo | None:
        """Which pairing the phone recognised (``hello`` ``result.pc``); secure mode only."""
        return self._phone.pc

    @property
    def secure(self) -> bool:
        """``True`` for pinned TLS with a token, ``False`` for dev mode (plaintext)."""
        return self._phone.secure

    @property
    def paired_device(self) -> PairedDevice | None:
        """The pairing used (secure mode), ``None`` in dev mode."""
        return self._phone.paired_device

    @property
    def url(self) -> str | None:
        """The URL connected to."""
        return self._phone.url

    @property
    def closed(self) -> bool:
        return self._phone.closed

    @property
    def closed_reason(self) -> ConnectionClosed | None:
        """Why the connection ended (``None`` while open)."""
        return self._phone.closed_reason

    @property
    def request_timeout(self) -> float | None:
        """Default timeout of requests, in seconds (``None``: wait forever). Settable."""
        return self._phone.request_timeout

    @request_timeout.setter
    def request_timeout(self, value: float | None) -> None:
        self._phone.request_timeout = value

    @property
    def queue_size(self) -> int:
        """Default queue size of new subscriptions. Settable."""
        return self._phone.queue_size

    @queue_size.setter
    def queue_size(self, value: int) -> None:
        self._phone.queue_size = value

    def call(
        self, method: str, params: Mapping[str, Any] | None = None, *, timeout: Any = _UNSET
    ) -> Any:
        """Blocking :meth:`Phone.call <phonetopc.Phone.call>`."""
        return self._runner.run(self._phone.call(method, params, timeout=timeout))

    def request_approval(
        self, reason: str, timeout: float = DEFAULT_APPROVAL_TIMEOUT
    ) -> ApprovalResult:
        """Blocking :meth:`Phone.request_approval <phonetopc.Phone.request_approval>`."""
        return self._runner.run(self._phone.request_approval(reason, timeout))

    def accept_approval_key(self) -> bool:
        """Blocking :meth:`Phone.accept_approval_key <phonetopc.Phone.accept_approval_key>`."""
        return self._runner.run(self._phone.accept_approval_key())

    @property
    def approval_key(self) -> str | None:
        """Pin of the phone's approval key in this session (``hello``), if it has one."""
        return self._phone.approval_key

    def notify(
        self,
        title: str,
        body: str | None = None,
        *,
        id: str | None = None,
        actions: Mapping[str, str] | Iterable[tuple[str, str]] | None = None,
        priority: Literal["default", "high"] | None = None,
    ) -> str:
        """Blocking :meth:`Phone.notify <phonetopc.Phone.notify>`."""
        return self._runner.run(
            self._phone.notify(title, body, id=id, actions=actions, priority=priority)
        )

    def notify_cancel(self, id: str) -> None:
        """Blocking :meth:`Phone.notify_cancel <phonetopc.Phone.notify_cancel>`."""
        self._runner.run(self._phone.notify_cancel(id))

    def show_panel(
        self,
        panel: str,
        title: str,
        layout: Mapping[str, Any],
        *,
        keep_awake: bool | None = None,
        orientation: Orientation | None = None,
        validate: bool = True,
    ) -> SyncPanel:
        """Blocking :meth:`Phone.show_panel <phonetopc.Phone.show_panel>`."""
        made = self._runner.run(
            self._phone.show_panel(
                panel,
                title,
                layout,
                keep_awake=keep_awake,
                orientation=orientation,
                validate=validate,
            )
        )
        return SyncPanel(self._runner, made)

    def attach_panel(self, panel: str, *, after_pseq: int = -1) -> SyncPanel:
        """Blocking :meth:`Phone.attach_panel <phonetopc.Phone.attach_panel>`."""
        made = self._runner.run(self._phone.attach_panel(panel, after_pseq=after_pseq))
        return SyncPanel(self._runner, made)

    def list_panels(self) -> list[PanelInfo]:
        """Blocking :meth:`Phone.list_panels <phonetopc.Phone.list_panels>`."""
        return self._runner.run(self._phone.list_panels())

    def list_sensors(self) -> list[SensorInfo]:
        """Blocking :meth:`Phone.list_sensors <phonetopc.Phone.list_sensors>`."""
        return self._runner.run(self._phone.list_sensors())

    def get(self, source: str, type: str | None = None) -> Event:
        """Blocking :meth:`Phone.get <phonetopc.Phone.get>`."""
        return self._runner.run(self._phone.get(source, type))

    def subscribe(
        self,
        source: str,
        *,
        type: str | None = None,
        rate_hz: float | None = None,
        min_delta: float | None = None,
        queue_size: int | None = None,
        overflow: Overflow = "auto",
    ) -> SyncSubscription:
        """Prepare a subscription; ``with`` it to start receiving events."""
        sub = self._phone.subscribe(
            source,
            type=type,
            rate_hz=rate_hz,
            min_delta=min_delta,
            queue_size=queue_size,
            overflow=overflow,
        )
        return SyncSubscription(self._runner, sub)

    def close(self) -> None:
        """Close the connection and stop the background event loop."""
        try:
            if not self._runner.loop.is_closed():
                self._runner.run(self._phone.close())
        finally:
            self._runner.stop()

    def wait_closed(self) -> None:
        """Block until the connection is closed (by either side)."""
        if self._phone.closed:  # also covers close(), after which the loop is gone
            return
        self._runner.run(self._phone.wait_closed())

    def __enter__(self) -> SyncPhone:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __repr__(self) -> str:
        return f"<Sync{repr(self._phone)[1:]}"


def connect(
    addr: str | None = None,
    *,
    device: str | None = None,
    dev: bool = False,
    client_name: str = "phonetopc-python",
    client_version: str = __version__,
    request_timeout: float | None = DEFAULT_REQUEST_TIMEOUT,
    open_timeout: float | None = 10.0,
    queue_size: int = DEFAULT_QUEUE_SIZE,
    **ws_kwargs: Any,
) -> SyncPhone:
    """Blocking :func:`phonetopc.connect`. Use as ``with connect(...) as phone:``.

    Takes the same arguments: by default a secure connection to the paired
    phone (``device=`` picks one), ``dev=True`` or a ``ws://`` URL for dev mode.
    """
    runner = _LoopThread()

    async def _open() -> Phone:
        return await _async_connect(
            addr,
            device=device,
            dev=dev,
            client_name=client_name,
            client_version=client_version,
            request_timeout=request_timeout,
            open_timeout=open_timeout,
            queue_size=queue_size,
            **ws_kwargs,
        )

    try:
        phone = runner.run(_open())
    except BaseException:
        runner.stop()
        raise
    return SyncPhone(runner, phone)
