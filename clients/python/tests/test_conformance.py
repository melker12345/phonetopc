"""Replay spec/conformance/*.jsonl transcripts through the client's parsers.

Transcript format: one ``{"dir": "c2s" | "s2c", "msg": {...}}`` per line.
Server-to-client messages are fed to :meth:`Phone._handle_message` exactly as
the reader task would; results are parsed with the typed model matching the
request method (looked up via the c2s request with the same id). Skips when
no transcripts exist yet.
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest
from fakeserver import REPO_ROOT

import phonetopc
from phonetopc.client import Phone, _Pending

CONFORMANCE_DIR = REPO_ROOT / "spec" / "conformance"
TRANSCRIPTS = sorted(CONFORMANCE_DIR.glob("*.jsonl")) if CONFORMANCE_DIR.is_dir() else []


def _parse_result(method: str, result: Any) -> Any:
    if method == "hello":
        return phonetopc.HelloResult.from_json(result)
    if method == "list_sensors":
        return [phonetopc.SensorInfo.from_json(s) for s in result["sensors"]]
    if method == "subscribe":
        return phonetopc.SubscribeResult.from_json(result)
    if method == "get":
        return phonetopc.Event.from_json(result, notification=False)
    return result


class _NullWS:
    """Stands in for the WebSocket; replay never sends."""


def _load(path: Path) -> list[dict[str, Any]]:
    lines = []
    for n, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip():
            continue
        entry = json.loads(line)
        assert entry.get("dir") in ("c2s", "s2c"), f"{path.name}:{n}: bad dir"
        assert isinstance(entry.get("msg"), (dict, list)), f"{path.name}:{n}: bad msg"
        lines.append(entry)
    return lines


@pytest.mark.skipif(not TRANSCRIPTS, reason="no spec/conformance/*.jsonl transcripts yet")
@pytest.mark.parametrize("path", TRANSCRIPTS, ids=[p.name for p in TRANSCRIPTS])
async def test_replay_transcript(path: Path) -> None:
    phone = Phone(_NullWS(), request_timeout=None)  # type: ignore[arg-type]
    loop = asyncio.get_running_loop()
    methods: dict[Any, str] = {}
    futures: dict[Any, asyncio.Future[Any]] = {}
    subscriptions: dict[str, phonetopc.Subscription] = {}
    expected: dict[str, int] = {}

    for entry in _load(path):
        msg = entry["msg"]
        if entry["dir"] == "c2s":
            if isinstance(msg, dict) and "id" in msg and "method" in msg:
                methods[msg["id"]] = msg["method"]
                fut = loop.create_future()
                futures[msg["id"]] = fut
                on_result = None
                if msg["method"] == "subscribe":
                    # Mimic Subscription.start() without sending anything.
                    sub = phone.subscribe(msg["params"]["source"], queue_size=100_000)
                    sub._started = True
                    sub._has_data = asyncio.Event()
                    on_result = sub._on_subscribed
                    subscriptions[str(msg["id"])] = sub
                if isinstance(msg["id"], int):
                    phone._pending[msg["id"]] = _Pending(fut, msg["method"], on_result)
            continue

        # s2c: everything must go through the reader's dispatch without raising.
        if isinstance(msg, dict) and msg.get("method") == "event":
            ev = phonetopc.Event.from_json(msg["params"])  # strict parse must succeed
            assert ev.subscription is not None
            if ev.subscription in phone._subs:
                expected[ev.subscription] = expected.get(ev.subscription, 0) + 1
        phone._handle_message(msg)
        if isinstance(msg, dict) and isinstance(msg.get("id"), int) and msg["id"] in futures:
            assert futures[msg["id"]].done(), f"response {msg['id']} was not correlated"

        if isinstance(msg, dict) and "id" in msg and msg["id"] in methods:
            req_id = msg["id"]
            if "result" in msg:
                _parse_result(methods[req_id], msg["result"])
            else:
                err = phonetopc.errors.error_from_response(msg["error"])
                assert isinstance(err, phonetopc.PhonetopcError)
                if err.code in range(-32099, -32000 + 1):
                    # Application errors should carry data.name (PROTOCOL.md section 7).
                    assert err.name is not None

    for fut in futures.values():
        if fut.done():
            fut.exception()  # mark retrieved
    # Every event for a registered subscription reached its queue.
    for sub in subscriptions.values():
        if sub.id is not None:
            assert sub.received == expected.get(sub.id, 0)
