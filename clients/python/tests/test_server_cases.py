"""Run spec/conformance/server-cases.json against the fake server.

The suite (format: spec/conformance/README.md) pins down how a server answers
malformed and out-of-order requests, so the fake that the client tests rely on
behaves like the Go mock and the Android app.
"""

from __future__ import annotations

import asyncio
import base64
import json
from collections.abc import AsyncIterator
from typing import Any

import pytest
from fakeserver import REPO_ROOT, FakeServer
from websockets.asyncio.client import ClientConnection, connect
from websockets.exceptions import ConnectionClosed

CASES: list[dict[str, Any]] = json.loads(
    (REPO_ROOT / "spec" / "conformance" / "server-cases.json").read_text()
)

HELLO = {
    "jsonrpc": "2.0",
    "id": "conformance-hello",
    "method": "hello",
    "params": {"protocol": "phonetopc/1", "client": {"name": "conformance", "version": "1"}},
}
PROBE = {"jsonrpc": "2.0", "id": "conformance-probe", "method": "list_sensors"}
TIMEOUT = 5.0


def test_case_names_are_unique() -> None:
    names = [c["name"] for c in CASES]
    assert len(names) == len(set(names))


@pytest.fixture
async def conformance_server() -> AsyncIterator[FakeServer]:
    srv = FakeServer()
    # The fixture the suite assumes: proximity unavailable, battery has a
    # value, ui has none.
    srv.unavailable = {"proximity"}
    srv.auto_stream = False
    await srv.start()
    try:
        yield srv
    finally:
        await srv.stop()
    # The cases send invalid requests on purpose; only what the fake sends
    # must be schema-valid.
    errors = [e for e in srv.schema_errors if not e.startswith("client sent")]
    assert not errors, "\n".join(errors)


def _frame(send: Any) -> str | bytes:
    if isinstance(send, dict) and len(send) == 1:
        if "raw" in send:
            return str(send["raw"])
        if "binary" in send:
            return base64.b64decode(send["binary"])
    return json.dumps(send)


def _same(a: Any, b: Any) -> bool:
    """JSON equality: 1 and "1" differ, and so do 1 and true."""
    return type(a) is type(b) and a == b


async def _response(ws: ClientConnection, where: str) -> dict[str, Any]:
    """The next response, skipping notifications."""
    while True:
        raw = await asyncio.wait_for(ws.recv(), TIMEOUT)
        msg = json.loads(raw)
        assert isinstance(msg, dict), f"{where}: server sent {raw!r}"
        if "id" in msg:
            return msg


async def _step(ws: ClientConnection, where: str, step: dict[str, Any]) -> None:
    await ws.send(_frame(step["send"]))
    expect = step["expect"]
    if "close" in expect:
        with pytest.raises(ConnectionClosed) as closed:
            await _response(ws, where)
        rcvd = closed.value.rcvd
        assert rcvd is not None and rcvd.code == expect["close"], f"{where}: {closed.value}"
    elif expect.get("no_reply"):
        await ws.send(json.dumps(PROBE))
        r = await _response(ws, where)
        assert r["id"] == PROBE["id"], f"{where}: want no reply, got {r}"
    elif "result" in expect:
        r = await _response(ws, where)
        assert "result" in r and "error" not in r, f"{where}: want a result, got {r}"
        assert _same(r["id"], expect["id"]), f"{where}: id {r['id']!r}, want {expect['id']!r}"
    elif "error" in expect:
        r = await _response(ws, where)
        assert "error" in r, f"{where}: want an error, got {r}"
        err = r["error"]
        assert err["code"] == expect["error"], f"{where}: got {r}"
        assert err.get("data", {}).get("name") == expect["name"], f"{where}: got {r}"
        assert _same(r["id"], expect["id"]), f"{where}: id {r['id']!r}, want {expect['id']!r}"
        for key, value in expect.get("data", {}).items():
            assert err["data"].get(key) == value, f"{where}: data.{key} in {r}"
    else:
        raise AssertionError(f"{where}: unknown expect form {expect}")


@pytest.mark.parametrize("case", CASES, ids=[c["name"] for c in CASES])
async def test_server_case(conformance_server: FakeServer, case: dict[str, Any]) -> None:
    async with connect(f"ws://{conformance_server.addr}") as ws:
        if case["hello"]:
            await ws.send(json.dumps(HELLO))
            r = await _response(ws, "hello")
            assert r["id"] == HELLO["id"] and "result" in r, r
        for i, step in enumerate(case.get("setup", [])):
            await _step(ws, f"setup[{i}]", step)
        await _step(ws, "send", case)
