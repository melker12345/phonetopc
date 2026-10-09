from __future__ import annotations

import asyncio
import sys
import threading
from collections.abc import AsyncIterator, Iterator
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent))

from fakeserver import FakeServer

import phonetopc


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


@pytest.fixture
def threaded_server() -> Iterator[FakeServer]:
    """A FakeServer on its own loop thread, so sync (blocking) code can use it."""
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    srv = FakeServer()
    asyncio.run_coroutine_threadsafe(srv.start(), loop).result(5)
    try:
        yield srv
    finally:
        asyncio.run_coroutine_threadsafe(srv.stop(), loop).result(5)
        loop.call_soon_threadsafe(loop.stop)
        thread.join(5)
        loop.close()
    assert not srv.schema_errors, "\n".join(srv.schema_errors)
