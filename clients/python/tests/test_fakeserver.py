"""The fake server itself follows draft 1 where the client never goes.

These use a bare WebSocket, so they can send what the client never would.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import AsyncIterator
from typing import Any

import pytest
from fakeserver import FakeServer
from websockets.asyncio.client import ClientConnection, connect


@pytest.fixture
async def ws(server: FakeServer) -> AsyncIterator[ClientConnection]:
    async with connect(f"ws://{server.addr}/v1/ws") as conn:
        yield conn
    # Everything here is deliberately off-spec on the client side.
    server.schema_errors = [e for e in server.schema_errors if not e.startswith("client sent")]


async def recv(ws: ClientConnection) -> Any:
    return json.loads(await asyncio.wait_for(ws.recv(), 2))


async def call(ws: ClientConnection, raw: Any) -> Any:
    await ws.send(raw if isinstance(raw, str) else json.dumps(raw))
    while True:
        msg = await recv(ws)
        if "id" in msg:  # skip events
            return msg


def req(req_id: Any, method: str, params: Any = None, **extra: Any) -> dict[str, Any]:
    msg: dict[str, Any] = {"jsonrpc": "2.0", "id": req_id, "method": method, **extra}
    if params is not None:
        msg["params"] = params
    return msg


HELLO = {"protocol": "phonetopc/1"}


def code(resp: Any) -> int:
    assert "error" in resp, resp
    c: int = resp["error"]["code"]
    return c


async def test_hello_gating(ws: ClientConnection) -> None:
    assert code(await call(ws, req(1, "frobnicate"))) == -32003  # before method lookup
    assert code(await call(ws, req(2, "hello", {"protocol": "phonetopc/9"}))) == -32004
    assert "result" in await call(ws, req(3, "hello", HELLO))
    # A failed hello later doesn't undo the earlier one.
    assert code(await call(ws, req(4, "hello", {"protocol": "phonetopc/9"}))) == -32004
    assert code(await call(ws, req(5, "frobnicate"))) == -32601
    assert "result" in await call(ws, req(6, "list_sensors"))


async def test_client_notifications_are_ignored(server: FakeServer, ws: ClientConnection) -> None:
    await ws.send(json.dumps({"jsonrpc": "2.0", "method": "hello", "params": HELLO}))
    await ws.send(json.dumps({"jsonrpc": "2.0", "method": "list_sensors"}))
    resp = await call(ws, req("x", "hello", HELLO))
    assert resp["id"] == "x" and "result" in resp  # the first reply is to "x"
    assert len(server.sent) == 1


async def test_params_null_is_absent(ws: ClientConnection) -> None:
    await call(ws, req(1, "hello", HELLO))
    base = {"jsonrpc": "2.0", "params": None}
    assert "result" in await call(ws, {**base, "id": 2, "method": "list_sensors"})
    assert code(await call(ws, {**base, "id": 3, "method": "get"})) == -32602  # needs source
    assert code(await call(ws, req(4, "list_sensors", [1]))) == -32602


@pytest.mark.parametrize(
    "params",
    [
        [],
        "gyro",
        {"source": 5},
        {"source": "gyro", "type": 1},
        {"source": "gyro", "rate_hz": 0},
        {"source": "gyro", "rate_hz": -1},
        {"source": "gyro", "rate_hz": "fast"},
        {"source": "light", "rate_hz": 0},  # checked even where rate_hz is ignored
        {"source": "gyro", "min_delta": -0.1},
        {"source": "gyro", "type": "bogus"},
    ],
)
async def test_subscribe_invalid_params(ws: ClientConnection, params: Any) -> None:
    await call(ws, req(1, "hello", HELLO))
    resp = await call(ws, req(2, "subscribe", params))
    assert code(resp) == -32602
    assert resp["error"]["data"]["name"] == "invalid_params"


async def test_subscribe_results(ws: ClientConnection) -> None:
    await call(ws, req(1, "hello", HELLO))
    gyro = await call(ws, req(2, "subscribe", {"source": "gyro", "rate_hz": 9999}))
    assert gyro["result"]["mode"] == "continuous" and gyro["result"]["rate_hz"] == 1000
    ui = await call(ws, req(3, "subscribe", {"source": "ui", "rate_hz": 5, "min_delta": 0}))
    assert ui["result"]["mode"] == "on_change" and "rate_hz" not in ui["result"]
    resp = await call(ws, req(4, "get", {"source": "gyro", "type": "nope"}))
    assert code(resp) == -32602


@pytest.mark.parametrize(
    ("raw", "expected", "resp_id"),
    [
        ("hello", -32700, None),
        ('{"jsonrpc":"2.0","id":1,"method":"hello"} x', -32700, None),
        ('{"jsonrpc":"2.0","id":NaN,"method":"hello"}', -32700, None),
        ("[]", -32600, None),
        ("42", -32600, None),
        ('{"jsonrpc":"2.0","id":1.5,"method":"hello"}', -32600, None),
        ('{"jsonrpc":"2.0","id":1e3,"method":"hello"}', -32600, None),
        ('{"jsonrpc":"2.0","id":null,"method":"hello"}', -32600, None),
        ('{"jsonrpc":"1.0","id":1,"method":"hello"}', -32600, 1),
    ],
)
async def test_framing_errors(
    ws: ClientConnection, raw: str, expected: int, resp_id: int | None
) -> None:
    resp = await call(ws, raw)
    assert code(resp) == expected
    # A valid id is echoed even if the rest of the request is invalid.
    assert resp["id"] == resp_id
