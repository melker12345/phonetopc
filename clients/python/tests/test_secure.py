"""Secure mode (SECURITY.md sections 3, 5 and 6) against the in-process fake phone."""

from __future__ import annotations

import asyncio
import json
import os
import socket
import ssl
import sys
from collections.abc import AsyncIterator
from pathlib import Path

import pytest
from conftest import StoreWriter
from fakeserver import DEVICE, PC, PINS, TOKEN, FakeServer, device_entry

import phonetopc
import phonetopc.sync
from phonetopc import _tls, client, discovery
from phonetopc.cli import main


@pytest.fixture
async def pc_cert_server() -> AsyncIterator[FakeServer]:
    """A secure server presenting the *PC* test certificate: not the paired phone's key."""
    srv = FakeServer(secure=True, cert="pc")
    await srv.start()
    try:
        yield srv
    finally:
        await srv.stop()


@pytest.fixture
def no_loopback_skip(monkeypatch: pytest.MonkeyPatch) -> None:
    """The fake phone lives on 127.0.0.1, which the client never stores as an address."""
    monkeypatch.setattr(client, "_is_loopback", lambda host: False)


def stored_addresses(path: Path, index: int = 0) -> list[str]:
    return list(json.loads(path.read_text())["devices"][index]["addresses"])


# -- the happy path ----------------------------------------------------------------


async def test_connect_paired_phone(secure_server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(secure_server))
    async with phonetopc.connect(client_name="pytest") as phone:
        assert phone.secure
        assert phone.url == f"wss://127.0.0.1:{secure_server.port}/v1/ws"
        assert phone.paired_device is not None and phone.paired_device.id == DEVICE["id"]
        assert phone.pc == phonetopc.PCInfo(id=PC["id"], name=PC["name"])
        assert phone.device.id == DEVICE["id"]
        assert (await phone.get("battery")).value["level"] == 82
        async with phone.subscribe("gyro", rate_hz=200) as events:
            async for ev in events:
                assert ev.seq == 0
                break
    (hello,) = secure_server.requests("hello")
    assert hello["params"]["auth"] == {"token": TOKEN}
    assert secure_server.received[0] is hello


async def test_dev_mode_sends_no_token(server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(server))  # a pairing exists, but dev mode never uses it
    async with phonetopc.connect(server.addr, dev=True) as phone:
        assert not phone.secure
        assert phone.paired_device is None and phone.pc is None
    assert "auth" not in server.requests("hello")[0]["params"]


@pytest.mark.parametrize("form", ["host:port", "wss", "env"])
async def test_explicit_address_of_paired_phone(
    secure_server: FakeServer,
    write_store: StoreWriter,
    monkeypatch: pytest.MonkeyPatch,
    form: str,
) -> None:
    write_store(
        device_entry(id="ffff", name="Tablet", addresses=["127.0.0.9"]),
        device_entry(secure_server),
    )
    addr: str | None = secure_server.addr
    if form == "wss":
        addr = f"wss://{secure_server.addr}"
    elif form == "env":
        monkeypatch.setenv("PHONETOPC_ADDR", secure_server.addr)
        addr = None
    async with phonetopc.connect(addr) as phone:
        assert phone.secure and phone.paired_device is not None
        assert phone.paired_device.name == "Fake Phone"


async def test_unknown_address_with_one_pairing(
    secure_server: FakeServer, write_store: StoreWriter
) -> None:
    write_store(device_entry(secure_server, addresses=["192.0.2.1"]))
    async with phonetopc.connect(secure_server.addr) as phone:  # e.g. the phone's new IP
        assert phone.secure


async def test_unknown_address_with_several_pairings(
    secure_server: FakeServer, write_store: StoreWriter
) -> None:
    write_store(
        device_entry(secure_server, addresses=[]),
        device_entry(id="ffff", name="Tablet", addresses=[]),
    )
    with pytest.raises(phonetopc.AmbiguousDevice, match="pass device="):
        await phonetopc.connect(secure_server.addr)
    async with phonetopc.connect(secure_server.addr, device="Fake Phone") as phone:
        assert phone.secure


async def test_device_by_name_or_id(secure_server: FakeServer, write_store: StoreWriter) -> None:
    write_store(
        device_entry(id="ffff", name="Tablet", addresses=["127.0.0.9"], port=1),
        device_entry(secure_server),
    )
    with pytest.raises(phonetopc.AmbiguousDevice, match=r"several paired phones.*pass device="):
        await phonetopc.connect()
    for query in ("Fake Phone", "fake phone", DEVICE["id"]):
        async with phonetopc.connect(device=query) as phone:
            assert phone.paired_device is not None and phone.paired_device.id == DEVICE["id"]
    with pytest.raises(phonetopc.NotPaired, match="'Pixel'"):
        await phonetopc.connect(device="Pixel")


async def test_not_paired() -> None:
    with pytest.raises(phonetopc.NotPaired, match=r"no paired phone: run `phonetopc pair`"):
        await phonetopc.connect()
    with pytest.raises(phonetopc.NotPaired, match="dev=True"):
        await phonetopc.connect("192.168.1.20")


@pytest.mark.parametrize(
    ("kwargs", "match"),
    [
        ({"dev": True, "device": "x"}, "can't be combined"),
        ({"addr": "ws://127.0.0.1", "device": "x"}, "dev-mode"),
        ({"addr": "wss://127.0.0.1", "dev": True}, "without TLS"),
    ],
)
def test_conflicting_arguments(kwargs: dict[str, object], match: str) -> None:
    with pytest.raises(ValueError, match=match):
        phonetopc.connect(**kwargs)  # type: ignore[arg-type]


# -- finding the phone ---------------------------------------------------------------


async def test_address_fallback_and_update(
    secure_server: FakeServer, write_store: StoreWriter, no_loopback_skip: None
) -> None:
    # 127.0.0.2 refuses (the fake listens on 127.0.0.1 only), so the second one is used,
    # and is moved to the front for next time.
    path = write_store(device_entry(secure_server, addresses=["127.0.0.2", "127.0.0.1"]))
    async with phonetopc.connect() as phone:
        assert phone.url == f"wss://127.0.0.1:{secure_server.port}/v1/ws"
    assert stored_addresses(path) == ["127.0.0.1", "127.0.0.2"]
    assert path.stat().st_mode & 0o777 == 0o600


async def test_explicit_new_address_is_remembered(
    secure_server: FakeServer, write_store: StoreWriter, no_loopback_skip: None
) -> None:
    path = write_store(device_entry(secure_server, addresses=["192.168.1.153"]))
    async with phonetopc.connect(secure_server.addr):
        pass
    assert stored_addresses(path) == ["127.0.0.1", "192.168.1.153"]


async def test_loopback_is_not_remembered(
    secure_server: FakeServer, write_store: StoreWriter
) -> None:
    path = write_store(device_entry(secure_server, addresses=["192.168.1.153"]))
    async with phonetopc.connect(secure_server.addr):  # e.g. through adb forward
        pass
    assert stored_addresses(path) == ["192.168.1.153"]


async def test_store_update_failure_doesnt_break_connect(
    secure_server: FakeServer,
    write_store: StoreWriter,
    no_loopback_skip: None,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    write_store(device_entry(secure_server, addresses=[]))

    def fail(*args: object, **kwargs: object) -> bool:
        raise OSError("read-only file system")

    monkeypatch.setattr(client, "remember_address", fail)
    async with phonetopc.connect(secure_server.addr) as phone:
        assert phone.secure
    assert "read-only file system" in caplog.text


async def test_without_zeroconf_mdns_is_skipped() -> None:
    # conftest makes `import zeroconf` fail, as if the extra weren't installed.
    with pytest.raises(ImportError):
        import zeroconf  # noqa: F401
    assert await discovery.locate("a1b2c3d4", timeout=5) is None


async def test_mdns_result_is_tried_first_and_stored(
    secure_server: FakeServer,
    write_store: StoreWriter,
    monkeypatch: pytest.MonkeyPatch,
    no_loopback_skip: None,
) -> None:
    # The stored port is stale; mDNS knows the current one.
    path = write_store(device_entry(secure_server, port=1, addresses=["127.0.0.1"]))
    asked: list[str] = []

    async def fake_locate(device_id: str, timeout: float = 2.0) -> discovery.Located:
        asked.append(device_id)
        return discovery.Located(("127.0.0.1",), secure_server.port)

    monkeypatch.setattr(discovery, "locate", fake_locate)
    async with phonetopc.connect() as phone:
        assert phone.url == f"wss://127.0.0.1:{secure_server.port}/v1/ws"
    assert asked == [DEVICE["id"]]
    assert json.loads(path.read_text())["devices"][0]["port"] == secure_server.port


async def test_unreachable(write_store: StoreWriter) -> None:
    write_store(device_entry(port=1, addresses=["127.0.0.1", "127.0.0.2"]))
    with pytest.raises(phonetopc.PhoneUnreachable) as info:
        await phonetopc.connect(open_timeout=2)
    err = info.value
    assert isinstance(err, OSError)
    assert [url for url, _ in err.attempts] == [
        "wss://127.0.0.1:1/v1/ws",
        "wss://127.0.0.2:1/v1/ws",
    ]
    assert "same network" in str(err) and "phonetopc[discovery]" in str(err)


async def test_no_known_address(write_store: StoreWriter) -> None:
    write_store(device_entry(addresses=[]))
    with pytest.raises(phonetopc.PhoneUnreachable, match="no known address"):
        await phonetopc.connect()


async def test_phone_in_dev_mode(server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(server))
    with pytest.raises(phonetopc.PhoneUnreachable, match="dev mode"):
        await phonetopc.connect()
    assert server.received == []  # no plaintext fallback


# -- pins ---------------------------------------------------------------------------


async def test_pin_mismatch(pc_cert_server: FakeServer, write_store: StoreWriter) -> None:
    # The stored pin is the phone's; this server has another key. A second, good
    # address must not be tried: a mismatch is a hard failure (SECURITY.md 3).
    write_store(device_entry(pc_cert_server, addresses=["127.0.0.1", "127.0.0.3"]))
    with pytest.raises(phonetopc.PinMismatch) as info:
        await phonetopc.connect()
    err = info.value
    assert "phone identity changed" in str(err) and "phonetopc pair" in str(err)
    assert err.expected == PINS["phone"] and err.actual == PINS["pc"]
    assert err.address == f"wss://127.0.0.1:{pc_cert_server.port}/v1/ws"
    # Nothing was sent: not even the WebSocket upgrade request, let alone the token.
    await asyncio.sleep(0.05)
    assert pc_cert_server.http_requests == []
    assert pc_cert_server.received == []


async def test_pin_rechecked_before_hello(
    pc_cert_server: FakeServer, write_store: StoreWriter, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Simulate an event loop that bypasses SSLContext.sslobject_class (the
    # in-handshake check): the check on the open connection still stops hello.
    monkeypatch.setattr(_tls.PinnedContext, "sslobject_class", ssl.SSLObject)
    write_store(device_entry(pc_cert_server))
    with pytest.raises(phonetopc.PinMismatch):
        await phonetopc.connect()
    await asyncio.sleep(0.05)
    assert pc_cert_server.received == []


def test_pinned_context_settings() -> None:
    ctx = _tls.pinned_context(PINS["phone"])
    assert ctx.verify_mode == ssl.CERT_NONE and not ctx.check_hostname
    assert ctx.minimum_version == ssl.TLSVersion.TLSv1_2
    tls12 = [c for c in ctx.get_ciphers() if c["protocol"] == "TLSv1.2"]
    assert tls12 and all(c["kea"] == "kx-ecdhe" and c["aead"] for c in tls12)


# -- tokens ---------------------------------------------------------------------------


async def test_wrong_token(secure_server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(secure_server, token="bm90LXRoZS10b2tlbg"))
    with pytest.raises(phonetopc.Unauthorized) as info:
        await phonetopc.connect()
    err = info.value
    assert err.code == -32010 and err.name == "unauthorized"
    assert isinstance(err, phonetopc.PhonetopcError)
    assert "revoked" in str(err) and "phonetopc pair" in str(err)
    await asyncio.sleep(0.05)
    assert secure_server.requests("hello")  # it did get a hello, and refused it


async def test_revoked_while_connected(secure_server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(secure_server))
    async with phonetopc.connect() as phone:
        await secure_server.revoke(TOKEN)
        await asyncio.wait_for(phone.wait_closed(), 2)
        assert phone.closed_reason is not None and phone.closed_reason.code == 1008
    with pytest.raises(phonetopc.Unauthorized):
        await phonetopc.connect()


# -- sync API and CLI ------------------------------------------------------------------


def test_sync_secure(threaded_secure_server: FakeServer, write_store: StoreWriter) -> None:
    write_store(device_entry(threaded_secure_server))
    with phonetopc.sync.connect(device="Fake Phone") as phone:
        assert phone.secure and phone.pc is not None and phone.pc.name == PC["name"]
        assert phone.get("battery").value["level"] == 82
    with pytest.raises(phonetopc.NotPaired):
        phonetopc.sync.connect(device="nope")


def test_cli_secure(
    threaded_secure_server: FakeServer,
    write_store: StoreWriter,
    capsys: pytest.CaptureFixture[str],
) -> None:
    write_store(device_entry(threaded_secure_server))
    assert main(["info", "--device", "Fake Phone"]) == 0
    out = capsys.readouterr().out
    assert "pinned TLS + token" in out and PC["name"] in out and "wss://" in out
    assert main(["get", "battery", "--json"]) == 0
    assert json.loads(capsys.readouterr().out)["value"]["level"] == 82
    assert main(["info", "--device", "nope"]) == 1
    assert "no paired phone with id or name 'nope'" in capsys.readouterr().err


def test_cli_dev_flag(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["info", "--addr", threaded_server.addr]) == 1
    assert "run `phonetopc pair`" in capsys.readouterr().err
    assert main(["info", "--dev", "--addr", threaded_server.addr]) == 0
    assert "none (dev mode)" in capsys.readouterr().out


async def test_real_mdns(
    secure_server: FakeServer, write_store: StoreWriter, monkeypatch: pytest.MonkeyPatch
) -> None:
    """End to end with the real zeroconf package, if installed and multicast works here.

    Skipped otherwise; set PHONETOPC_TEST_MDNS=1 to make "no multicast" a failure.
    """
    monkeypatch.delitem(sys.modules, "zeroconf", raising=False)
    monkeypatch.delitem(sys.modules, "zeroconf.asyncio", raising=False)
    zc = pytest.importorskip("zeroconf")
    azc = pytest.importorskip("zeroconf.asyncio")
    strict = os.environ.get("PHONETOPC_TEST_MDNS") == "1"

    path = write_store(device_entry(secure_server, port=1, addresses=[]))
    aiozc = azc.AsyncZeroconf(interfaces=["127.0.0.1"], ip_version=zc.IPVersion.V4Only)
    info = azc.AsyncServiceInfo(
        discovery.SERVICE_TYPE,
        f"Fake Phone {secure_server.port}.{discovery.SERVICE_TYPE}",
        addresses=[socket.inet_aton("127.0.0.1")],
        port=secure_server.port,
        properties={"id": DEVICE["id"], "proto": "1", "tls": "1"},
        server=f"fake-phone-{secure_server.port}.local.",
    )
    try:
        await aiozc.async_register_service(info)
        located = await discovery.locate(DEVICE["id"], timeout=3)
        if located is None and not strict:
            pytest.skip("mDNS multicast doesn't work in this environment")
        assert located == discovery.Located(("127.0.0.1",), secure_server.port)
        assert await discovery.locate("not-this-one", timeout=0.5) is None
        async with phonetopc.connect() as phone:  # no stored address, stale port
            assert phone.url == f"wss://127.0.0.1:{secure_server.port}/v1/ws"
        assert stored_addresses(path) == []  # loopback is never stored
    finally:
        await aiozc.async_unregister_service(info)
        await aiozc.async_close()
