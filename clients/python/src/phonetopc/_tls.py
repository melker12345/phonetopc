"""Pinned TLS for secure mode (spec/SECURITY.md sections 2-3).

Nobody validates chains, expiry or host names; trust comes only from the pin.
The pin is checked *inside* the TLS handshake: :class:`PinnedContext` makes
asyncio wrap the connection in :class:`_PinnedSSLObject`, whose
``do_handshake`` compares the peer's pin as soon as the handshake completes and
fails it otherwise, so not a single byte of application data (not even the
WebSocket upgrade request) reaches a phone with the wrong key.
:func:`verify_transport` repeats the check on the finished connection before
``hello`` (and its token) is sent, in case an event loop (uvloop, say) doesn't
go through ``SSLContext.sslobject_class``.
"""

from __future__ import annotations

import ssl
from typing import Any

from .pin import CertificateParseError, pin_from_der, pins_equal

__all__ = ["PinCheckFailed", "PinnedContext", "peer_pin", "pinned_context", "verify_transport"]

#: TLS 1.2 suites allowed besides TLS 1.3's (which are all AEAD): ECDHE + AEAD only.
_TLS12_CIPHERS = "ECDHE+AESGCM:ECDHE+CHACHA20"


class PinCheckFailed(ssl.SSLCertVerificationError):
    """Raised inside the TLS handshake; the client turns it into ``PinMismatch``.

    It is an ``SSLError`` so that asyncio treats it like any failed
    certificate verification and aborts the connection.
    """

    def __init__(self, actual: str) -> None:
        super().__init__(f"server key pin mismatch (got {actual})")
        self.actual = actual


def peer_pin(sslobj: ssl.SSLObject | ssl.SSLSocket) -> str:
    """The pin of the peer's certificate, or a description of why there is none."""
    der = sslobj.getpeercert(binary_form=True)
    if not der:
        return "(no certificate)"
    try:
        return pin_from_der(der)
    except CertificateParseError as exc:
        return f"(unreadable certificate: {exc})"


def _check(sslobj: ssl.SSLObject | ssl.SSLSocket, expected: str) -> None:
    actual = peer_pin(sslobj)
    # A non-pin placeholder never equals a stored pin, which always starts with sha256/.
    if not pins_equal(actual, expected):
        raise PinCheckFailed(actual)


class _PinnedSSLObject(ssl.SSLObject):
    def do_handshake(self) -> None:
        super().do_handshake()  # raises SSLWantReadError until the handshake is done
        ctx = self.context
        if not isinstance(ctx, PinnedContext):  # pragma: no cover - defensive
            raise PinCheckFailed("(no pin configured)")
        _check(self, ctx.expected_pin)


class PinnedContext(ssl.SSLContext):
    """A client context that accepts exactly one public key, by pin."""

    sslobject_class = _PinnedSSLObject
    expected_pin: str = ""


def pinned_context(expected_pin: str) -> PinnedContext:
    ctx = PinnedContext(ssl.PROTOCOL_TLS_CLIENT)
    # No chain, expiry or host name checks (SECURITY.md section 2): the pin is
    # the only trust anchor, checked by _PinnedSSLObject.
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.set_ciphers(_TLS12_CIPHERS)
    ctx.expected_pin = expected_pin
    return ctx


def verify_transport(transport: Any, expected_pin: str) -> None:
    """Check the pin of an established asyncio TLS transport."""
    sslobj = transport.get_extra_info("ssl_object") if transport is not None else None
    if sslobj is None:
        raise PinCheckFailed("(not a TLS connection)")
    _check(sslobj, expected_pin)
