"""Pins (SECURITY.md section 2) and the DER walker behind them."""

from __future__ import annotations

import json
import random
import ssl
from pathlib import Path
from typing import Any

import pytest

from phonetopc.pin import (
    CertificateParseError,
    _b64url,
    check_pin_format,
    pin_from_der,
    pin_from_pem,
    pin_from_spki,
    pins_equal,
    spki_from_der,
)

VECTORS_DIR = Path(__file__).resolve().parents[3] / "spec" / "conformance" / "vectors"
VECTORS: dict[str, Any] = json.loads((VECTORS_DIR / "vectors.json").read_text())


def _der(name: str) -> bytes:
    return ssl.PEM_cert_to_DER_cert((VECTORS_DIR / name).read_text())


@pytest.mark.parametrize("vector", VECTORS["pin"], ids=lambda v: v["cert"])
def test_pin_vectors(vector: dict[str, str]) -> None:
    assert pin_from_pem((VECTORS_DIR / vector["cert"]).read_text()) == vector["pin"]
    assert pin_from_der(_der(vector["cert"])) == vector["pin"]


def test_base64url_vector() -> None:
    b = VECTORS["base64url"]
    assert _b64url(bytes.fromhex(b["bytes_hex"])) == b["encoded"]


def test_spki_is_a_p256_key() -> None:
    spki = spki_from_der(_der("phone-cert.pem"))
    # SEQUENCE (89 bytes) { SEQUENCE { id-ecPublicKey, prime256v1 }, BIT STRING (66 bytes) }
    assert len(spki) == 91
    assert spki[:2] == b"\x30\x59"
    assert bytes.fromhex("06072a8648ce3d0201") in spki  # id-ecPublicKey
    assert bytes.fromhex("06082a8648ce3d030107") in spki  # prime256v1
    assert pin_from_spki(spki) == pin_from_der(_der("phone-cert.pem"))


# -- a tiny DER encoder to build certificate-shaped inputs ----------------------


def tlv(tag: int, content: bytes = b"") -> bytes:
    n = len(content)
    if n < 0x80:
        length = bytes([n])
    else:
        raw = n.to_bytes((n.bit_length() + 7) // 8, "big")
        length = bytes([0x80 | len(raw)]) + raw
    return bytes([tag]) + length + content


def seq(*parts: bytes) -> bytes:
    return tlv(0x30, b"".join(parts))


SPKI = seq(seq(tlv(0x06, b"\x2a\x86\x48\xce\x3d\x02\x01")), tlv(0x03, b"\x00\x04" + b"k" * 64))


def cert(*, version: bool = True, spki: bytes = SPKI, extra_tbs: bytes = b"") -> bytes:
    tbs = seq(
        tlv(0xA0, tlv(0x02, b"\x02")) if version else b"",
        tlv(0x02, b"\x01"),  # serialNumber
        seq(),  # signature
        seq(),  # issuer
        seq(),  # validity
        seq(),  # subject
        spki,
        extra_tbs,
    )
    return seq(tbs, seq(), tlv(0x03, b"\x00sig"))


def test_synthetic_certificates() -> None:
    assert spki_from_der(cert()) == SPKI
    assert spki_from_der(cert(version=False)) == SPKI  # version is OPTIONAL (v1)
    assert spki_from_der(cert(extra_tbs=tlv(0xA3, seq()))) == SPKI  # extensions after it
    # Long-form lengths are fine when they are needed.
    big = seq(seq(tlv(0x06, b"\x2a")), tlv(0x03, b"\x00" + b"k" * 300))
    assert spki_from_der(cert(spki=big)) == big
    assert spki_from_der(bytearray(cert())) == SPKI


@pytest.mark.parametrize(
    ("data", "match"),
    [
        (b"", "truncated"),
        (b"\x30", "truncated"),
        (b"\x30\x80\x00\x00", "indefinite"),
        (b"\x30\x81\x05" + b"\x00" * 5, "below 128"),
        (b"\x30\x82\x00\x80" + b"\x00" * 128, "leading zero"),
        (b"\x30\x85\x01\x00\x00\x00\x00", "too long"),
        (b"\x30\x84\x7f\xff\xff\xff", "implausibly large"),
        (b"\x30\x05\x00", "truncated"),
        (b"\x1f\x01\x00", "multi-byte tags"),
        (b"\x31\x00", "expected tag 0x30"),
        (cert() + b"\x00", "trailing bytes"),
        (seq(seq()), "truncated"),  # empty tbsCertificate
        (cert(spki=tlv(0x30, seq())), "truncated"),  # SPKI without key
        (cert(spki=seq(seq(), tlv(0x03))), "empty BIT STRING"),
        (cert(spki=seq(seq(), tlv(0x03, b"\x00"), tlv(0x05))), "trailing bytes"),
        (cert(spki=seq(seq(), tlv(0x04, b"\x00"))), "expected tag 0x03"),
        (cert(spki=tlv(0x02, b"\x01")), "expected tag 0x30"),
    ],
)
def test_malformed(data: bytes, match: str) -> None:
    with pytest.raises(CertificateParseError, match=match):
        spki_from_der(data)


def test_parse_error_is_a_value_error() -> None:
    with pytest.raises(ValueError):
        spki_from_der(b"nope")


@pytest.mark.parametrize("name", ["phone-cert.pem", "pc-cert.pem"])
def test_every_truncation_fails_cleanly(name: str) -> None:
    der = _der(name)
    for n in range(len(der)):
        with pytest.raises(CertificateParseError):
            spki_from_der(der[:n])


def _parse_or_clean_error(data: bytes) -> bytes | None:
    """Parse, allowing only CertificateParseError; a result must be a slice of the input."""
    try:
        spki = spki_from_der(data)
    except CertificateParseError:
        return None
    assert spki in data
    return spki


def test_fuzz_random_garbage() -> None:
    rng = random.Random(20261009)
    for _ in range(5000):
        n = rng.choice([0, 1, 2, 3, 5, 16, 64, 300, 1000])
        data = bytes(rng.getrandbits(8) for _ in range(n))
        if rng.random() < 0.5 and data:
            data = b"\x30" + data[1:]  # get past the first check more often
        _parse_or_clean_error(data)


@pytest.mark.parametrize("name", ["phone-cert.pem", "pc-cert.pem"])
def test_fuzz_mutations(name: str) -> None:
    der = _der(name)
    original = spki_from_der(der)
    rng = random.Random(name)
    for pos in range(len(der)):
        for value in {0x00, 0x7F, 0x80, 0x81, 0xFF, der[pos] ^ 0x01, rng.getrandbits(8)}:
            mutated = der[:pos] + bytes([value]) + der[pos + 1 :]
            spki = _parse_or_clean_error(mutated)
            if spki is not None and pos < 4:
                assert spki == original  # header bytes can't silently change the key


def test_pin_from_pem_rejects_non_pem() -> None:
    with pytest.raises(CertificateParseError):
        pin_from_pem("not a certificate")


def test_pin_format() -> None:
    for v in VECTORS["pin"]:
        check_pin_format(v["pin"])
    for bad in [
        "",
        "sha1/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc",
        "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaB",  # too short
        "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc=",  # padded
        "sha256/tQITurCV6l1Rq9Rk0X+kPrHlWaaKPnkI6CGl03GEaBc",  # standard alphabet
        "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBd",  # non-canonical last char
        "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBé",
    ]:
        with pytest.raises(ValueError):
            check_pin_format(bad)


def test_pins_equal() -> None:
    a, b = (v["pin"] for v in VECTORS["pin"])
    assert pins_equal(a, a)
    assert not pins_equal(a, b)
    assert not pins_equal(a, a[:-1])
    assert not pins_equal(a, "")
