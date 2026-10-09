"""Blocking wrapper for scripts that don't want asyncio.

The asyncio client runs on an event loop in a private daemon thread; every
method here submits a coroutine to it and waits for the result::

    import phonetopc.sync

    with phonetopc.sync.connect() as phone:  # the phone paired with `phonetopc pair`
        print(phone.get("battery").value)
        with phone.subscribe("gyro", rate_hz=50) as events:
            for ev in events:
                print(ev.value)
"""

from __future__ import annotations

import asyncio
import threading
from collections.abc import Coroutine, Iterator, Mapping
from typing import Any, TypeVar

from .approval import DEFAULT_APPROVAL_TIMEOUT, ApprovalResult
from .client import DEFAULT_QUEUE_SIZE, DEFAULT_REQUEST_TIMEOUT, Overflow, Phone, Subscription
from .client import connect as _async_connect
from .models import DeviceInfo, Event, HelloResult, PCInfo, SensorInfo

__all__ = ["SyncPhone", "SyncSubscription", "connect"]

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
        """The underlying async subscription (for ``dropped``, ``info``, ...)."""
        return self._sub

    @property
    def dropped(self) -> int:
        return self._sub.dropped

    @property
    def id(self) -> str | None:
        return self._sub.id

    def start(self) -> SyncSubscription:
        self._runner.run(self._sub.start())
        return self

    def close(self) -> None:
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


async def _next_or_none(sub: Subscription) -> Event | None:
    # StopAsyncIteration can't cross run_coroutine_threadsafe cleanly.
    try:
        return await sub.__anext__()
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
        return self._phone.device

    @property
    def pc(self) -> PCInfo | None:
        return self._phone.pc

    @property
    def secure(self) -> bool:
        return self._phone.secure

    @property
    def closed(self) -> bool:
        return self._phone.closed

    def call(self, method: str, params: Mapping[str, Any] | None = None) -> Any:
        return self._runner.run(self._phone.call(method, params))

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
        return self._phone.approval_key

    def list_sensors(self) -> list[SensorInfo]:
        return self._runner.run(self._phone.list_sensors())

    def get(self, source: str, type: str | None = None) -> Event:
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
        try:
            if not self._runner.loop.is_closed():
                self._runner.run(self._phone.close())
        finally:
            self._runner.stop()

    def __enter__(self) -> SyncPhone:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def connect(
    addr: str | None = None,
    *,
    device: str | None = None,
    dev: bool = False,
    client_name: str = "phonetopc-python",
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
