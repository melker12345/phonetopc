"""examples/stream_deck.py, drawpad.py and notify_when_done.py against the fake phone."""

from __future__ import annotations

import asyncio
import importlib.util
import queue
import sys
import time
from types import ModuleType
from typing import Any

import pytest
from fakeserver import REPO_ROOT, FakeServer

import phonetopc


def load_example(name: str) -> ModuleType:
    path = REPO_ROOT / "examples" / f"{name}.py"
    spec = importlib.util.spec_from_file_location(f"example_{name}", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # dataclasses look their module up
    spec.loader.exec_module(module)
    return module


async def wait_until(cond: Any, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        assert time.monotonic() < deadline, "condition never became true"
        await asyncio.sleep(0.005)


async def test_stream_deck(server: FakeServer, phone: phonetopc.Phone) -> None:
    deck = load_example("stream_deck")
    deck.POLL_S = 60
    audio = deck.Audio()
    task = asyncio.ensure_future(deck.run(phone, audio))
    await wait_until(lambda: "stream_deck" in server.panels.get("dev", {}))
    widgets = server.panel("stream_deck").widgets
    assert widgets["vol"]["value"] == 40 and widgets["mic"]["value"] is True

    await server.press("stream_deck", "mute")
    await wait_until(lambda: widgets["mute"]["label"] == "🔇 Unmute")
    assert audio.muted()
    await server.change("stream_deck", "vol", 72, final=True)
    await server.change("stream_deck", "mic", False)
    await wait_until(lambda: audio.volume() == 72 and not audio.mic_on())

    await server.set_state("stream_deck", "closed", reason="user")
    await asyncio.wait_for(task, 2)  # returns: the user closed it


async def test_stream_deck_raises_when_disconnected(server: FakeServer) -> None:
    deck = load_example("stream_deck")
    phone = await phonetopc.connect(server.addr, dev=True)
    task = asyncio.ensure_future(deck.run(phone, deck.Audio()))
    await wait_until(lambda: "stream_deck" in server.panels.get("dev", {}))
    await server.drop_connections()
    with pytest.raises(phonetopc.ConnectionClosed):
        await asyncio.wait_for(task, 2)


async def test_drawpad(server: FakeServer, phone: phonetopc.Phone) -> None:
    pad = load_example("drawpad")
    out: queue.Queue[Any] = queue.Queue()
    task = asyncio.ensure_future(pad.follow(phone, out, -1))
    await wait_until(lambda: "drawpad" in server.panels.get("dev", {}))
    await server.draw("drawpad", "ink", [(0.0, 0.0), (0.5, 0.5), (1.0, 1.0)], size=(400, 800))
    await server.press("drawpad", "clear")
    await wait_until(lambda: bool(server.requests("canvas_clear")))
    await server.set_state("drawpad", "closed", reason="user")
    assert await asyncio.wait_for(task, 2) is None

    items = []
    while not out.empty():
        items.append(out.get())
    assert isinstance(items[0], pad.Clear) and isinstance(items[-1], pad.Clear)
    strokes = items[1:-1]
    assert [s.phase for s in strokes] == ["start", "move", "end"]

    drawn: list[Any] = []
    deleted: list[str] = []
    sizes: list[tuple[float, float]] = []
    mirror = pad.Mirror(360, 720, drawn.append, deleted.append, lambda w, h: sizes.append((w, h)))
    for item in items:
        mirror.handle(item)
    assert sizes == [(360.0, 720)]  # aspect 400:800
    # The first point starts the line; the next two each draw a segment.
    assert [d[1:5] for d in drawn] == [
        (0.0, 0.0, 0.0, 0.0),
        (0.0, 0.0, 180.0, 360.0),
        (180.0, 360.0, 360.0, 720.0),
    ]
    assert deleted == ["all", "all"]


async def test_drawpad_reattaches(server: FakeServer) -> None:
    pad = load_example("drawpad")
    out: queue.Queue[Any] = queue.Queue()
    async with phonetopc.connect(server.addr, dev=True) as phone:
        task = asyncio.ensure_future(pad.follow(phone, out, -1))
        await wait_until(lambda: "drawpad" in server.panels.get("dev", {}))
        await server.draw("drawpad", "ink", [(0.1, 0.1), (0.2, 0.2)])
        await wait_until(lambda: out.qsize() == 3)  # Clear, start, end
    last = await asyncio.wait_for(task, 2)  # the connection ended
    assert last == 1
    await wait_until(lambda: server.panel("drawpad").owner is None)
    await server.draw("drawpad", "ink", [(0.3, 0.3), (0.4, 0.4)])  # while offline
    async with phonetopc.connect(server.addr, dev=True) as phone:
        task = asyncio.ensure_future(pad.follow(phone, out, last))
        await wait_until(lambda: out.qsize() == 5)
        task.cancel()
    replayed = [out.get() for _ in range(5)][3:]
    assert [(e.pseq, e.replayed) for e in replayed] == [(2, True), (3, True)]


def test_notify_when_done(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    notify = load_example("notify_when_done")
    addr = f"ws://{threaded_server.addr}"
    fail = [sys.executable, "-c", "raise SystemExit(3)"]

    assert notify.main(["--addr", addr, "--", *fail]) == 3
    (req,) = threaded_server.requests("notify")
    assert req["params"]["title"].startswith("❌ python")
    assert req["params"]["title"].endswith("failed (exit 3)")

    assert notify.main(["--addr", addr, "--panel", "--", sys.executable, "-c", "pass"]) == 0
    assert threaded_server.requests("panel_show")
    update = threaded_server.requests("panel_update")[-1]["params"]["updates"]
    assert {"widget": "bar", "set": {"value": 1, "tone": "good"}} in update
    assert threaded_server.requests("notify")[-1]["params"]["title"].startswith("✅")

    assert notify.main(["--addr", addr, "--title", "Claude Code is done", "--id", "cc"]) == 0
    assert threaded_server.requests("notify")[-1]["params"]["id"] == "cc"

    threaded_server.permissions["notify"] = False
    assert notify.main(["--addr", addr, "--", *fail]) == 3  # the command's status wins
    assert notify.main(["--addr", addr, "--title", "x"]) == 2
    assert "Notifications" in capsys.readouterr().err
