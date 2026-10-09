from __future__ import annotations

import asyncio
import contextlib
import json
import os
import sys
import threading
from collections.abc import AsyncIterator, Callable, Iterator
from pathlib import Path
from typing import Any

import pytest

sys.path.insert(0, str(Path(__file__).parent))

from fakeserver import FakeServer

import phonetopc


@pytest.fixture(autouse=True)
def config_home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Every test gets an empty XDG_CONFIG_HOME, so the real devices.json is never touched."""
    home = tmp_path / "xdg-config"
    home.mkdir()
    monkeypatch.setenv("XDG_CONFIG_HOME", str(home))
    monkeypatch.delenv("PHONETOPC_ADDR", raising=False)
    # No real mDNS in tests, even when zeroconf is installed: tests that want
    # discovery patch phonetopc.discovery.locate.
    monkeypatch.setitem(sys.modules, "zeroconf", None)
    monkeypatch.setitem(sys.modules, "zeroconf.asyncio", None)
    return home


StoreWriter = Callable[..., Path]


@pytest.fixture
def write_store(config_home: Path) -> StoreWriter:
    """Write devices.json (version 1, mode 0600) like the Go CLI does."""

    def write(*devices: dict[str, Any], mode: int = 0o600, **doc: Any) -> Path:
        directory = config_home / "phonetopc"
        directory.mkdir(mode=0o700, exist_ok=True)
        path = directory / "devices.json"
        content = {"version": 1, "devices": list(devices), **doc}
        path.write_text(json.dumps(content, indent=2))
        os.chmod(path, mode)
        return path

    return write


@pytest.fixture
async def secure_server() -> AsyncIterator[FakeServer]:
    srv = FakeServer(secure=True)
    await srv.start()
    try:
        yield srv
    finally:
        await srv.stop()
    assert not srv.schema_errors, "\n".join(srv.schema_errors)


@pytest.fixture
async def server() -> AsyncIterator[FakeServer]:
    srv = FakeServer()
    await srv.start()
    try:
        yield srv
    finally:
        await srv.stop()
    assert not srv.schema_errors, "\n".join(srv.schema_errors)


@pytest.fixture
async def phone(server: FakeServer) -> AsyncIterator[phonetopc.Phone]:
    async with phonetopc.connect(server.addr, client_name="pytest") as p:
        yield p


@contextlib.contextmanager
def _threaded(srv: FakeServer) -> Iterator[FakeServer]:
    """Run ``srv`` on its own loop thread, so sync (blocking) code can use it."""
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    asyncio.run_coroutine_threadsafe(srv.start(), loop).result(5)
    try:
        yield srv
    finally:
        asyncio.run_coroutine_threadsafe(srv.stop(), loop).result(5)
        loop.call_soon_threadsafe(loop.stop)
        thread.join(5)
        loop.close()
    assert not srv.schema_errors, "\n".join(srv.schema_errors)


@pytest.fixture
def threaded_server() -> Iterator[FakeServer]:
    """A FakeServer on its own loop thread, so sync (blocking) code can use it."""
    with _threaded(FakeServer()) as srv:
        yield srv


@pytest.fixture
def threaded_secure_server() -> Iterator[FakeServer]:
    """A secure FakeServer on its own loop thread."""
    with _threaded(FakeServer(secure=True)) as srv:
        yield srv
