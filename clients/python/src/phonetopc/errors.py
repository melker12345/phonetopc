"""Exception hierarchy.

Two bases matter when catching:

* :class:`Error` is the root of *everything* this package raises: server
  error replies, lost connections, timeouts, pairing and TLS-pin problems.
  Catch it for "anything phonetopc went wrong".
* :class:`ServerError` (a subclass of :class:`Error`) is only for JSON-RPC
  error *responses*: the server understood the request and said no. The
  concrete subclass is picked by error code (PROTOCOL.md section 7), e.g.
  :class:`UnknownSource` or :class:`NotAllowed`. Catch it when the phone
  refusing a request is expected and recoverable.

``PhonetopcError`` is the older name of :class:`ServerError`, kept as an alias
so existing ``except phonetopc.PhonetopcError`` clauses keep working.
"""

from __future__ import annotations

from typing import Any

__all__ = [
    "AmbiguousDevice",
    "ApprovalDependencyMissing",
    "ApprovalError",
    "ApprovalKeyChanged",
    "ApprovalNotSupported",
    "ApprovalVerificationError",
    "ConnectionClosed",
    "CredentialStoreError",
    "Error",
    "HelloRequired",
    "InternalError",
    "InvalidParams",
    "InvalidRequest",
    "MethodNotFound",
    "NoValue",
    "NotAllowed",
    "NotPaired",
    "ParseError",
    "PhoneUnreachable",
    "PhonetopcError",
    "PinMismatch",
    "ProtocolError",
    "RateLimited",
    "RequestTimeout",
    "ServerError",
    "SourceUnavailable",
    "SubscriptionOverflow",
    "TooManyPanels",
    "Unauthorized",
    "UnknownPanel",
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


# -- secure connections (SECURITY.md) ------------------------------------------


class PinMismatch(Error):
    """The phone's TLS key doesn't match the pin stored when it was paired.

    Either the phone app was reinstalled or reset (new key: pair again), or
    something else is answering at that address. Nothing was sent to it.

    When several addresses were tried, this is raised only if none of them
    worked (SECURITY.md section 6); ``address``/``actual`` describe the first
    mismatch and ``attempts`` lists ``(address, error)`` for every address tried.
    """

    def __init__(
        self,
        device: str,
        address: str,
        expected: str,
        actual: str,
        attempts: list[tuple[str, BaseException]] | None = None,
    ) -> None:
        super().__init__(
            f"phone identity changed: the phone at {address} is not the {device!r} you paired "
            f"(expected key {expected}, got {actual}). If you reinstalled or reset the phone "
            "app, re-pair with `phonetopc pair`; otherwise something else is answering at that "
            "address. Nothing was sent to it."
        )
        self.device = device
        self.address = address
        self.expected = expected
        self.actual = actual
        self.attempts: list[tuple[str, BaseException]] = attempts or []


class NotPaired(Error, LookupError):
    """No pairing matches what was asked for (or there is none at all)."""


class AmbiguousDevice(Error, LookupError):
    """Several paired phones could be meant; pass ``device=``."""


class CredentialStoreError(Error):
    """``devices.json`` is unreadable, malformed or of an unknown version."""


class PhoneUnreachable(Error, OSError):
    """A paired phone couldn't be reached at any known address.

    ``attempts`` lists ``(address, error)`` for every address that was tried.
    """

    def __init__(self, message: str, attempts: list[tuple[str, BaseException]]) -> None:
        super().__init__(message)
        self.attempts = attempts


# -- fingerprint approval (APPROVAL.md) -------------------------------------------


class ApprovalError(Error):
    """Base class of approval problems that are not a plain "no" from the user."""


class ApprovalNotSupported(ApprovalError):
    """Approval needs a secure connection to a paired phone; dev mode has none."""


class ApprovalKeyChanged(ApprovalError):
    """The phone's approval key isn't the one accepted before (APPROVAL.md section 3).

    Usually fingerprints were added or removed on the phone, which replaces
    its approval key. The request was not sent. Trust the new key explicitly
    with ``phonetopc approval-key --accept``.
    """

    def __init__(self, device: str, stored: str, actual: str) -> None:
        super().__init__(
            f"the approval key of {device} changed (accepted {stored}, the phone now has "
            f"{actual}). Fingerprints changed on the phone? run `phonetopc approval-key "
            "--accept` (or `python -m phonetopc approval-key --accept`) to trust the new key. "
            "The approval request was not sent."
        )
        self.device = device
        self.stored = stored
        self.actual = actual


class ApprovalVerificationError(ApprovalError):
    """The phone answered ``approved: true``, but without a valid signature.

    This is a failure, never an approval (APPROVAL.md section 4.1).
    """


class ApprovalDependencyMissing(Error, ImportError):
    """Verifying approvals needs the ``cryptography`` package (``phonetopc[approval]``)."""


class ServerError(Error):
    """A JSON-RPC error response from the server (the request was refused).

    Only error *replies* are ``ServerError``; connection, timeout, pairing and
    other local failures are other :class:`Error` subclasses. ``code``,
    ``message`` and ``data`` are the reply's ``error`` members. Also available
    as ``PhonetopcError``, its original name.
    """

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


#: Original name of :class:`ServerError`, kept for backward compatibility.
PhonetopcError = ServerError


# Standard JSON-RPC 2.0 errors.


class ParseError(ServerError):
    CODE = -32700
    NAME = "parse_error"


class InvalidRequest(ServerError):
    CODE = -32600
    NAME = "invalid_request"


class MethodNotFound(ServerError):
    CODE = -32601
    NAME = "method_not_found"


class InvalidParams(ServerError):
    CODE = -32602
    NAME = "invalid_params"


class InternalError(ServerError):
    CODE = -32603
    NAME = "internal_error"


# phonetopc application errors (PROTOCOL.md section 7).


class UnknownSource(ServerError):
    """``source`` is not in the server's catalog."""

    CODE = -32001
    NAME = "unknown_source"


class SourceUnavailable(ServerError):
    """Known source, but absent or disabled on this device."""

    CODE = -32002
    NAME = "source_unavailable"


class HelloRequired(ServerError):
    """A request was sent before a successful ``hello``."""

    CODE = -32003
    NAME = "hello_required"


class UnsupportedProtocol(ServerError):
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


class UnknownSubscription(ServerError):
    """``unsubscribe`` with an id that isn't in this session."""

    CODE = -32005
    NAME = "unknown_subscription"


class NoValue(ServerError):
    """``get`` has nothing to return yet."""

    CODE = -32006
    NAME = "no_value"


class Unauthorized(ServerError):
    """Secure mode: the phone doesn't accept this PC's token (``-32010``).

    The PC was revoked on the phone, or the phone forgot the pairing. The
    phone closes the connection right after this error.
    """

    CODE = -32010
    NAME = "unauthorized"

    def __init__(self, code: int, message: str, data: Any = None) -> None:
        super().__init__(code, message, data)
        self.args = (
            f"{message} (code {code}): this PC was revoked on the phone, or the phone no "
            "longer knows it. Run `phonetopc pair` to pair again.",
        )


# Notifications and panels (PANELS.md section 9).


class NotAllowed(ServerError):
    """The user hasn't allowed notifications or panels for this PC (``-32030``).

    ``permission`` is ``"notify"`` or ``"panels"``. Both are off by default:
    turn them on for this PC in the phone app (*Notifications*, *Screen panels*).
    """

    CODE = -32030
    NAME = "not_allowed"

    def __init__(self, code: int, message: str, data: Any = None) -> None:
        super().__init__(code, message, data)
        setting = {"notify": "Notifications", "panels": "Screen panels"}.get(
            self.permission or "", "Notifications / Screen panels"
        )
        self.args = (
            f"{message} (code {code}): turn on {setting} for this PC in the phone app "
            "(both are off by default)",
        )

    @property
    def permission(self) -> str | None:
        """``data.permission``: ``"notify"`` or ``"panels"``."""
        if isinstance(self.data, dict) and isinstance(self.data.get("permission"), str):
            return str(self.data["permission"])
        return None


class UnknownPanel(ServerError):
    """No panel of this PC has that id: never created, closed, or expired (``-32031``)."""

    CODE = -32031
    NAME = "unknown_panel"


class TooManyPanels(ServerError):
    """``panel_show`` of a new panel when the PC already has 8 (``-32032``)."""

    CODE = -32032
    NAME = "too_many_panels"


class RateLimited(ServerError):
    """``notify`` above the phone's rate limit (``-32033``)."""

    CODE = -32033
    NAME = "rate_limited"


_BY_CODE: dict[int, type[ServerError]] = {}
_BY_NAME: dict[str, type[ServerError]] = {}
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
    Unauthorized,
    NotAllowed,
    UnknownPanel,
    TooManyPanels,
    RateLimited,
):
    assert _cls.CODE is not None and _cls.NAME is not None
    _BY_CODE[_cls.CODE] = _cls
    _BY_NAME[_cls.NAME] = _cls


def error_from_response(error: Any) -> ServerError:
    """Build the right exception from a JSON-RPC ``error`` object.

    The numeric code wins; ``data.name`` is only used when the code is unknown,
    so a future server can introduce new codes without breaking ``except``
    clauses that match on the name.
    """
    if not isinstance(error, dict):
        return ServerError(-32603, f"malformed error object: {error!r}")
    raw_code = error.get("code")
    code = raw_code if isinstance(raw_code, int) and not isinstance(raw_code, bool) else -32603
    message = error.get("message")
    if not isinstance(message, str):
        message = "unknown error"
    data = error.get("data")
    cls = _BY_CODE.get(code)
    if cls is None and isinstance(data, dict) and isinstance(data.get("name"), str):
        cls = _BY_NAME.get(data["name"])
    return (cls or ServerError)(code, message, data)
