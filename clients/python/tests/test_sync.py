from __future__ import annotations

import threading

import pytest
from fakeserver import DEVICE, FakeServer

import phonetopc
import phonetopc.sync


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
