"""phonetopc: stream Android phone sensor events to your PC.

Pair the phone once with ``phonetopc pair`` (the Go CLI), then::

    import asyncio
    import phonetopc

    async def main():
        async with phonetopc.connect(client_name="demo") as phone:  # pinned TLS + token
            print(phone.device)
            async with phone.subscribe("gyro", rate_hz=100) as events:
                async for ev in events:
                    print(ev.seq, ev.value)

    asyncio.run(main())

For a phone in dev mode (plaintext, no pairing) use
``phonetopc.connect("192.168.1.20", dev=True)``. Scripts that don't want
asyncio can use :mod:`phonetopc.sync`.

``await phone.request_approval("Run deploy?")`` asks for a fingerprint on the
phone and verifies the phone's signature (``pip install 'phonetopc[approval]'``).
"""

from __future__ import annotations

from ._version import __version__
from .approval import ApprovalResult
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
    AmbiguousDevice,
    ApprovalDependencyMissing,
    ApprovalError,
    ApprovalKeyChanged,
    ApprovalNotSupported,
    ApprovalVerificationError,
    ConnectionClosed,
    CredentialStoreError,
    Error,
    HelloRequired,
    InternalError,
    InvalidParams,
    InvalidRequest,
    MethodNotFound,
    NotPaired,
    NoValue,
    ParseError,
    PhonetopcError,
    PhoneUnreachable,
    PinMismatch,
    ProtocolError,
    RequestTimeout,
    SourceUnavailable,
    SubscriptionOverflow,
    Unauthorized,
    UnknownSource,
    UnknownSubscription,
    UnsupportedProtocol,
)
from .models import (
    CONTINUOUS,
    ON_CHANGE,
    ApprovalInfo,
    DeviceInfo,
    Event,
    HelloResult,
    PCInfo,
    SensorInfo,
    ServerInfo,
    SubscribeResult,
)
from .store import InsecurePermissionsWarning, PairedDevice, load_devices

__all__ = [
    "CONTINUOUS",
    "DEFAULT_ADDR",
    "DEFAULT_PATH",
    "DEFAULT_PORT",
    "ENV_ADDR",
    "ON_CHANGE",
    "PROTOCOL",
    "AmbiguousDevice",
    "ApprovalDependencyMissing",
    "ApprovalError",
    "ApprovalInfo",
    "ApprovalKeyChanged",
    "ApprovalNotSupported",
    "ApprovalResult",
    "ApprovalVerificationError",
    "ConnectionClosed",
    "CredentialStoreError",
    "DeviceInfo",
    "Error",
    "Event",
    "HelloRequired",
    "HelloResult",
    "InsecurePermissionsWarning",
    "InternalError",
    "InvalidParams",
    "InvalidRequest",
    "MethodNotFound",
    "NoValue",
    "NotPaired",
    "Overflow",
    "PCInfo",
    "PairedDevice",
    "ParseError",
    "Phone",
    "PhoneUnreachable",
    "PhonetopcError",
    "PinMismatch",
    "ProtocolError",
    "RequestTimeout",
    "SensorInfo",
    "ServerInfo",
    "SourceUnavailable",
    "SubscribeResult",
    "Subscription",
    "SubscriptionOverflow",
    "Unauthorized",
    "UnknownSource",
    "UnknownSubscription",
    "UnsupportedProtocol",
    "__version__",
    "connect",
    "load_devices",
    "resolve_url",
]
