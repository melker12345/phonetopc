"""Fingerprint approval (spec/APPROVAL.md): the signed message and its verification.

The phone signs, with a biometric-bound ECDSA P-256 key, these UTF-8 lines
joined by ``\\n`` (APPROVAL.md section 5)::

    phonetopc-approval/1
    <phone id>
    <pc id>
    <nonce, base64url as sent>
    <base64url(SHA-256(reason))>

The PC builds the message from *its own* copies of these values and verifies
the signature with the key from ``hello``. ECDSA needs the optional
``cryptography`` package (``pip install 'phonetopc[approval]'``); it is
imported only when an approval is actually requested or verified.

:meth:`phonetopc.Phone.request_approval` is the API most programs want; the
functions here are public for other tools and for tests.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import secrets
import unicodedata
from dataclasses import dataclass, field
from typing import Any

from .errors import ApprovalDependencyMissing

__all__ = [
    "APPROVAL_GRACE",
    "DEFAULT_APPROVAL_TIMEOUT",
    "MAX_APPROVAL_TIMEOUT",
    "MAX_REASON_LENGTH",
    "MESSAGE_PREFIX",
    "MIN_APPROVAL_TIMEOUT",
    "WHY_VALUES",
    "ApprovalResult",
    "approval_message",
    "b64url_decode",
    "b64url_encode",
    "check_reason",
    "new_nonce",
    "require_crypto",
    "verify_signature",
]

MESSAGE_PREFIX = "phonetopc-approval/1"
MAX_REASON_LENGTH = 200
#: Seconds; the phone accepts 5000-120000 ms (APPROVAL.md section 4).
DEFAULT_APPROVAL_TIMEOUT = 30.0
MIN_APPROVAL_TIMEOUT = 5.0
MAX_APPROVAL_TIMEOUT = 120.0
#: The client waits this much longer than ``timeout`` for the answer (section 4: at least 5 s).
APPROVAL_GRACE = 5.0
NONCE_BYTES = 32

#: ``why`` values of APPROVAL.md section 4.1. Newer phones may send others.
WHY_VALUES = ("denied", "cancelled", "timeout", "busy", "lockout", "unavailable")

_INSTALL_HINT = (
    "fingerprint approval needs the 'cryptography' package to verify the phone's signature: "
    "pip install 'phonetopc[approval]'"
)


@dataclass(frozen=True)
class ApprovalResult:
    """Outcome of :meth:`phonetopc.Phone.request_approval`.

    ``approved`` is ``True`` only when the phone's signature was verified;
    ``why`` says why not otherwise (``"denied"``, ``"cancelled"``,
    ``"timeout"``, ``"busy"``, ``"lockout"``, ``"unavailable"``, or a value
    from a newer phone). ``bool(result)`` is ``result.approved``.
    """

    approved: bool
    why: str | None = None
    raw: Any = field(default=None, repr=False, compare=False)

    def __bool__(self) -> bool:
        return self.approved


def b64url_encode(raw: bytes) -> str:
    """base64url without padding."""
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def b64url_decode(text: str) -> bytes:
    """Strict base64url without padding; :class:`ValueError` on anything else."""
    if not isinstance(text, str) or not text.isascii():
        raise ValueError("not a base64url string")
    if any(c in text for c in "+/=") or len(text) % 4 == 1:
        raise ValueError("not unpadded base64url")
    try:
        raw = base64.b64decode(text + "=" * (-len(text) % 4), altchars=b"-_", validate=True)
    except (ValueError, binascii.Error):
        raise ValueError("not unpadded base64url") from None
    if b64url_encode(raw) != text:
        raise ValueError("not canonical base64url")
    return raw


def new_nonce() -> str:
    """32 fresh random bytes, base64url (43 characters)."""
    return b64url_encode(secrets.token_bytes(NONCE_BYTES))


def check_reason(reason: str) -> str:
    """Validate ``reason`` (APPROVAL.md section 4) and return it trimmed.

    1-200 characters after trimming, no control characters. The trimmed text
    is what gets sent and signed, so both sides hash the same bytes. The limit
    is checked in code points and in UTF-16 units (what Kotlin counts), so a
    reason accepted here is never too long for the phone.
    """
    if not isinstance(reason, str):
        raise TypeError(f"reason must be a str, not {type(reason).__name__}")
    text = reason.strip()
    if not text:
        raise ValueError("reason must not be empty")
    utf16_units = len(text.encode("utf-16-le")) // 2
    if max(len(text), utf16_units) > MAX_REASON_LENGTH:
        raise ValueError(f"reason is longer than {MAX_REASON_LENGTH} characters")
    if any(unicodedata.category(c) == "Cc" for c in text):
        raise ValueError("reason must not contain control characters (newlines, tabs, ...)")
    return text


def approval_message(phone_id: str, pc_id: str, nonce: str, reason: str) -> bytes:
    """The exact bytes the phone signs (APPROVAL.md section 5)."""
    parts = [phone_id, pc_id, nonce]
    if any("\n" in p for p in parts):
        raise ValueError("ids and nonce must not contain newlines")
    reason_hash = b64url_encode(hashlib.sha256(reason.encode("utf-8")).digest())
    return "\n".join([MESSAGE_PREFIX, *parts, reason_hash]).encode("utf-8")


def require_crypto() -> None:
    """Raise :class:`~phonetopc.errors.ApprovalDependencyMissing` unless ``cryptography`` works."""
    try:
        from cryptography.hazmat.primitives.asymmetric import ec  # noqa: F401
    except ImportError as exc:
        raise ApprovalDependencyMissing(_INSTALL_HINT, name="cryptography") from exc


def verify_signature(key: str, message: bytes, signature: str) -> bool:
    """Whether ``signature`` (base64url DER ECDSA) is valid for ``message``.

    ``key`` is the base64url DER ``SubjectPublicKeyInfo`` of a P-256 key.
    Anything malformed (encoding, key type, curve, signature) is ``False``.
    """
    require_crypto()
    from cryptography.exceptions import InvalidSignature, UnsupportedAlgorithm
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec

    try:
        spki = b64url_decode(key)
        sig = b64url_decode(signature)
    except ValueError:
        return False
    try:
        public_key = serialization.load_der_public_key(spki)
    except (ValueError, TypeError, UnsupportedAlgorithm):
        return False
    if not isinstance(public_key, ec.EllipticCurvePublicKey) or not isinstance(
        public_key.curve, ec.SECP256R1
    ):
        return False
    try:
        public_key.verify(sig, message, ec.ECDSA(hashes.SHA256()))
    except (InvalidSignature, ValueError):
        return False
    return True
