"""Public-key pins (spec/SECURITY.md section 2), with the standard library only.

A pin is ``sha256/`` + base64url (no padding) of SHA-256 over the DER-encoded
``SubjectPublicKeyInfo`` of an X.509 certificate. Python's :mod:`ssl` gives us
the peer certificate as DER but has no way to pull the public key out of it,
so this module carries a small, strict DER walker that does just that: it
follows the fixed outer structure of a certificate (RFC 5280 section 4.1) down
to ``subjectPublicKeyInfo`` and returns its exact bytes. It never interprets
anything else, and every malformed input raises :class:`CertificateParseError`.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import ssl

from .errors import Error

__all__ = [
    "PIN_PREFIX",
    "CertificateParseError",
    "check_pin_format",
    "pin_from_der",
    "pin_from_pem",
    "pin_from_spki",
    "pins_equal",
    "spki_from_der",
]

PIN_PREFIX = "sha256/"

# DER tags (universal class, single byte).
_SEQUENCE = 0x30
_INTEGER = 0x02
_BIT_STRING = 0x03
_CONTEXT_0 = 0xA0  # [0] EXPLICIT, constructed: the certificate version

#: No certificate we pin is anywhere near this; a larger length is garbage.
_MAX_LENGTH = 1 << 24


class CertificateParseError(Error, ValueError):
    """The bytes are not a DER-encoded X.509 certificate this module can read."""


def _read_tlv(data: bytes, pos: int, end: int, what: str) -> tuple[int, int, int]:
    """Read one tag-length-value at ``data[pos:end]``.

    Returns ``(tag, value_start, value_end)``. Only definite, minimally
    encoded (DER) lengths and single-byte tags are accepted.
    """
    if pos >= end:
        raise CertificateParseError(f"{what}: truncated (no tag)")
    tag = data[pos]
    if tag & 0x1F == 0x1F:
        raise CertificateParseError(f"{what}: multi-byte tags are not supported")
    pos += 1
    if pos >= end:
        raise CertificateParseError(f"{what}: truncated (no length)")
    first = data[pos]
    pos += 1
    if first < 0x80:
        length = first
    elif first == 0x80:
        raise CertificateParseError(f"{what}: indefinite length is not DER")
    else:
        n = first & 0x7F
        if n > 4:
            raise CertificateParseError(f"{what}: length of {n} bytes is too long")
        if end - pos < n:
            raise CertificateParseError(f"{what}: truncated (length)")
        length_bytes = data[pos : pos + n]
        pos += n
        if length_bytes[0] == 0:
            raise CertificateParseError(f"{what}: length has a leading zero byte (not DER)")
        length = int.from_bytes(length_bytes, "big")
        if length < 0x80:
            raise CertificateParseError(f"{what}: long-form length below 128 (not DER)")
    if length > _MAX_LENGTH:
        raise CertificateParseError(f"{what}: length {length} is implausibly large")
    if end - pos < length:
        raise CertificateParseError(f"{what}: truncated (needs {length} bytes, {end - pos} left)")
    return tag, pos, pos + length


def _expect(data: bytes, pos: int, end: int, tag: int, what: str) -> tuple[int, int]:
    """Read a TLV that must have ``tag``; return ``(value_start, value_end)``."""
    got, start, stop = _read_tlv(data, pos, end, what)
    if got != tag:
        raise CertificateParseError(f"{what}: expected tag 0x{tag:02x}, got 0x{got:02x}")
    return start, stop


def spki_from_der(cert_der: bytes) -> bytes:
    """Return the DER ``SubjectPublicKeyInfo`` (tag and length included) of a certificate.

    ::

        Certificate  ::= SEQUENCE { tbsCertificate, signatureAlgorithm, signatureValue }
        TBSCertificate ::= SEQUENCE {
            version [0] EXPLICIT OPTIONAL, serialNumber INTEGER, signature SEQUENCE,
            issuer SEQUENCE, validity SEQUENCE, subject SEQUENCE,
            subjectPublicKeyInfo SEQUENCE { algorithm SEQUENCE, subjectPublicKey BIT STRING },
            ... }
    """
    data = bytes(cert_der)
    start, stop = _expect(data, 0, len(data), _SEQUENCE, "certificate")
    if stop != len(data):
        raise CertificateParseError("certificate: trailing bytes after the certificate")
    tbs_start, tbs_end = _expect(data, start, stop, _SEQUENCE, "tbsCertificate")

    pos = tbs_start
    tag, _, value_end = _read_tlv(data, pos, tbs_end, "tbsCertificate.version")
    if tag == _CONTEXT_0:
        pos = value_end
    for tag, what in (
        (_INTEGER, "serialNumber"),
        (_SEQUENCE, "signature"),
        (_SEQUENCE, "issuer"),
        (_SEQUENCE, "validity"),
        (_SEQUENCE, "subject"),
    ):
        _, pos = _expect(data, pos, tbs_end, tag, f"tbsCertificate.{what}")

    spki_start = pos
    value_start, spki_end = _expect(
        data, pos, tbs_end, _SEQUENCE, "tbsCertificate.subjectPublicKeyInfo"
    )
    # Check the inside too, so that we only ever pin something shaped like a key.
    _, pos = _expect(data, value_start, spki_end, _SEQUENCE, "subjectPublicKeyInfo.algorithm")
    key_start, pos = _expect(
        data, pos, spki_end, _BIT_STRING, "subjectPublicKeyInfo.subjectPublicKey"
    )
    if pos != spki_end:
        raise CertificateParseError("subjectPublicKeyInfo: trailing bytes")
    if key_start == pos:
        raise CertificateParseError("subjectPublicKeyInfo.subjectPublicKey: empty BIT STRING")
    return data[spki_start:spki_end]


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def pin_from_spki(spki_der: bytes) -> str:
    """The pin of a DER ``SubjectPublicKeyInfo``."""
    return PIN_PREFIX + _b64url(hashlib.sha256(spki_der).digest())


def pin_from_der(cert_der: bytes) -> str:
    """The pin of a DER certificate (e.g. ``SSLObject.getpeercert(binary_form=True)``)."""
    return pin_from_spki(spki_from_der(cert_der))


def pin_from_pem(cert_pem: str) -> str:
    """The pin of a PEM certificate (one ``-----BEGIN CERTIFICATE-----`` block)."""
    try:
        der = ssl.PEM_cert_to_DER_cert(cert_pem)
    except (ValueError, binascii.Error) as exc:
        raise CertificateParseError(f"not a PEM certificate: {exc}") from None
    return pin_from_der(der)


def check_pin_format(pin: str) -> None:
    """Raise :class:`ValueError` unless ``pin`` looks like ``sha256/<43 base64url chars>``."""
    if not isinstance(pin, str) or not pin.startswith(PIN_PREFIX):
        raise ValueError(f"pin must start with {PIN_PREFIX!r}")
    body = pin[len(PIN_PREFIX) :]
    if len(body) != 43 or not body.isascii():
        raise ValueError("pin must be sha256/ followed by 43 base64url characters")
    try:
        raw = base64.urlsafe_b64decode(body + "=")
    except (ValueError, binascii.Error):
        raise ValueError("pin is not valid base64url") from None
    if len(raw) != 32 or _b64url(raw) != body:
        raise ValueError("pin is not a canonical base64url SHA-256 digest")


def pins_equal(a: str, b: str) -> bool:
    """Compare two pins in constant time."""
    return hmac.compare_digest(a.encode("utf-8"), b.encode("utf-8"))
