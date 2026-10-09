"""asyncio client for the phonetopc/1 protocol (spec/PROTOCOL.md).

One :class:`Phone` owns one WebSocket and one reader task. The reader
correlates responses with pending requests by ``id`` and routes ``event``
notifications to the right :class:`Subscription` queue, so any number of
requests and subscriptions can be in flight concurrently.
"""

from __future__ import annotations

import asyncio
import collections
import contextlib
import itertools
import json
import logging
import os
from collections.abc import AsyncIterator, Generator, Mapping
from typing import (
    Any,
    Callable,
    Literal,
)
from urllib.parse import urlsplit, urlunsplit

import websockets.exceptions
from websockets.asyncio.client import ClientConnection
from websockets.asyncio.client import connect as _ws_connect

from ._version import __version__
from .errors import (
    ConnectionClosed,
    ProtocolError,
    RequestTimeout,
    SubscriptionOverflow,
    UnknownSubscription,
    error_from_response,
)
from .models import (
    CONTINUOUS,
    ON_CHANGE,
    DeviceInfo,
    Event,
    HelloResult,
    SensorInfo,
    ServerInfo,
    SubscribeResult,
)

__all__ = [
    "DEFAULT_ADDR",
    "DEFAULT_PATH",
    "DEFAULT_PORT",
    "ENV_ADDR",
    "PROTOCOL",
    "Overflow",
    "Phone",
    "Subscription",
    "connect",
    "resolve_url",
]

logger = logging.getLogger("phonetopc")

PROTOCOL = "phonetopc/1"
DEFAULT_PORT = 8765
DEFAULT_PATH = "/v1/ws"
DEFAULT_ADDR = f"127.0.0.1:{DEFAULT_PORT}"
ENV_ADDR = "PHONETOPC_ADDR"

DEFAULT_QUEUE_SIZE = 256
DEFAULT_REQUEST_TIMEOUT = 10.0

#: What a subscription does when its local queue is full.
#: ``"drop_oldest"`` discards the oldest queued event and counts it in
#: ``dropped``; ``"error"`` ends the subscription with
#: :class:`~phonetopc.errors.SubscriptionOverflow`; ``"auto"`` picks
#: ``drop_oldest`` for continuous sensors and ``error`` for everything else
#: (the server never drops on_change events, so neither does the client).
Overflow = Literal["auto", "drop_oldest", "error"]

_UNSET: Any = object()


def resolve_url(addr: str | None = None) -> str:
    """Turn a user-supplied address into a WebSocket URL.

    * ``None`` -> ``$PHONETOPC_ADDR``, else ``127.0.0.1:8765``
    * ``host`` / ``host:port`` / ``[v6]:port`` / bare IPv6 -> ``ws://host:port/v1/ws``
      (port defaults to 8765)
    * ``ws://`` / ``wss://`` URLs are kept as given, except that an empty path
      becomes ``/v1/ws``.
    """
    if addr is None:
        addr = os.environ.get(ENV_ADDR) or DEFAULT_ADDR
    addr = addr.strip()
    if not addr:
        raise ValueError("empty address")
    if "://" in addr:
        parts = urlsplit(addr)
        if parts.scheme not in ("ws", "wss"):
            raise ValueError(f"unsupported URL scheme {parts.scheme!r} (use ws:// or wss://)")
        if not parts.hostname:
            raise ValueError(f"no host in {addr!r}")
        parts.port  # noqa: B018 - raises ValueError on a malformed port
        path = parts.path if parts.path not in ("", "/") else DEFAULT_PATH
        return urlunsplit((parts.scheme, parts.netloc, path, parts.query, ""))
    if not addr.startswith("[") and addr.count(":") > 1:
        addr = f"[{addr}]"  # bare IPv6 address, no port
    parts = urlsplit("ws://" + addr)
    if not parts.hostname:
        raise ValueError(f"no host in {addr!r}")
    port = parts.port  # raises ValueError on a malformed port
    netloc = parts.netloc if port is not None else f"{parts.netloc}:{DEFAULT_PORT}"
    path = parts.path if parts.path not in ("", "/") else DEFAULT_PATH
    return urlunsplit(("ws", netloc, path, parts.query, ""))


class _Pending:
    __slots__ = ("future", "method", "on_result")

    def __init__(
        self,
        future: asyncio.Future[Any],
        method: str,
        on_result: Callable[[Any], Any] | None,
    ) -> None:
        self.future = future
        self.method = method
        # Runs inside the reader task, before any later message is processed.
        self.on_result = on_result


class Subscription:
    """A stream of events from one ``subscribe`` call.

    Use as an async context manager (``unsubscribe`` is sent on exit) and as
    an async iterator of :class:`~phonetopc.models.Event`::

        async with phone.subscribe("gyro", rate_hz=100) as events:
            async for ev in events:
                ...

    Iteration ends when the subscription or the connection is closed;
    ``closed_reason`` then says why. On ``on_change`` overflow (see
    :data:`Overflow`) iteration raises
    :class:`~phonetopc.errors.SubscriptionOverflow` after the queued events.
    """

    def __init__(
        self,
        phone: Phone,
        source: str,
        *,
        type: str | None = None,
        rate_hz: float | None = None,
        min_delta: float | None = None,
        queue_size: int = DEFAULT_QUEUE_SIZE,
        overflow: Overflow = "auto",
    ) -> None:
        if queue_size < 1:
            raise ValueError("queue_size must be >= 1")
        if overflow not in ("auto", "drop_oldest", "error"):
            raise ValueError(f"invalid overflow policy {overflow!r}")
        self._phone = phone
        self.source = source
        self.type = type
        self.requested_rate_hz = rate_hz
        self.min_delta = min_delta
        self.queue_size = queue_size
        self._overflow_arg: Overflow = overflow
        self.overflow: Overflow = overflow
        #: Server's answer to ``subscribe``; ``None`` until started.
        self.info: SubscribeResult | None = None
        #: Events the server skipped (``seq`` gaps).
        self.dropped_remote = 0
        #: Events discarded locally because the queue was full.
        self.dropped_local = 0
        #: Events received from the server for this subscription.
        self.received = 0
        #: Why iteration ended (``None`` while open or after a normal close).
        self.closed_reason: BaseException | None = None
        self._queue: collections.deque[Event] = collections.deque()
        self._has_data: asyncio.Event | None = None
        self._last_seq: int | None = None
        self._started = False
        self._ended = False
        self._raise: BaseException | None = None
        self._closing = False

    # -- public API -------------------------------------------------------

    @property
    def id(self) -> str | None:
        """Server-assigned subscription id (``None`` until started)."""
        return self.info.subscription if self.info else None

    @property
    def rate_hz(self) -> float | None:
        """Effective rate reported by the server, if any."""
        return self.info.rate_hz if self.info else None

    @property
    def mode(self) -> str | None:
        """Sensor mode (``"continuous"`` / ``"on_change"``); ``None`` until started.

        Taken from the ``subscribe`` result. Draft 0 servers didn't send it;
        then it is inferred from ``rate_hz``, which servers report for
        continuous sensors only.
        """
        if self.info is None:
            return None
        if self.info.mode is not None:
            return self.info.mode
        return CONTINUOUS if self.info.rate_hz is not None else ON_CHANGE

    @property
    def dropped(self) -> int:
        """Total events lost: server-side (``seq`` gaps) plus local queue overflow."""
        return self.dropped_remote + self.dropped_local

    @property
    def closed(self) -> bool:
        return self._ended

    def qsize(self) -> int:
        """Number of events waiting to be consumed."""
        return len(self._queue)

    async def start(self) -> Subscription:
        """Send ``subscribe``. Called by ``async with``; returns ``self``."""
        if self._started:
            raise RuntimeError("subscription already started")
        self._started = True
        self._has_data = asyncio.Event()
        params: dict[str, Any] = {"source": self.source}
        if self.type is not None:
            params["type"] = self.type
        if self.requested_rate_hz is not None:
            params["rate_hz"] = self.requested_rate_hz
        if self.min_delta is not None:
            params["min_delta"] = self.min_delta
        await self._phone._call("subscribe", params, on_result=self._on_subscribed)
        return self

    async def close(self) -> None:
        """Send ``unsubscribe`` (if still connected) and end iteration."""
        if self._closing:
            return
        self._closing = True
        sub_id = self.id
        try:
            if sub_id is not None and not self._ended and not self._phone.closed:
                await self._phone._unsubscribe(sub_id)
        finally:
            self._phone._detach(self)
            self._queue.clear()
            self._end(None)

    async def __aenter__(self) -> Subscription:
        return await self.start()

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    def __aiter__(self) -> AsyncIterator[Event]:
        return self

    async def __anext__(self) -> Event:
        if not self._started:
            raise RuntimeError("subscription not started; use `async with phone.subscribe(...)`")
        assert self._has_data is not None
        while not self._queue:
            if self._ended:
                if self._raise is not None:
                    raise self._raise
                raise StopAsyncIteration
            self._has_data.clear()
            await self._has_data.wait()
        return self._queue.popleft()

    def __repr__(self) -> str:
        return (
            f"<Subscription {self.id or '?'} source={self.source!r} "
            f"dropped={self.dropped} queued={len(self._queue)}"
            f"{' closed' if self._ended else ''}>"
        )

    # -- called by Phone (inside the reader task) --------------------------

    def _on_subscribed(self, result: Any) -> SubscribeResult:
        info = SubscribeResult.from_json(result)
        self.info = info
        if self._overflow_arg == "auto":
            self.overflow = "drop_oldest" if self.mode == CONTINUOUS else "error"
        self._phone._attach(self)
        return info

    def _push(self, ev: Event) -> None:
        if self._ended:
            return
        self.received += 1
        seq = ev.seq if ev.seq is not None else 0
        if self._last_seq is None:
            # seq starts at 0 (PROTOCOL.md section 5), so a first seq of N means
            # N events were already dropped.
            if seq > 0:
                self.dropped_remote += seq
        elif seq > self._last_seq + 1:
            self.dropped_remote += seq - self._last_seq - 1
        # seq <= last (duplicate / server restart): just resync.
        self._last_seq = seq

        if len(self._queue) >= self.queue_size:
            self.dropped_local += 1
            if self.overflow == "drop_oldest":
                self._queue.popleft()
            else:
                self._phone._detach(self)
                self._end(
                    SubscriptionOverflow(
                        f"subscription {self.id} ({self.source}) overflowed its queue of "
                        f"{self.queue_size} events"
                    ),
                    raise_it=True,
                )
                if self.id is not None:
                    self._phone._spawn(self._phone._unsubscribe_quietly(self.id))
                return
        self._queue.append(ev)
        if self._has_data is not None:
            self._has_data.set()

    def _end(self, reason: BaseException | None, *, raise_it: bool = False) -> None:
        if self._ended:
            return
        self._ended = True
        self.closed_reason = reason
        if raise_it:
            self._raise = reason
        if self._has_data is not None:
            self._has_data.set()


class Phone:
    """A connected phone (or ``phonetopcd``). Create it with :func:`connect`."""

    def __init__(
        self,
        ws: ClientConnection,
        *,
        request_timeout: float | None = DEFAULT_REQUEST_TIMEOUT,
        queue_size: int = DEFAULT_QUEUE_SIZE,
    ) -> None:
        self._ws = ws
        self.url: str | None = None
        self.request_timeout = request_timeout
        self.queue_size = queue_size
        self._ids = itertools.count(1)
        self._pending: dict[int, _Pending] = {}
        self._subs: dict[str, Subscription] = {}
        self._tasks: set[asyncio.Task[Any]] = set()
        self._reader: asyncio.Task[None] | None = None
        self._closed = False
        self._closed_event = asyncio.Event()
        self._hello: HelloResult | None = None
        #: Why the connection ended (``None`` while open).
        self.closed_reason: ConnectionClosed | None = None

    # -- handshake info ---------------------------------------------------

    @property
    def hello(self) -> HelloResult:
        if self._hello is None:
            raise RuntimeError("hello has not completed")
        return self._hello

    @property
    def device(self) -> DeviceInfo:
        """:class:`~phonetopc.models.DeviceInfo` from ``hello``."""
        return self.hello.device

    @property
    def server(self) -> ServerInfo:
        """:class:`~phonetopc.models.ServerInfo` from ``hello``."""
        return self.hello.server

    @property
    def capabilities(self) -> tuple[str, ...]:
        return self.hello.capabilities

    @property
    def closed(self) -> bool:
        return self._closed

    # -- requests ---------------------------------------------------------

    async def call(
        self, method: str, params: Mapping[str, Any] | None = None, *, timeout: Any = _UNSET
    ) -> Any:
        """Send a raw JSON-RPC request and return its ``result``.

        Raises a :class:`~phonetopc.errors.PhonetopcError` subclass on an error
        response. ``timeout`` (seconds, ``None`` = forever) defaults to
        ``request_timeout``.
        """
        return await self._call(method, params, timeout=timeout)

    async def list_sensors(self) -> list[SensorInfo]:
        """``list_sensors``: what this phone has. Unknown sources are included."""
        result = await self._call("list_sensors")
        if not isinstance(result, dict) or not isinstance(result.get("sensors"), list):
            raise ProtocolError(f"list_sensors: malformed result {result!r}")
        return [SensorInfo.from_json(s) for s in result["sensors"]]

    async def get(self, source: str, type: str | None = None) -> Event:
        """``get``: one-shot read of the latest value of ``source``."""
        params: dict[str, Any] = {"source": source}
        if type is not None:
            params["type"] = type
        result = await self._call("get", params)
        return Event.from_json(result, notification=False)

    def subscribe(
        self,
        source: str,
        *,
        type: str | None = None,
        rate_hz: float | None = None,
        min_delta: float | None = None,
        queue_size: int | None = None,
        overflow: Overflow = "auto",
    ) -> Subscription:
        """Prepare a subscription; ``async with`` it to start receiving events.

        ``queue_size`` bounds the local event queue (default: the phone's
        ``queue_size``); ``overflow`` says what happens when it's full.
        """
        return Subscription(
            self,
            source,
            type=type,
            rate_hz=rate_hz,
            min_delta=min_delta,
            queue_size=queue_size if queue_size is not None else self.queue_size,
            overflow=overflow,
        )

    # -- lifecycle --------------------------------------------------------

    async def close(self) -> None:
        """Close the connection. Pending calls fail, subscriptions end."""
        try:
            await self._ws.close()
        finally:
            if self._reader is not None and self._reader is not asyncio.current_task():
                try:
                    await self._reader
                except asyncio.CancelledError:
                    if not self._reader.cancelled():
                        raise
            self._shutdown(ConnectionClosed("connection closed by client"))
            for task in list(self._tasks):
                task.cancel()

    async def wait_closed(self) -> None:
        """Wait until the connection is closed (by either side)."""
        await self._closed_event.wait()

    async def __aenter__(self) -> Phone:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    def __repr__(self) -> str:
        dev = self._hello.device.name if self._hello else "?"
        state = "closed" if self._closed else "open"
        return f"<Phone {dev!r} {self.url or ''} {state}>"

    # -- internals --------------------------------------------------------

    def _start(self) -> None:
        self._reader = asyncio.ensure_future(self._read_loop())

    async def _handshake(self, client_name: str, client_version: str) -> HelloResult:
        result = await self._call(
            "hello",
            {"protocol": PROTOCOL, "client": {"name": client_name, "version": client_version}},
        )
        hello = HelloResult.from_json(result)
        if hello.protocol != PROTOCOL:
            raise ProtocolError(f"server answered hello with protocol {hello.protocol!r}")
        self._hello = hello
        return hello

    async def _call(
        self,
        method: str,
        params: Mapping[str, Any] | None = None,
        *,
        timeout: Any = _UNSET,
        on_result: Callable[[Any], Any] | None = None,
    ) -> Any:
        if self._closed:
            raise self._closed_error()
        if timeout is _UNSET:
            timeout = self.request_timeout
        req_id = next(self._ids)
        fut: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[req_id] = _Pending(fut, method, on_result)
        msg: dict[str, Any] = {"jsonrpc": "2.0", "id": req_id, "method": method}
        if params is not None:
            msg["params"] = dict(params)
        try:
            try:
                await self._ws.send(json.dumps(msg, separators=(",", ":")))
            except websockets.exceptions.ConnectionClosed as exc:
                raise (self._closed_error() if self._closed else _closed_from(exc)) from exc
            if timeout is None:
                return await fut
            return await asyncio.wait_for(fut, timeout)
        except asyncio.TimeoutError:
            raise RequestTimeout(f"no response to {method!r} within {timeout} s") from None
        finally:
            # Settle the future whatever happened (send failed, timeout,
            # cancellation) so the reader or _shutdown never leaves an
            # exception on it that nobody retrieves.
            if not fut.done():
                fut.cancel()
            elif not fut.cancelled():
                fut.exception()
            # A late "subscribe" response must still reach the reader so it can
            # unsubscribe the orphan; everything else can be forgotten.
            if method != "subscribe":
                self._pending.pop(req_id, None)

    async def _unsubscribe(self, sub_id: str) -> None:
        with contextlib.suppress(UnknownSubscription, ConnectionClosed):
            await self._call("unsubscribe", {"subscription": sub_id})

    async def _unsubscribe_quietly(self, sub_id: str) -> None:
        try:
            await self._unsubscribe(sub_id)
        except Exception as exc:  # background cleanup, nobody to report to
            logger.debug("background unsubscribe of %s failed: %r", sub_id, exc)

    def _spawn(self, coro: Any) -> None:
        if self._closed:
            coro.close()
            return
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    def _attach(self, sub: Subscription) -> None:
        sub_id = sub.id
        assert sub_id is not None
        self._subs[sub_id] = sub

    def _detach(self, sub: Subscription) -> None:
        sub_id = sub.id
        if sub_id is not None and self._subs.get(sub_id) is sub:
            del self._subs[sub_id]

    async def _read_loop(self) -> None:
        reason = ConnectionClosed("connection closed by server")
        try:
            async for raw in self._ws:
                if isinstance(raw, bytes):
                    logger.warning("ignoring binary frame (reserved in phonetopc/1)")
                    continue
                try:
                    msg = json.loads(raw)
                except ValueError:
                    logger.warning("ignoring non-JSON frame: %.200r", raw)
                    continue
                self._handle_message(msg)
        except websockets.exceptions.ConnectionClosed as exc:
            reason = _closed_from(exc)
        except Exception as exc:  # pragma: no cover - defensive
            logger.exception("phonetopc reader crashed")
            reason = ConnectionClosed(f"reader failed: {exc!r}")
            await self._ws.close(1011)
        else:
            # Clean close: report the close frame if we got one.
            rcvd = self._ws.protocol.close_rcvd
            if rcvd is not None:
                detail = f", {rcvd.reason}" if rcvd.reason else ""
                reason = ConnectionClosed(
                    f"connection closed (code {rcvd.code}{detail})",
                    code=rcvd.code,
                    reason=rcvd.reason,
                )
        finally:
            self._shutdown(reason)

    def _handle_message(self, msg: Any) -> None:
        if not isinstance(msg, dict):
            logger.warning("ignoring non-object message: %.200r", msg)
            return
        if "id" not in msg:
            if msg.get("method") == "event":
                self._dispatch_event(msg.get("params"))
            else:
                logger.debug("ignoring notification %r", msg.get("method"))
            return
        if "result" not in msg and "error" not in msg:
            # Server-to-client requests are not defined in phonetopc/1.
            logger.debug("ignoring unexpected request %r", msg.get("method"))
            return
        req_id = msg["id"]
        pending = self._pending.pop(req_id, None) if isinstance(req_id, int) else None
        if pending is None:
            if req_id is None and "error" in msg:
                # JSON-RPC: id null means the server couldn't tell which
                # request failed (e.g. parse error). Nothing to resolve.
                logger.warning("server error without id: %r", msg.get("error"))
            else:
                logger.debug("ignoring response to unknown id %r", req_id)
            return
        self._resolve(pending, msg)

    def _resolve(self, pending: _Pending, msg: Mapping[str, Any]) -> None:
        fut = pending.future
        if fut.done():
            # Caller gave up (timeout / cancellation). If that was a subscribe
            # that succeeded, don't leave the subscription running server-side.
            result = msg.get("result")
            if (
                pending.method == "subscribe"
                and isinstance(result, dict)
                and isinstance(result.get("subscription"), str)
            ):
                self._spawn(self._unsubscribe_quietly(result["subscription"]))
            return
        if "error" in msg:
            fut.set_exception(error_from_response(msg["error"]))
            return
        result = msg["result"]
        if pending.on_result is not None:
            try:
                result = pending.on_result(result)
            except ProtocolError as exc:
                fut.set_exception(exc)
                return
        fut.set_result(result)

    def _dispatch_event(self, params: Any) -> None:
        try:
            ev = Event.from_json(params)
        except ProtocolError as exc:
            logger.warning("ignoring malformed event: %s", exc)
            return
        sub_id = ev.subscription
        assert sub_id is not None
        sub = self._subs.get(sub_id)
        if sub is not None:
            sub._push(ev)
            return
        # Not (or no longer) ours: a subscription being unsubscribed (PROTOCOL.md
        # 4.4 says to discard these) or an orphan from a timed-out subscribe.
        # No early-event buffer is needed: the server sends the subscribe
        # response before the first event (4.3), and the response attaches the
        # subscription inside this reader task before the next frame is read.
        # Should a non-conforming server send events first anyway, they are
        # lost here and show up as a seq gap (dropped_remote).
        logger.debug("ignoring event for unknown subscription %r", sub_id)

    def _closed_error(self) -> ConnectionClosed:
        """A fresh copy of ``closed_reason`` (exceptions shouldn't be re-raised
        from several places: their tracebacks and causes would pile up)."""
        r = self.closed_reason
        if r is None:
            return ConnectionClosed()
        return ConnectionClosed(str(r), code=r.code, reason=r.reason)

    def _shutdown(self, reason: ConnectionClosed) -> None:
        if self._closed:
            return
        self._closed = True
        self.closed_reason = reason
        pending, self._pending = self._pending, {}
        for p in pending.values():
            if not p.future.done():
                p.future.set_exception(self._closed_error())
        subs, self._subs = self._subs, {}
        for sub in subs.values():
            sub._end(self._closed_error())
        self._closed_event.set()


def _closed_from(exc: websockets.exceptions.ConnectionClosed) -> ConnectionClosed:
    frame = exc.rcvd or exc.sent
    if frame is None:
        return ConnectionClosed("connection lost (no close frame)")
    detail = f"code {frame.code}" + (f", {frame.reason}" if frame.reason else "")
    return ConnectionClosed(f"connection closed ({detail})", code=frame.code, reason=frame.reason)


class _Connect:
    """Return value of :func:`connect`: awaitable *and* async context manager."""

    def __init__(self, url: str, kwargs: dict[str, Any], ws_kwargs: dict[str, Any]) -> None:
        self._url = url
        self._kwargs = kwargs
        self._ws_kwargs = ws_kwargs
        self._phone: Phone | None = None

    async def _open(self) -> Phone:
        kw = self._kwargs
        ws = await _ws_connect(self._url, open_timeout=kw["open_timeout"], **self._ws_kwargs)
        phone = Phone(ws, request_timeout=kw["request_timeout"], queue_size=kw["queue_size"])
        phone.url = self._url
        phone._start()
        try:
            await phone._handshake(kw["client_name"], kw["client_version"])
        except BaseException:
            await phone.close()
            raise
        return phone

    def __await__(self) -> Generator[Any, None, Phone]:
        return self._open().__await__()

    async def __aenter__(self) -> Phone:
        self._phone = await self._open()
        return self._phone

    async def __aexit__(self, *exc: object) -> None:
        if self._phone is not None:
            await self._phone.close()


def connect(
    addr: str | None = None,
    *,
    client_name: str = "phonetopc-python",
    client_version: str = __version__,
    request_timeout: float | None = DEFAULT_REQUEST_TIMEOUT,
    open_timeout: float | None = 10.0,
    queue_size: int = DEFAULT_QUEUE_SIZE,
    **ws_kwargs: Any,
) -> _Connect:
    """Connect to a phone and perform the ``hello`` handshake.

    Use either ``async with connect(...) as phone:`` or
    ``phone = await connect(...)`` (then call ``await phone.close()``).

    ``addr`` is ``host[:port]`` or a ``ws://`` URL; see :func:`resolve_url`.
    Extra keyword arguments go to :func:`websockets.asyncio.client.connect`.

    Raises :class:`OSError` / :class:`TimeoutError` if the socket can't be
    opened, :class:`~phonetopc.errors.UnsupportedProtocol` if the server
    doesn't speak ``phonetopc/1``.
    """
    url = resolve_url(addr)
    kwargs: dict[str, Any] = {
        "client_name": client_name,
        "client_version": client_version,
        "request_timeout": request_timeout,
        "open_timeout": open_timeout,
        "queue_size": queue_size,
    }
    return _Connect(url, kwargs, ws_kwargs)
