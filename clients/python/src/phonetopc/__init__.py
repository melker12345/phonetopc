"""phonetopc: stream Android phone sensor events to your PC.

Quickstart::

    import asyncio
    import phonetopc

    async def main():
        async with phonetopc.connect("192.168.1.20:8765", client_name="demo") as phone:
            print(phone.device)
            async with phone.subscribe("gyro", rate_hz=100) as events:
                async for ev in events:
                    print(ev.seq, ev.value)

    asyncio.run(main())

Scripts that don't want asyncio can use :mod:`phonetopc.sync`.
"""

from __future__ import annotations

from ._version import __version__
from .client import (
    DEFAULT_ADDR,
    DEFAULT_PATH,
    DEFAULT_PORT,
    ENV_ADDR,
    PROTOCOL,
    Overflow,
    Phone,
    Subscription,
    connect,
    resolve_url,
)
from .errors import (
    ConnectionClosed,
    Error,
    HelloRequired,
    InternalError,
    InvalidParams,
    InvalidRequest,
    MethodNotFound,
    NoValue,
    ParseError,
    PhonetopcError,
    ProtocolError,
    RequestTimeout,
    SourceUnavailable,
    SubscriptionOverflow,
    UnknownSource,
    UnknownSubscription,
    UnsupportedProtocol,
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
    "CONTINUOUS",
    "DEFAULT_ADDR",
    "DEFAULT_PATH",
    "DEFAULT_PORT",
    "ENV_ADDR",
    "ON_CHANGE",
    "PROTOCOL",
    "ConnectionClosed",
    "DeviceInfo",
    "Error",
    "Event",
    "HelloRequired",
    "HelloResult",
    "InternalError",
    "InvalidParams",
    "InvalidRequest",
    "MethodNotFound",
    "NoValue",
    "Overflow",
    "ParseError",
    "Phone",
    "PhonetopcError",
    "ProtocolError",
    "RequestTimeout",
    "SensorInfo",
    "ServerInfo",
    "SourceUnavailable",
    "SubscribeResult",
    "Subscription",
    "SubscriptionOverflow",
    "UnknownSource",
    "UnknownSubscription",
    "UnsupportedProtocol",
    "__version__",
    "connect",
    "resolve_url",
]
