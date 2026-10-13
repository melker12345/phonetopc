"""Notifications and panels (PANELS.md) against the fake phone."""

from __future__ import annotations

import asyncio
import time
from typing import Any

import pytest
from fakeserver import FakeServer

import phonetopc
import phonetopc.sync
from phonetopc import ui
from phonetopc.panels import Panel, PanelEvent

DECK = ui.grid(
    3,
    ui.button("mute", "Mute", tone="bad"),
    ui.toggle("mic", "Mic", value=True),
    ui.slider("vol", label="Volume", min=0, max=100, step=1, value=40),
    ui.text("Idle", id="status"),
    ui.text_input("say", label="Say"),
)
PAD = ui.column(ui.canvas("ink", color="#ff0000"), ui.button("clear", "Clear"))


async def next_event(panel: Panel) -> PanelEvent:
    return await asyncio.wait_for(panel.__anext__(), 2)


async def wait_until(cond: Any, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        assert time.monotonic() < deadline, "condition never became true"
        await asyncio.sleep(0.005)


def test_builders_leave_out_unset_members() -> None:
    assert ui.button("b", "Go") == {"type": "button", "id": "b", "label": "Go"}
    assert ui.progress() == {"type": "progress", "value": None}
    assert DECK["cols"] == 3 and len(DECK["children"]) == 5
    assert phonetopc.panels.layout_problem(DECK) is None


# -- notifications ------------------------------------------------------------


async def test_notify(server: FakeServer, phone: phonetopc.Phone) -> None:
    nid = await phone.notify(
        "Claude Code is done", "3 files changed.\nAll tests pass.", actions={"open": "Open"}
    )
    (req,) = server.requests("notify")
    assert req["params"] == {
        "title": "Claude Code is done",
        "body": "3 files changed.\nAll tests pass.",
        "actions": [{"id": "open", "label": "Open"}],
    }
    assert server.notifications["dev"][nid]["title"] == "Claude Code is done"
    assert await phone.notify("Again", id="claude", priority="high") == "claude"
    await phone.notify_cancel("claude")
    assert "claude" not in server.notifications["dev"]


async def test_notification_events(server: FakeServer, phone: phonetopc.Phone) -> None:
    nid = await phone.notify("Deploy?", actions=[("yes", "Yes"), ("no", "No")])
    async with phone.subscribe("notification") as events:
        await server.notification_event(nid, "action", action="yes")
        ev = await asyncio.wait_for(events.__anext__(), 2)
    assert (ev.type, ev.value) == ("action", {"notification": nid, "action": "yes"})


async def test_notify_checks_before_sending(server: FakeServer, phone: phonetopc.Phone) -> None:
    for kwargs in (
        {"title": ""},
        {"title": "a\nb"},
        {"title": "evil‮"},
        {"title": "t", "actions": {str(i): "x" for i in range(4)}},
        {"title": "t", "id": "has space"},
    ):
        with pytest.raises(ValueError):
            await phone.notify(**kwargs)
    assert not server.requests("notify")


async def test_notify_not_allowed(server: FakeServer, phone: phonetopc.Phone) -> None:
    server.permissions["notify"] = False
    with pytest.raises(phonetopc.NotAllowed) as err:
        await phone.notify("Hi")
    assert err.value.permission == "notify"
    assert "Notifications" in str(err.value)


async def test_notify_rate_limited(server: FakeServer, phone: phonetopc.Phone) -> None:
    server.notify_limit = 1
    await phone.notify("one")
    with pytest.raises(phonetopc.RateLimited):
        await phone.notify("two")


# -- panels -------------------------------------------------------------------


async def test_panel_events(server: FakeServer, phone: phonetopc.Phone) -> None:
    panel = await phone.show_panel("deck", "Stream deck", DECK, keep_awake=True)
    assert (panel.id, panel.visible, panel.last_pseq) == ("deck", False, -1)
    # The client subscribed to the panel source before showing (PANELS.md 2).
    assert server.received.index(server.requests("subscribe")[0]) < server.received.index(
        server.requests("panel_show")[0]
    )

    await server.set_state("deck", "visible")
    await server.press("deck", "mute")
    await server.change("deck", "mic", False)
    await server.change("deck", "vol", 72, final=True)
    await server.submit("deck", "say", "hello")

    state = await next_event(panel)
    assert isinstance(state, phonetopc.StateEvent) and state.state == "visible"
    assert panel.visible
    press = await next_event(panel)
    assert isinstance(press, phonetopc.PressEvent)
    assert (press.widget, press.pseq) == ("mute", 1)
    mic = await next_event(panel)
    assert isinstance(mic, phonetopc.ChangeEvent) and mic.value is False and not mic.final
    vol = await next_event(panel)
    assert isinstance(vol, phonetopc.ChangeEvent) and (vol.value, vol.final) == (72, True)
    say = await next_event(panel)
    assert isinstance(say, phonetopc.SubmitEvent) and say.value == "hello"
    assert panel.last_pseq == 4


async def test_events_go_to_their_panel(server: FakeServer, phone: phonetopc.Phone) -> None:
    deck = await phone.show_panel("deck", "Deck", DECK)
    pad = await phone.show_panel("pad", "Pad", PAD)
    await server.press("pad", "clear")
    await server.press("deck", "mute")
    assert (await next_event(deck)).widget == "mute"
    assert (await next_event(pad)).widget == "clear"
    assert [p.panel for p in await phone.list_panels()] == ["deck", "pad"]


async def test_strokes(server: FakeServer, phone: phonetopc.Phone) -> None:
    panel = await phone.show_panel("pad", "Pad", PAD)
    stroke = await server.draw("pad", "ink", [(0.1, 0.1), (0.2, 0.2), (0.3, 0.3)])
    events = [await next_event(panel) for _ in range(3)]
    assert all(isinstance(e, phonetopc.StrokeEvent) for e in events)
    start, move, end = events
    assert isinstance(start, phonetopc.StrokeEvent)
    assert isinstance(move, phonetopc.StrokeEvent)
    assert isinstance(end, phonetopc.StrokeEvent)
    assert [e.phase for e in (start, move, end)] == ["start", "move", "end"]
    assert start.stroke == stroke and start.size == (360.0, 640.0) and move.size is None
    assert start.points[1] == phonetopc.Point(0.2, 0.2, 0.5, 8)
    assert start.points[1].x == 0.2 and end.points == ()

    await panel.clear_canvas("ink")
    assert server.panel("pad").strokes == []


async def test_update(server: FakeServer, phone: phonetopc.Phone) -> None:
    panel = await phone.show_panel("deck", "Deck", DECK)
    await panel.update("status", text="Claude: done", tone="good")
    await panel.update_many({"mic": {"value": False}, "vol": {"value": 10}})
    widgets = server.panel("deck").widgets
    assert widgets["status"]["text"] == "Claude: done"
    assert server.panel("deck").values() == {"mic": False, "vol": 10}

    sent = len(server.requests("panel_update"))
    with pytest.raises(ValueError, match="no widget"):
        await panel.update("nope", text="x")
    with pytest.raises(ValueError, match="must be a bool"):
        await panel.update("mic", value="yes")
    with pytest.raises(ValueError, match="can't be changed"):
        await panel.update("status", type="button")
    assert len(server.requests("panel_update")) == sent
    # The local copy of the layout follows the updates, so later checks use them.
    with pytest.raises(ValueError, match="min must be less than max"):
        await panel.update("vol", min=100)


async def test_show_panel_checks_layout(server: FakeServer, phone: phonetopc.Phone) -> None:
    with pytest.raises(ValueError, match="needs an id"):
        await phone.show_panel("p", "T", {"type": "button", "label": "x"})
    with pytest.raises(ValueError, match="unknown widget type"):
        await phone.show_panel("p", "T", {"type": "image", "id": "i"})
    assert not server.requests("panel_show")
    # validate=False sends it anyway; the phone has the last word.
    with pytest.raises(phonetopc.InvalidParams):
        await phone.show_panel("p", "T", {"type": "image", "id": "i"}, validate=False)
    server.schema_errors.clear()  # that request was invalid on purpose


async def test_close(server: FakeServer, phone: phonetopc.Phone) -> None:
    async with await phone.show_panel("deck", "Deck", DECK) as panel:
        pass
    assert panel.closed and "deck" not in server.panels["dev"]
    with pytest.raises(StopAsyncIteration):
        await next_event(panel)
    with pytest.raises(phonetopc.UnknownPanel):
        await phone.attach_panel("deck")


async def test_closed_by_user(server: FakeServer, phone: phonetopc.Phone) -> None:
    panel = await phone.show_panel("deck", "Deck", DECK)
    await server.set_permission("panels", False)
    ev = await next_event(panel)
    assert isinstance(ev, phonetopc.StateEvent)
    assert (ev.state, ev.reason) == ("closed", "revoked")
    assert [e async for e in panel] == []
    assert panel.closed


async def test_replaced_panel_ends(server: FakeServer, phone: phonetopc.Phone) -> None:
    old = await phone.show_panel("deck", "Deck", DECK)
    new = await phone.show_panel("deck", "Deck 2", DECK)
    await server.press("deck", "mute")
    assert [e async for e in old] == []
    assert (await next_event(new)).widget == "mute"


async def test_too_many_panels(server: FakeServer, phone: phonetopc.Phone) -> None:
    for i in range(8):
        await phone.show_panel(f"p{i}", "T", PAD)
    with pytest.raises(phonetopc.TooManyPanels):
        await phone.show_panel("p8", "T", PAD)


async def test_panels_not_allowed(server: FakeServer, phone: phonetopc.Phone) -> None:
    server.permissions["panels"] = False
    with pytest.raises(phonetopc.NotAllowed) as err:
        await phone.show_panel("deck", "Deck", DECK)
    assert err.value.permission == "panels"
    assert "Screen panels" in str(err.value)


async def test_reattach_replays_missed_strokes(server: FakeServer) -> None:
    async with phonetopc.connect(server.addr, dev=True) as phone:
        panel = await phone.show_panel("pad", "Pad", ui.column(ui.canvas("ink"), DECK))
        await server.draw("pad", "ink", [(0.1, 0.1), (0.2, 0.2), (0.3, 0.3)])
        for _ in range(3):
            await next_event(panel)
        await server.change("pad", "mic", False)
        await next_event(panel)
    assert panel.closed and panel.last_pseq == 3

    await wait_until(lambda: server.panel("pad").owner is None)  # detached
    await server.draw("pad", "ink", [(0.5, 0.5), (0.6, 0.6)])  # drawn while offline

    async with phonetopc.connect(server.addr, dev=True) as phone:
        again = await phone.attach_panel("pad", after_pseq=panel.last_pseq)
        assert again.attach_result is not None
        assert again.attach_result.last_pseq == 5
        assert again.attach_result.values == {"mic": False, "vol": 40}
        replayed = [await next_event(again) for _ in range(2)]
        assert [e.pseq for e in replayed] == [4, 5]
        assert all(e.replayed for e in replayed)
        assert again.last_pseq == 5

        # From scratch: every stroke since the last clear.
        fresh = await phone.attach_panel("pad")
        assert [(await next_event(fresh)).pseq for _ in range(5)] == [0, 1, 2, 4, 5]


def test_sync_panel(threaded_server: FakeServer) -> None:
    with phonetopc.sync.connect(threaded_server.addr, dev=True) as phone:
        assert phone.notify("Hi", id="n") == "n"
        panel = phone.show_panel("deck", "Deck", DECK)
        assert [p.panel for p in phone.list_panels()] == ["deck"]
        panel.update("status", text="Busy")
        loop = phone._runner.loop
        asyncio.run_coroutine_threadsafe(threaded_server.press("deck", "mute"), loop).result(2)
        ev = next(iter(panel))
        assert isinstance(ev, phonetopc.PressEvent) and ev.widget == "mute"
        assert panel.last_pseq == 0
        panel.close()
        assert list(panel) == []
