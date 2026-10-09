"""Typed views of protocol objects (PROTOCOL.md sections 4-6).

Parsers are lenient in the direction the spec asks for: unknown fields are
ignored (kept in ``raw``), but required fields must be present with the right
JSON type, otherwise :class:`~phonetopc.errors.ProtocolError` is raised.
"""

from __future__ import annotations

import datetime as _dt
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

from .errors import ProtocolError

__all__ = [
    "CONTINUOUS",
    "ON_CHANGE",
    "DeviceInfo",
    "Event",
    "HelloResult",
    "PCInfo",
    "SensorInfo",
    "ServerInfo",
    "SubscribeResult",
]

CONTINUOUS = "continuous"
ON_CHANGE = "on_change"


# --- small validation helpers -------------------------------------------------


def _obj(value: Any, what: str) -> Mapping[str, Any]:
    if not isinstance(value, dict):
        raise ProtocolError(f"{what}: expected a JSON object, got {type(value).__name__}")
    return value


def _str(obj: Mapping[str, Any], key: str, what: str) -> str:
    value = obj.get(key)
    if not isinstance(value, str):
        raise ProtocolError(f"{what}: field {key!r} must be a string, got {value!r}")
    return value


def _opt_str(obj: Mapping[str, Any], key: str, what: str) -> str | None:
    if obj.get(key) is None:
        return None
    return _str(obj, key, what)


def _int(obj: Mapping[str, Any], key: str, what: str) -> int:
    value = obj.get(key)
    if not isinstance(value, int) or isinstance(value, bool):
        raise ProtocolError(f"{what}: field {key!r} must be an integer, got {value!r}")
    return value


def _opt_num(obj: Mapping[str, Any], key: str, what: str) -> float | None:
    value = obj.get(key)
    if value is None:
        return None
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise ProtocolError(f"{what}: field {key!r} must be a number, got {value!r}")
    return float(value)


def _str_list(obj: Mapping[str, Any], key: str, what: str) -> tuple[str, ...]:
    value = obj.get(key)
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value):
        raise ProtocolError(f"{what}: field {key!r} must be a list of strings, got {value!r}")
    return tuple(value)


# --- dataclasses --------------------------------------------------------------


@dataclass(frozen=True)
class DeviceInfo:
    """The phone, as described by ``hello`` (``result.device``)."""

    id: str
    name: str
    platform: str
    manufacturer: str | None = None
    model: str | None = None
    platform_version: str | None = None
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False, compare=False)

    @classmethod
    def from_json(cls, data: Any) -> DeviceInfo:
        w = "hello.device"
        obj = _obj(data, w)
        return cls(
            id=_str(obj, "id", w),
            name=_str(obj, "name", w),
            platform=_str(obj, "platform", w),
            manufacturer=_opt_str(obj, "manufacturer", w),
            model=_opt_str(obj, "model", w),
            platform_version=_opt_str(obj, "platform_version", w),
            raw=obj,
        )


@dataclass(frozen=True)
class ServerInfo:
    """``hello`` ``result.server``."""

    name: str
    version: str

    @classmethod
    def from_json(cls, data: Any) -> ServerInfo:
        w = "hello.server"
        obj = _obj(data, w)
        return cls(name=_str(obj, "name", w), version=_str(obj, "version", w))


@dataclass(frozen=True)
class PCInfo:
    """``hello`` ``result.pc`` (secure mode): the pairing the phone recognised."""

    id: str
    name: str

    @classmethod
    def from_json(cls, data: Any) -> PCInfo:
        w = "hello.pc"
        obj = _obj(data, w)
        return cls(id=_str(obj, "id", w), name=_str(obj, "name", w))


@dataclass(frozen=True)
class HelloResult:
    """Result of the ``hello`` handshake.

    ``pc`` is set in secure mode only (SECURITY.md section 5).
    """

    protocol: str
    server: ServerInfo
    device: DeviceInfo
    capabilities: tuple[str, ...]
    pc: PCInfo | None = None
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False, compare=False)

    @classmethod
    def from_json(cls, data: Any) -> HelloResult:
        w = "hello result"
        obj = _obj(data, w)
        return cls(
            protocol=_str(obj, "protocol", w),
            server=ServerInfo.from_json(obj.get("server")),
            device=DeviceInfo.from_json(obj.get("device")),
            capabilities=_str_list(obj, "capabilities", w),
            pc=PCInfo.from_json(obj["pc"]) if obj.get("pc") is not None else None,
            raw=obj,
        )


@dataclass(frozen=True)
class SensorInfo:
    """One entry of ``list_sensors``."""

    source: str
    types: tuple[str, ...]
    unit: str
    shape: str
    mode: str
    available: bool
    min_rate_hz: float | None = None
    max_rate_hz: float | None = None
    default_rate_hz: float | None = None
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False, compare=False)

    @property
    def continuous(self) -> bool:
        return self.mode == CONTINUOUS

    @classmethod
    def from_json(cls, data: Any) -> SensorInfo:
        w = "sensor info"
        obj = _obj(data, w)
        available = obj.get("available")
        if not isinstance(available, bool):
            raise ProtocolError(f"{w}: field 'available' must be a boolean, got {available!r}")
        # shape and mode are enums in the schema, but new values may appear in
        # later drafts, so they're kept as plain strings here.
        return cls(
            source=_str(obj, "source", w),
            types=_str_list(obj, "types", w),
            unit=_str(obj, "unit", w),
            shape=_str(obj, "shape", w),
            mode=_str(obj, "mode", w),
            available=available,
            min_rate_hz=_opt_num(obj, "min_rate_hz", w),
            max_rate_hz=_opt_num(obj, "max_rate_hz", w),
            default_rate_hz=_opt_num(obj, "default_rate_hz", w),
            raw=obj,
        )


@dataclass(frozen=True)
class SubscribeResult:
    """Result of ``subscribe``.

    ``mode`` is ``"continuous"`` or ``"on_change"`` (copied by the server from
    ``list_sensors``). Draft 1 requires it; it is ``None`` only when talking to
    an older (draft 0) server that didn't send it.
    """

    subscription: str
    source: str
    types: tuple[str, ...]
    mode: str | None = None
    rate_hz: float | None = None

    @classmethod
    def from_json(cls, data: Any) -> SubscribeResult:
        w = "subscribe result"
        obj = _obj(data, w)
        return cls(
            subscription=_str(obj, "subscription", w),
            source=_str(obj, "source", w),
            types=_str_list(obj, "types", w),
            # An enum in the schema; kept as a plain string like SensorInfo.mode.
            mode=_opt_str(obj, "mode", w),
            rate_hz=_opt_num(obj, "rate_hz", w),
        )


@dataclass(frozen=True)
class Event:
    """A sensor reading.

    Comes from an ``event`` notification, or from ``get`` (then
    ``subscription`` and ``seq`` are ``None``).
    """

    source: str
    type: str
    value: Any
    unit: str
    ts: int
    """Wall-clock Unix time in milliseconds."""
    ts_mono_ns: int
    """Monotonic nanoseconds; only differences from the same device are meaningful."""
    device: str
    subscription: str | None = None
    seq: int | None = None

    @property
    def datetime(self) -> _dt.datetime:
        """``ts`` as a timezone-aware UTC :class:`datetime.datetime`."""
        return _dt.datetime.fromtimestamp(self.ts / 1000.0, tz=_dt.timezone.utc)

    @classmethod
    def from_json(cls, data: Any, *, notification: bool = True) -> Event:
        """Parse event ``params`` (``notification=True``) or a ``get`` result."""
        w = "event" if notification else "get result"
        obj = _obj(data, w)
        if "value" not in obj:
            raise ProtocolError(f"{w}: missing field 'value'")
        subscription: str | None = None
        seq: int | None = None
        if notification:
            subscription = _str(obj, "subscription", w)
            seq = _int(obj, "seq", w)
        return cls(
            source=_str(obj, "source", w),
            type=_str(obj, "type", w),
            value=obj["value"],
            unit=_str(obj, "unit", w),
            ts=_int(obj, "ts", w),
            ts_mono_ns=_int(obj, "ts_mono_ns", w),
            device=_str(obj, "device", w),
            subscription=subscription,
            seq=seq,
        )
