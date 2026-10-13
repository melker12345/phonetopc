from __future__ import annotations

import threading
import time
from collections.abc import Callable

import pytest
from fakeserver import DEVICE, FakeServer

import phonetopc
import phonetopc.sync
from phonetopc import ui


def test_sync_roundtrip(threaded_server: FakeServer) -> None:
    with phonetopc.sync.connect(threaded_server.addr, dev=True, client_name="sync") as phone:
        assert phone.device.id == DEVICE["id"]
        assert any(s.source == "gyro" for s in phone.list_sensors())
        assert phone.get("battery").value["level"] == 82
        with pytest.raises(phonetopc.UnknownSource):
            phone.get("nope")
        with phone.subscribe("gyro", rate_hz=500) as events:
            seqs = []
            for ev in events:
                seqs.append(ev.seq)
                if len(seqs) == 5:
                    break
        assert seqs == [0, 1, 2, 3, 4]
        assert events.dropped == 0
    assert phone.closed
    assert threaded_server.requests("unsubscribe")


def test_sync_iterator_ends_on_disconnect(threaded_server: FakeServer) -> None:
    threaded_server.auto_stream = False
    with (
        phonetopc.sync.connect(threaded_server.addr, dev=True) as phone,
        phone.subscribe("light") as ev,
    ):
        threaded_server.close_on.add("get")
        with pytest.raises(phonetopc.ConnectionClosed):
            phone.get("battery")
        assert list(ev) == []


def test_sync_connect_failure_cleans_up() -> None:
    before = threading.active_count()
    with pytest.raises(OSError):
        phonetopc.sync.connect("127.0.0.1:1", dev=True, open_timeout=2)
    assert threading.active_count() == before


# -- handshake info, raw calls, lifecycle ---------------------------------------


def test_sync_handshake_and_call(threaded_server: FakeServer) -> None:
    with phonetopc.sync.connect(
        threaded_server.addr, dev=True, client_name="sync", client_version="9.9.9"
    ) as phone:
        (hello,) = threaded_server.requests("hello")
        assert hello["params"]["client"] == {"name": "sync", "version": "9.9.9"}
        assert phone.server.name == "fake"
        assert phone.capabilities == ("events", "get", "notify", "panels")
        assert not phone.secure and phone.paired_device is None and phone.pc is None
        assert phone.url is not None and phone.url.startswith("ws://")
        assert phone.call("get", {"source": "battery"}, timeout=2)["value"]["level"] == 82
        phone.request_timeout = 3.0
        assert phone.phone.request_timeout == 3.0
        assert phone.closed_reason is None
        assert repr(phone).startswith("<SyncPhone 'Fake Phone'")
    phone.wait_closed()  # returns at once, after close() stopped the loop
    assert phone.closed and phone.closed_reason is not None


def test_sync_wait_closed(threaded_server: FakeServer) -> None:
    with phonetopc.sync.connect(threaded_server.addr, dev=True) as phone:
        threaded_server.run_threadsafe(threaded_server.drop_connections())
        phone.wait_closed()
        assert phone.closed
        assert isinstance(phone.closed_reason, phonetopc.ConnectionClosed)


def test_sync_subscription_info(threaded_server: FakeServer) -> None:
    with phonetopc.sync.connect(threaded_server.addr, dev=True) as phone:
        sub = phone.subscribe("gyro", rate_hz=50, queue_size=16)
        assert (sub.id, sub.mode, sub.rate_hz, sub.info) == (None, None, None, None)
        with sub:
            assert sub.id is not None and sub.mode == phonetopc.CONTINUOUS
            assert sub.info is not None and sub.rate_hz == sub.info.rate_hz
            assert (sub.source, sub.type, sub.requested_rate_hz) == ("gyro", None, 50)
            assert (sub.queue_size, sub.overflow, sub.min_delta) == (16, "drop_oldest", None)
            next(sub)
            assert sub.received >= 1 and sub.dropped_local == 0 and sub.dropped_remote == 0
            assert sub.qsize() >= 0 and not sub.closed
        assert sub.closed and sub.closed_reason is None


# -- notifications and panels ---------------------------------------------------

DECK = ui.column(
    ui.text("Idle", id="status"),
    ui.toggle("mic", "Mic", value=True),
    ui.slider("vol", min=0, max=100, value=40),
    ui.canvas("ink"),
)


def wait_until(cond: Callable[[], bool], timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        assert time.monotonic() < deadline, "condition never became true"
        time.sleep(0.005)


def test_sync_notify(threaded_server: FakeServer) -> None:
    srv = threaded_server
    with phonetopc.sync.connect(srv.addr, dev=True) as phone:
        nid = phone.notify("Build done", "All green", actions={"open": "Open"}, priority="high")
        assert srv.notifications["dev"][nid]["priority"] == "high"
        with phone.subscribe("notification") as events:
            srv.run_threadsafe(srv.notification_event(nid, "action", action="open"))
            ev = next(events)
        assert (ev.type, ev.value) == ("action", {"notification": nid, "action": "open"})

        assert phone.notify("Again", id="fixed") == "fixed"
        phone.notify_cancel("fixed")
        assert "fixed" not in srv.notifications["dev"]

        with pytest.raises(ValueError):
            phone.notify("two\nlines")
        srv.permissions["notify"] = False
        with pytest.raises(phonetopc.NotAllowed):
            phone.notify("Hi")


def test_sync_panel_lifecycle(threaded_server: FakeServer) -> None:
    srv = threaded_server
    with phonetopc.sync.connect(srv.addr, dev=True) as phone:
        with phone.show_panel("deck", "Deck", DECK, keep_awake=True) as panel:
            assert (panel.id, panel.title, panel.visible) == ("deck", "Deck", False)
            assert panel.last_pseq == -1
            assert panel.layout is not None and panel.attach_result is None
            assert repr(panel).startswith("<SyncPanel 'deck'")

            panel.update("status", text="Busy", tone="warn")
            panel.update_many({"mic": {"value": False}, "vol": {"value": 7}})
            assert srv.panel("deck").widgets["status"]["text"] == "Busy"
            assert srv.panel("deck").values() == {"mic": False, "vol": 7}
            with pytest.raises(ValueError, match="no widget"):
                panel.update("nope", text="x")

            srv.run_threadsafe(srv.set_state("deck", "visible"))
            srv.run_threadsafe(srv.draw("deck", "ink", [(0.1, 0.1), (0.2, 0.2)], chunk=2))
            state = next(panel)
            assert isinstance(state, phonetopc.StateEvent) and panel.visible
            strokes = [next(panel) for _ in range(2)]
            assert [s.phase for s in strokes if isinstance(s, phonetopc.StrokeEvent)] == [
                "start",
                "end",
            ]
            assert panel.last_pseq == 2
            panel.clear_canvas("ink")
            assert srv.panel("deck").strokes == []
        assert panel.closed and panel.closed_reason is None
        assert "deck" not in srv.panels["dev"]
        assert list(panel) == []

        with pytest.raises(ValueError, match="unknown widget type"):
            phone.show_panel("bad", "Bad", {"type": "image", "id": "i"})
        with pytest.raises(phonetopc.UnknownPanel):
            phone.attach_panel("deck")


def test_sync_panel_closed_by_user(threaded_server: FakeServer) -> None:
    srv = threaded_server
    with phonetopc.sync.connect(srv.addr, dev=True) as phone:
        panel = phone.show_panel("deck", "Deck", DECK)
        srv.run_threadsafe(srv.set_permission("panels", False))
        events = list(panel)
        assert len(events) == 1 and isinstance(events[0], phonetopc.StateEvent)
        assert (events[0].state, events[0].reason) == ("closed", "revoked")
        assert panel.closed
        with pytest.raises(phonetopc.NotAllowed):
            phone.show_panel("deck", "Deck", DECK)


def test_sync_attach_panel(threaded_server: FakeServer) -> None:
    srv = threaded_server
    with phonetopc.sync.connect(srv.addr, dev=True) as phone:
        panel = phone.show_panel("pad", "Pad", DECK)
        srv.run_threadsafe(srv.press("pad", "mic"))
        next(panel)
        last = panel.last_pseq
    wait_until(lambda: srv.panel("pad").owner is None)  # detached
    srv.run_threadsafe(srv.draw("pad", "ink", [(0.5, 0.5)], chunk=1))  # while away

    with phonetopc.sync.connect(srv.addr, dev=True) as phone:
        assert [p.panel for p in phone.list_panels()] == ["pad"]
        again = phone.attach_panel("pad", after_pseq=last)
        assert again.attach_result is not None and again.layout is None
        replayed = [next(again) for _ in range(2)]
        assert all(e.replayed for e in replayed)
        assert again.last_pseq == replayed[-1].pseq > last
        again.close()
        assert phone.list_panels() == []
