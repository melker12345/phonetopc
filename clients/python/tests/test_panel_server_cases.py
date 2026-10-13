"""Run spec/conformance/panel-server-cases.json against the fake server.

Notifications and panels (PANELS.md), in dev mode: params rules, limits and
the order of checks. Format: spec/conformance/README.md, "Panel server cases".
Every case gets a fresh server, since panels belong to the (one) dev-mode PC.

The same cases also check the client's own validation (phonetopc.panels):
whatever a case expects to be -32602 at step 7 must be refused before sending.
"""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from typing import Any

import pytest
from fakeserver import REPO_ROOT, FakeServer
from test_server_cases import HELLO, _frame, _response, _same
from websockets.asyncio.client import ClientConnection, connect

from phonetopc.panels import request_problem

DOC: dict[str, Any] = json.loads(
    (REPO_ROOT / "spec" / "conformance" / "panel-server-cases.json").read_text()
)
CASES: list[dict[str, Any]] = DOC["cases"]


def test_case_names_are_unique() -> None:
    names = [c["name"] for c in CASES]
    assert len(names) == len(set(names))


@pytest.fixture
async def panel_server() -> AsyncIterator[FakeServer]:
    srv = FakeServer()
    # The fixture of server-cases.json, plus PANELS.md permissions.
    srv.unavailable = {"proximity"}
    srv.auto_stream = False
    assert DOC["fixture"]["permissions"] == {"notify": True, "panels": True}
    await srv.start()
    try:
        yield srv
    finally:
        await srv.stop()
    errors = [e for e in srv.schema_errors if not e.startswith("client sent")]
    assert not errors, "\n".join(errors)


def _equal(a: Any, b: Any) -> bool:
    """Deep JSON equality: 1 and 1.0 are the same number, but not the same as true."""
    if isinstance(a, dict) and isinstance(b, dict):
        return a.keys() == b.keys() and all(_equal(a[k], b[k]) for k in a)
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(map(_equal, a, b))
    if isinstance(a, bool) or isinstance(b, bool):
        return a is b
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return a == b
    return _same(a, b)


async def _step(ws: ClientConnection, where: str, step: dict[str, Any]) -> None:
    await ws.send(_frame(step["send"]))
    expect = step["expect"]
    r = await _response(ws, where)
    assert _same(r["id"], expect["id"]), f"{where}: id {r['id']!r}, want {expect['id']!r}"
    if "result" in expect:
        assert "result" in r and "error" not in r, f"{where}: want a result, got {r}"
        for key, value in expect.get("result_has", {}).items():
            assert _equal(r["result"].get(key), value), f"{where}: result.{key} in {r}"
        for cap in expect.get("result_capabilities", []):
            assert cap in r["result"]["capabilities"], f"{where}: no capability {cap} in {r}"
    else:
        assert "error" in r, f"{where}: want an error, got {r}"
        err = r["error"]
        assert err["code"] == expect["error"], f"{where}: got {r}"
        assert err.get("data", {}).get("name") == expect["name"], f"{where}: got {r}"
        for key, value in expect.get("data", {}).items():
            assert err["data"].get(key) == value, f"{where}: data.{key} in {r}"


@pytest.mark.parametrize("case", CASES, ids=[c["name"] for c in CASES])
async def test_panel_server_case(panel_server: FakeServer, case: dict[str, Any]) -> None:
    if case.get("permissions") == "off":
        panel_server.permissions = {"notify": False, "panels": False}
    async with connect(f"ws://{panel_server.addr}") as ws:
        if case["hello"]:
            await ws.send(json.dumps(HELLO))
            r = await _response(ws, "hello")
            assert r["id"] == HELLO["id"] and "result" in r, r
        for i, step in enumerate(case.get("setup", [])):
            await _step(ws, f"setup[{i}]", step)
        await _step(ws, "send", case)


def _client_side(case: dict[str, Any]) -> tuple[str, dict[str, Any]] | None:
    send = case["send"]
    if not isinstance(send, dict) or not isinstance(send.get("params"), dict):
        return None
    return send["method"], send["params"]


@pytest.mark.parametrize("case", CASES, ids=[c["name"] for c in CASES])
def test_client_validation_agrees(case: dict[str, Any]) -> None:
    """request_problem refuses exactly the step-7 errors of the cases.

    A step-8 -32602 (an unknown widget, say) needs the panel's state, so
    those cases (with a setup) only have to pass the local check.
    """
    found = _client_side(case)
    if found is None:
        return
    method, params = found
    problem = request_problem(method, params)
    expect = case["expect"]
    if expect.get("error") == -32602 and not case.get("setup"):
        assert problem is not None, f"{case['name']}: the client would send it"
    elif "result" in expect or expect.get("error") != -32602:
        assert problem is None, f"{case['name']}: the client refuses it: {problem}"
