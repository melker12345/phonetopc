"""Exception hierarchy.

Everything raised by this package derives from :class:`Error`. JSON-RPC error
responses from the server become :class:`PhonetopcError` (or one of its
subclasses, picked by error code; see PROTOCOL.md section 7).
"""

from __future__ import annotations

from typing import Any

__all__ = [
    "ConnectionClosed",
    "Error",
    "HelloRequired",
    "InternalError",
    "InvalidParams",
    "InvalidRequest",
    "MethodNotFound",
    "NoValue",
    "ParseError",
    "PhonetopcError",
    "ProtocolError",
    "RequestTimeout",
    "SourceUnavailable",
    "SubscriptionOverflow",
    "UnknownSource",
    "UnknownSubscription",
    "UnsupportedProtocol",
    "error_from_response",
]


class Error(Exception):
    """Base class of every exception raised by phonetopc."""


class ConnectionClosed(Error):
    """The connection to the server is closed (or was lost).

    Raised by pending and new calls. ``code`` and ``reason`` are the
    WebSocket close code and reason when known.
    """

    def __init__(
        self, message: str = "connection closed", code: int | None = None, reason: str = ""
    ) -> None:
        super().__init__(message)
        self.code = code
        self.reason = reason


class ProtocolError(Error):
    """The server sent something that violates the phonetopc protocol."""


class RequestTimeout(Error, TimeoutError):
    """The server didn't answer a request in time."""


class SubscriptionOverflow(Error):
    """An ``on_change`` subscription's local queue overflowed.

    The protocol forbids dropping ``on_change`` events (PROTOCOL.md section 5),
    so instead of silently dropping, the client ends the subscription with this
    error. Consume events faster or raise ``queue_size``.
    """


class PhonetopcError(Error):
    """A JSON-RPC error response from the server."""

    #: The spec's error code for this class (``None`` on the generic base class).
    CODE: int | None = None
    #: The spec's ``data.name`` for this class.
    NAME: str | None = None

    def __init__(self, code: int, message: str, data: Any = None) -> None:
        super().__init__(f"{message} (code {code})")
        self.code = code
        self.message = message
        self.data = data

    @property
    def name(self) -> str | None:
        """``data.name`` as sent by the server, falling back to the class's spec name."""
        if isinstance(self.data, dict):
            name = self.data.get("name")
            if isinstance(name, str):
                return name
        return self.NAME

    def __repr__(self) -> str:
        return (
            f"{type(self).__name__}(code={self.code!r}, message={self.message!r}, "
            f"data={self.data!r})"
        )


# Standard JSON-RPC 2.0 errors.


class ParseError(PhonetopcError):
    CODE = -32700
    NAME = "parse_error"


class InvalidRequest(PhonetopcError):
    CODE = -32600
    NAME = "invalid_request"


class MethodNotFound(PhonetopcError):
    CODE = -32601
    NAME = "method_not_found"


class InvalidParams(PhonetopcError):
    CODE = -32602
    NAME = "invalid_params"


class InternalError(PhonetopcError):
    CODE = -32603
    NAME = "internal_error"


# phonetopc application errors (PROTOCOL.md section 7).


class UnknownSource(PhonetopcError):
    """``source`` is not in the server's catalog."""

    CODE = -32001
    NAME = "unknown_source"


class SourceUnavailable(PhonetopcError):
    """Known source, but absent or disabled on this device."""

    CODE = -32002
    NAME = "source_unavailable"


class HelloRequired(PhonetopcError):
    """A request was sent before a successful ``hello``."""

    CODE = -32003
    NAME = "hello_required"


class UnsupportedProtocol(PhonetopcError):
    """The server doesn't speak our protocol version."""

    CODE = -32004
    NAME = "unsupported_protocol"

    @property
    def supported(self) -> list[str]:
        """Protocol versions the server says it supports (``data.supported``)."""
        if isinstance(self.data, dict):
            versions = self.data.get("supported")
            if isinstance(versions, list):
                return [str(v) for v in versions]
        return []


class UnknownSubscription(PhonetopcError):
    """``unsubscribe`` with an id that isn't in this session."""

    CODE = -32005
    NAME = "unknown_subscription"


class NoValue(PhonetopcError):
    """``get`` has nothing to return yet."""

    CODE = -32006
    NAME = "no_value"


_BY_CODE: dict[int, type[PhonetopcError]] = {}
_BY_NAME: dict[str, type[PhonetopcError]] = {}
for _cls in (
    ParseError,
    InvalidRequest,
    MethodNotFound,
    InvalidParams,
    InternalError,
    UnknownSource,
    SourceUnavailable,
    HelloRequired,
    UnsupportedProtocol,
    UnknownSubscription,
    NoValue,
):
    assert _cls.CODE is not None and _cls.NAME is not None
    _BY_CODE[_cls.CODE] = _cls
    _BY_NAME[_cls.NAME] = _cls


def error_from_response(error: Any) -> PhonetopcError:
    """Build the right exception from a JSON-RPC ``error`` object.

    The numeric code wins; ``data.name`` is only used when the code is unknown,
    so a future server can introduce new codes without breaking ``except``
    clauses that match on the name.
    """
    if not isinstance(error, dict):
        return PhonetopcError(-32603, f"malformed error object: {error!r}")
    raw_code = error.get("code")
    code = raw_code if isinstance(raw_code, int) and not isinstance(raw_code, bool) else -32603
    message = error.get("message")
    if not isinstance(message, str):
        message = "unknown error"
    data = error.get("data")
    cls = _BY_CODE.get(code)
    if cls is None and isinstance(data, dict) and isinstance(data.get("name"), str):
        cls = _BY_NAME.get(data["name"])
    return (cls or PhonetopcError)(code, message, data)
