"""Fingerprint approval (APPROVAL.md): signed message, verification, key trust, outcomes."""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from typing import Any

import pytest
from conftest import StoreWriter
from fakeserver import DEVICE, PC, PINS, VECTORS_DIR, FakeServer, device_entry

import phonetopc
import phonetopc.sync
from phonetopc import approval
from phonetopc.approval import (
    ApprovalResult,
    approval_message,
    b64url_decode,
    b64url_encode,
    check_reason,
    new_nonce,
    verify_signature,
)
from phonetopc.pin import pin_from_spki

VECTORS: dict[str, Any] = json.loads((VECTORS_DIR / "approval.json").read_text())

try:
    import cryptography  # noqa: F401

    HAVE_CRYPTO = True
except ImportError:  # pragma: no cover - the dev extra installs it
    HAVE_CRYPTO = False

needs_crypto = pytest.mark.skipif(not HAVE_CRYPTO, reason="needs the cryptography package")


def stored_approval_key(path: Path) -> str | None:
    value: str | None = json.loads(path.read_text())["devices"][0].get("approval_key")
    return value


# -- the signed message and the test vectors (section 5) ---------------------------


def test_message_matches_vectors() -> None:
    v = VECTORS["valid"]
    msg = approval_message(v["phone_id"], v["pc_id"], v["nonce"], v["reason"])
    assert msg == VECTORS["message_utf8"].encode("utf-8")
    assert msg.hex() == VECTORS["message_hex"]
    assert not msg.endswith(b"\n")


def test_vector_key_pin() -> None:
    assert pin_from_spki(b64url_decode(VECTORS["key"])) == VECTORS["pin"]


@needs_crypto
def test_valid_vector_verifies() -> None:
    v = VECTORS["valid"]
    msg = approval_message(v["phone_id"], v["pc_id"], v["nonce"], v["reason"])
    assert verify_signature(v["key"], msg, v["signature"])


@needs_crypto
@pytest.mark.parametrize("case", VECTORS["invalid"], ids=lambda c: c["name"])
def test_invalid_vectors_are_rejected(case: dict[str, str]) -> None:
    msg = approval_message(case["phone_id"], case["pc_id"], case["nonce"], case["reason"])
    assert not verify_signature(case["key"], msg, case["signature"])


def test_invalid_vectors_cover_every_field() -> None:
    names = {c["name"] for c in VECTORS["invalid"]}
    assert {"wrong nonce", "wrong reason", "wrong pc id", "wrong phone id"} <= names
    assert {"corrupted signature", "different key"} <= names


@needs_crypto
def test_sign_with_vector_key_round_trip() -> None:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec

    key = serialization.load_pem_private_key(
        (VECTORS_DIR / "approval-key.pem").read_bytes(), password=None
    )
    assert isinstance(key, ec.EllipticCurvePrivateKey)
    spki = key.public_key().public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    assert b64url_encode(spki) == VECTORS["key"]
    msg = VECTORS["message_utf8"].encode()
    sig = b64url_encode(key.sign(msg, ec.ECDSA(hashes.SHA256())))
    assert verify_signature(VECTORS["key"], msg, sig)
    assert not verify_signature(VECTORS["key"], msg + b"x", sig)


@needs_crypto
@pytest.mark.parametrize(
    ("key", "signature"),
    [
        ("not base64!", VECTORS["valid"]["signature"]),
        (VECTORS["valid"]["key"], "not base64!"),
        (VECTORS["valid"]["key"] + "==", VECTORS["valid"]["signature"]),
        ("AAAA", VECTORS["valid"]["signature"]),  # not a key
        (VECTORS["valid"]["key"], ""),
        (VECTORS["valid"]["key"], "AAAA"),  # not a DER signature
    ],
)
def test_garbage_never_verifies(key: str, signature: str) -> None:
    msg = VECTORS["message_utf8"].encode()
    assert not verify_signature(key, msg, signature)


@needs_crypto
def test_other_curve_is_rejected() -> None:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec

    key = ec.generate_private_key(ec.SECP384R1())
    spki = key.public_key().public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    msg = b"phonetopc-approval/1\nx"
    sig = key.sign(msg, ec.ECDSA(hashes.SHA256()))
    assert not verify_signature(b64url_encode(spki), msg, b64url_encode(sig))


def test_b64url_decode_is_strict() -> None:
    assert b64url_decode("AAEC") == b"\x00\x01\x02"
    assert b64url_decode("-_8") == b"\xfb\xff"
    for bad in ("AAE=", "+/8", "A", "AB$C", "AB", "ä"):
        with pytest.raises(ValueError):
            b64url_decode(bad)


def test_nonce_is_fresh_32_bytes() -> None:
    nonces = {new_nonce() for _ in range(100)}
    assert len(nonces) == 100
    assert all(len(n) == 43 and len(b64url_decode(n)) == 32 for n in nonces)


def test_check_reason() -> None:
    assert check_reason("Run deploy?") == "Run deploy?"
    assert check_reason("x" * 200) == "x" * 200
    assert check_reason("🚀" * 200) == "🚀" * 200  # code points, not UTF-16 units
    assert check_reason("Deploy 🚀?") == "Deploy 🚀?"
    bad_inputs = (
        "",
        "   ",
        " Run deploy?",
        "Run deploy? ",
        "x" * 201,
        "a\nb",
        "a\tb",
        "bell\x07",
        "x\x85x",
        "🚀" * 201,
        "pay \u202eevil",
        "a\u2066b",
        "a\u200fb",
        "a\u061cb",
    )
    for bad in bad_inputs:
        with pytest.raises(ValueError):
            check_reason(bad)
    with pytest.raises(TypeError):
        check_reason(b"bytes")  # type: ignore[arg-type]


def test_result_truthiness() -> None:
    assert ApprovalResult(approved=True)
    assert not ApprovalResult(approved=False, why="denied")
    assert ApprovalResult(approved=False, why="busy").why == "busy"


# -- against the fake phone ----------------------------------------------------------


@pytest.fixture
def approving(secure_server: FakeServer, write_store: StoreWriter) -> Path:
    """A paired fake phone that approves; returns the path of devices.json."""
    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    secure_server.enable_approval()
    return write_store(device_entry(secure_server))


async def test_approved(secure_server: FakeServer, approving: Path) -> None:
    async with phonetopc.connect() as phone:
        assert "approval" in phone.capabilities
        assert phone.approval_key == secure_server.approval_info["pin"]
        result = await phone.request_approval("Run deploy.sh on arch-desktop?")
        assert result and result.approved and result.why is None
        assert isinstance(result, phonetopc.ApprovalResult)
        again = await phone.request_approval("Run deploy.sh on arch-desktop?", timeout=60)
        assert again
    first, second = secure_server.requests("request_approval")
    assert first["params"]["reason"] == "Run deploy.sh on arch-desktop?"  # sent unchanged
    assert first["params"]["timeout_ms"] == 30000 and second["params"]["timeout_ms"] == 60000
    assert len(first["params"]["nonce"]) == 43
    assert first["params"]["nonce"] != second["params"]["nonce"]  # fresh every time
    # What the phone signed is exactly what the client verified.
    assert secure_server.approval_signed[0] == approval_message(
        DEVICE["id"], PC["id"], first["params"]["nonce"], "Run deploy.sh on arch-desktop?"
    )
    # Trusted on first sight and stored.
    assert stored_approval_key(approving) == secure_server.approval_info["pin"]


async def test_client_waits_longer_than_the_phone(
    secure_server: FakeServer, approving: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    async with phonetopc.connect(request_timeout=1) as phone:
        timeouts: list[Any] = []
        real_call = phone._call

        async def spy(method: str, params: Any = None, **kw: Any) -> Any:
            timeouts.append(kw.get("timeout"))
            return await real_call(method, params, **kw)

        monkeypatch.setattr(phone, "_call", spy)
        assert await phone.request_approval("ok?", timeout=12.5)
    assert timeouts == [12.5 + 5]


@pytest.mark.parametrize("why", ["denied", "cancelled", "timeout", "lockout", "unavailable"])
async def test_refusals(secure_server: FakeServer, approving: Path, why: str) -> None:
    secure_server.approval_mode = why
    async with phonetopc.connect() as phone:
        result = await phone.request_approval("Run deploy?")
    assert not result and result.approved is False and result.why == why


async def test_busy(secure_server: FakeServer, approving: Path) -> None:
    secure_server.approval_mode = "busy"
    async with phonetopc.connect() as phone:
        assert (await phone.request_approval("x?")).why == "busy"
        # Really one at a time: a second request while the first is pending.
        secure_server.approval_mode = "approve"
        secure_server.approval_delay = 0.3
        first = asyncio.ensure_future(phone.request_approval("first?"))
        await secure_server.wait_for_request("request_approval", 2)
        second = await phone.request_approval("second?")
        assert not second and second.why == "busy"
        assert await first


async def test_no_answer_in_time(
    secure_server: FakeServer, approving: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    secure_server.approval_mode = "hang"
    monkeypatch.setattr(approval, "APPROVAL_GRACE", -4.8)  # wait 0.2 s instead of 10 s
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.RequestTimeout):
            await phone.request_approval("x?", timeout=5)


async def test_approval_unavailable_without_capability(
    secure_server: FakeServer, write_store: StoreWriter
) -> None:
    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    path = write_store(device_entry(secure_server))
    async with phonetopc.connect() as phone:
        assert phone.approval_key is None
        result = await phone.request_approval("x?")
    assert not result and result.why == "unavailable"
    assert secure_server.requests("request_approval") == []
    assert stored_approval_key(path) is None


async def test_dev_mode_has_no_approval(server: FakeServer) -> None:
    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    async with phonetopc.connect(server.addr, dev=True) as phone:
        with pytest.raises(phonetopc.ApprovalNotSupported, match="secure connection"):
            await phone.request_approval("x?")
        # The fake follows section 1 too.
        with pytest.raises(phonetopc.MethodNotFound):
            await phone.call(
                "request_approval", {"reason": "x", "nonce": new_nonce(), "timeout_ms": 5000}
            )
    assert len(server.requests("request_approval")) == 1


@pytest.mark.parametrize(
    ("kwargs", "match"),
    [
        ({"reason": ""}, "empty"),
        ({"reason": "a\nb"}, "control"),
        ({"reason": "x" * 201}, "longer"),
        ({"reason": "x", "timeout": 4.9}, "5-120"),
        ({"reason": "x", "timeout": 121}, "5-120"),
    ],
)
async def test_invalid_arguments_send_nothing(
    secure_server: FakeServer, approving: Path, kwargs: dict[str, Any], match: str
) -> None:
    async with phonetopc.connect() as phone:
        with pytest.raises(ValueError, match=match):
            await phone.request_approval(**kwargs)
    assert secure_server.requests("request_approval") == []


@pytest.mark.parametrize(
    "params",
    [
        {"reason": " ", "nonce": "A" * 43},
        {"reason": "x\ny", "nonce": "A" * 43},
        {"reason": "x", "nonce": "AAAA"},
        {"reason": "x", "nonce": "A" * 43, "timeout_ms": 4999},
        {"reason": "x", "nonce": "A" * 43, "timeout_ms": 120001},
    ],
)
async def test_fake_rejects_invalid_params(
    secure_server: FakeServer, approving: Path, params: dict[str, Any]
) -> None:
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.InvalidParams):
            await phone.call("request_approval", params)
    secure_server.schema_errors.clear()  # some of these are invalid by schema, on purpose


# -- forged answers are never approvals (sections 4.1 and 5) ------------------------------


def _replace_line(index: int, value: bytes) -> Any:
    def tamper(message: bytes) -> bytes:
        lines = message.split(b"\n")
        lines[index] = value
        return b"\n".join(lines)

    return tamper


@pytest.mark.parametrize(
    "tamper",
    [
        _replace_line(1, b"0000000000000000"),  # phone id
        _replace_line(2, b"0000000000000000"),  # pc id
        _replace_line(3, b"A" * 43),  # nonce (e.g. a replayed answer)
        _replace_line(4, b"A" * 43),  # reason
        _replace_line(0, b"phonetopc-approval/2"),
        lambda m: m + b"\n",
    ],
    ids=["phone id", "pc id", "nonce", "reason", "prefix", "trailing newline"],
)
async def test_signature_over_other_data(
    secure_server: FakeServer, approving: Path, tamper: Any
) -> None:
    secure_server.approval_tamper = tamper
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ApprovalVerificationError, match="not approved"):
            await phone.request_approval("Run deploy?")


async def test_signature_from_another_key(secure_server: FakeServer, approving: Path) -> None:
    async with phonetopc.connect() as phone:
        assert await phone.request_approval("x?")  # pins the current key
        secure_server.rotate_approval_key()  # signs with a key this session never saw
        with pytest.raises(phonetopc.ApprovalVerificationError):
            await phone.request_approval("x?")


@pytest.mark.parametrize(
    "hook",
    [
        lambda r: {"approved": True},
        lambda r: {**r, "signature": None},
        lambda r: {
            **r,
            "signature": r["signature"][:-2] + ("AA" if r["signature"][-2:] != "AA" else "BA"),
        },
        lambda r: {**r, "signature": VECTORS["valid"]["signature"]},
        lambda r: {"approved": True, "why": "denied"},
    ],
    ids=["no signature", "null signature", "corrupted", "someone else's", "approved with why"],
)
async def test_approved_without_valid_signature(
    secure_server: FakeServer, approving: Path, hook: Any
) -> None:
    secure_server.approval_result_hook = hook
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ApprovalVerificationError):
            await phone.request_approval("x?")
    secure_server.schema_errors.clear()  # the forged results break the schema on purpose


@pytest.mark.parametrize(
    "hook",
    [lambda r: {"approved": "true", "signature": r["signature"]}, lambda r: ["approved"]],
)
async def test_malformed_result(secure_server: FakeServer, approving: Path, hook: Any) -> None:
    secure_server.approval_result_hook = hook
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ProtocolError):
            await phone.request_approval("x?")
    secure_server.schema_errors.clear()


async def test_hello_pin_must_match_key(
    secure_server: FakeServer, approving: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    real = FakeServer.approval_info.fget
    assert real is not None
    monkeypatch.setattr(
        FakeServer, "approval_info", property(lambda s: {**real(s), "pin": PINS["pc"]})
    )
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ProtocolError, match="pin"):
            await phone.request_approval("x?")
    assert secure_server.requests("request_approval") == []
    assert stored_approval_key(approving) is None


# -- trusting the approval key (section 3) --------------------------------------------------


async def test_key_change_needs_explicit_acceptance(
    secure_server: FakeServer, approving: Path
) -> None:
    async with phonetopc.connect() as phone:
        assert await phone.request_approval("x?")
    old_pin = secure_server.approval_info["pin"]
    assert stored_approval_key(approving) == old_pin

    secure_server.rotate_approval_key()  # fingerprints changed on the phone
    new_pin = secure_server.approval_info["pin"]
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ApprovalKeyChanged) as info:
            await phone.request_approval("x?")
        err = info.value
        assert "fingerprints changed on the phone? run `phonetopc approval-key --accept`" in (
            str(err).lower()
        )
        assert err.stored == old_pin and err.actual == new_pin
        assert isinstance(err, phonetopc.ApprovalError)
        # Not sent, and the stored key is untouched.
        assert len(secure_server.requests("request_approval")) == 1
        assert stored_approval_key(approving) == old_pin

        assert await phone.accept_approval_key()
        assert stored_approval_key(approving) == new_pin
        assert await phone.request_approval("x?")
        assert not await phone.accept_approval_key()  # already accepted


async def test_preexisting_approval_key_is_used(
    secure_server: FakeServer, write_store: StoreWriter
) -> None:
    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    secure_server.enable_approval()
    pin = secure_server.approval_info["pin"]
    path = write_store(device_entry(secure_server, approval_key=pin, extra="kept"))
    inode = path.stat().st_ino
    async with phonetopc.connect() as phone:
        assert await phone.request_approval("x?")
    assert path.stat().st_ino == inode  # nothing to write
    path = write_store(device_entry(secure_server, approval_key=PINS["pc"]))
    async with phonetopc.connect() as phone:
        with pytest.raises(phonetopc.ApprovalKeyChanged):
            await phone.request_approval("x?")


async def test_stored_key_is_read_fresh(secure_server: FakeServer, approving: Path) -> None:
    # `phonetopc approval-key --accept` in another process, while connected.
    secure_server.rotate_approval_key()
    async with phonetopc.connect() as phone:
        phonetopc.store.accept_approval_key(DEVICE["id"], PINS["pc"])
        with pytest.raises(phonetopc.ApprovalKeyChanged):
            await phone.request_approval("x?")
        phonetopc.store.accept_approval_key(DEVICE["id"], secure_server.approval_info["pin"])
        assert await phone.request_approval("x?")


# -- without cryptography ------------------------------------------------------------------


async def test_missing_cryptography(
    secure_server: FakeServer, write_store: StoreWriter, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Runs with and without cryptography installed: an approving phone if it is.
    if HAVE_CRYPTO:
        secure_server.enable_approval()
    write_store(device_entry(secure_server))
    async with phonetopc.connect() as phone:
        for name in list(sys.modules):
            if name == "cryptography" or name.startswith("cryptography."):
                monkeypatch.setitem(sys.modules, name, None)
        monkeypatch.setitem(sys.modules, "cryptography", None)
        with pytest.raises(phonetopc.ApprovalDependencyMissing) as info:
            await phone.request_approval("x?")
    err = info.value
    assert isinstance(err, ImportError) and isinstance(err, phonetopc.Error)
    assert "pip install 'phonetopc[approval]'" in str(err)
    assert secure_server.requests("request_approval") == []


# -- blocking API --------------------------------------------------------------------------


def test_sync_request_approval(
    threaded_secure_server: FakeServer, write_store: StoreWriter
) -> None:
    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    threaded_secure_server.enable_approval()
    write_store(device_entry(threaded_secure_server))
    with phonetopc.sync.connect() as phone:
        assert phone.request_approval("Run deploy?")
        threaded_secure_server.approval_mode = "denied"
        result = phone.request_approval("Run deploy?", timeout=10)
        assert not result and result.why == "denied"
        assert phone.approval_key == threaded_secure_server.approval_info["pin"]
        assert not phone.accept_approval_key()


def test_cli_approval_key(
    threaded_secure_server: FakeServer,
    write_store: StoreWriter,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from phonetopc.cli import main

    if not HAVE_CRYPTO:
        pytest.skip("needs the cryptography package")
    write_store(device_entry(threaded_secure_server))
    assert main(["approval-key"]) == 1
    assert "no approval key" in capsys.readouterr().out

    threaded_secure_server.enable_approval()
    pin = threaded_secure_server.approval_info["pin"]
    path = write_store(device_entry(threaded_secure_server, approval_key=PINS["pc"]))
    assert main(["approval-key"]) == 0
    out = capsys.readouterr().out
    assert pin in out and PINS["pc"] in out and "--accept" in out
    assert stored_approval_key(path) == PINS["pc"]

    assert main(["approval-key", "--accept"]) == 0
    assert capsys.readouterr().out.startswith("accepted: ")
    assert stored_approval_key(path) == pin
    assert main(["approval-key", "--accept"]) == 0
    assert "already accepted" in capsys.readouterr().out
