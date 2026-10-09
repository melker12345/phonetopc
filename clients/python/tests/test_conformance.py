"""Replay spec/conformance/*.jsonl transcripts through the client's parsers.

Transcript format: see spec/conformance/README.md. Only ``msg`` entries are
replayed; ``raw``, ``binary`` and ``close`` entries are skipped.
Server-to-client messages are fed to :meth:`Phone._handle_message` exactly as
the reader task would; results are parsed with the typed model matching the
request method (looked up via the c2s request with the same id).

The checks only rely on what the README documents as stable (ids, methods,
results, error codes, ``seq`` starting at 0 without gaps), never on exact
values, timestamps or event counts. They also accept transcripts recorded
before draft 1 (no ``mode`` in subscribe results). Skips when no transcripts
exist yet.
"""

from __future__ import annotations

import asyncio
import itertools
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
    if method == "unsubscribe":
        assert result == {}
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
        if "msg" not in entry:
            # raw (non-JSON text), binary and close frames never reach the client's
            # message parser, so there is nothing to replay for them.
            assert {"raw", "binary", "close"} & entry.keys(), f"{path.name}:{n}: bad entry"
            continue
        lines.append(entry)
    return lines


def _id_key(req_id: Any) -> tuple[str, Any] | None:
    """Key for a well-formed request id (integer or string), else None."""
    if isinstance(req_id, bool) or not isinstance(req_id, (int, str)):
        return None
    return (type(req_id).__name__, req_id)


@pytest.mark.skipif(not TRANSCRIPTS, reason="no spec/conformance/*.jsonl transcripts yet")
@pytest.mark.parametrize("path", TRANSCRIPTS, ids=[p.name for p in TRANSCRIPTS])
async def test_replay_transcript(path: Path) -> None:
    phone = Phone(_NullWS(), request_timeout=None)  # type: ignore[arg-type]
    loop = asyncio.get_running_loop()
    # The client only ever uses integer ids, so every transcript id (which may
    # be a string) is mapped to a fresh client id before the response is fed in.
    client_ids = itertools.count(1)
    requests: dict[tuple[str, Any], tuple[int, str]] = {}
    futures: dict[int, asyncio.Future[Any]] = {}
    subscriptions: dict[int, phonetopc.Subscription] = {}
    expected: dict[str, int] = {}

    for entry in _load(path):
        msg = entry["msg"]
        if entry["dir"] == "c2s":
            key = _id_key(msg.get("id")) if isinstance(msg, dict) else None
            if key is None or not isinstance(msg.get("method"), str):
                continue  # deliberately malformed request or a notification
            cid = next(client_ids)
            requests[key] = (cid, msg["method"])
            futures[cid] = fut = loop.create_future()
            on_result = None
            params = msg.get("params")
            if (
                msg["method"] == "subscribe"
                and isinstance(params, dict)
                and isinstance(params.get("source"), str)
            ):
                # Mimic Subscription.start() without sending anything.
                sub = phone.subscribe(params["source"], queue_size=100_000)
                sub._started = True
                sub._has_data = asyncio.Event()
                on_result = sub._on_subscribed
                subscriptions[cid] = sub
            phone._pending[cid] = _Pending(fut, msg["method"], on_result)
            continue

        # s2c: every message must validate as JSON-RPC and go through the
        # reader's dispatch without raising.
        assert isinstance(msg, dict), f"server sent a non-object: {msg!r}"
        if msg.get("method") == "event":
            ev = phonetopc.Event.from_json(msg["params"])  # strict parse must succeed
            assert ev.subscription is not None
            if ev.subscription in phone._subs:
                expected[ev.subscription] = expected.get(ev.subscription, 0) + 1
            phone._handle_message(msg)
            continue

        key = _id_key(msg.get("id"))
        if key is None or key not in requests:
            phone._handle_message(msg)  # e.g. an error with id null
            continue
        cid, method = requests.pop(key)
        phone._handle_message({**msg, "id": cid})
        assert futures[cid].done(), f"response {msg['id']!r} was not correlated"

        if "result" in msg:
            _parse_result(method, msg["result"])
            assert futures[cid].exception() is None
            sub = subscriptions.get(cid)
            if sub is not None:
                assert sub.id == msg["result"]["subscription"]
                if "mode" in msg["result"]:  # draft 1
                    assert sub.mode == msg["result"]["mode"]
                assert sub.mode in (phonetopc.CONTINUOUS, phonetopc.ON_CHANGE)
                want = "drop_oldest" if sub.mode == phonetopc.CONTINUOUS else "error"
                assert sub.overflow == want
        else:
            err = futures[cid].exception()
            assert isinstance(err, phonetopc.PhonetopcError)
            assert err.code == msg["error"]["code"]
            if err.code in range(-32099, -32000 + 1):
                # Application errors should carry data.name (PROTOCOL.md section 7).
                assert err.name is not None

    for fut in futures.values():
        if fut.done():
            fut.exception()  # mark retrieved
    for sub in subscriptions.values():
        if sub.id is not None:
            # Every event for a registered subscription reached its queue,
            # with seq starting at 0 and no gaps.
            assert sub.received == expected.get(sub.id, 0)
            assert sub.dropped == 0
