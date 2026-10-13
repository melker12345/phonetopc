from __future__ import annotations

import asyncio
import gc
import json
from collections.abc import Callable

import pytest
from fakeserver import DEVICE, FakeServer

import phonetopc
from phonetopc import Phone


async def until(predicate: Callable[[], bool], timeout: float = 2.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while not predicate():
        assert asyncio.get_running_loop().time() < deadline, "condition never became true"
        await asyncio.sleep(0.005)


async def take(
    events: phonetopc.Subscription, n: int, timeout: float = 2.0
) -> list[phonetopc.Event]:
    out: list[phonetopc.Event] = []

    async def _take() -> None:
        async for ev in events:
            out.append(ev)
            if len(out) == n:
                return

    await asyncio.wait_for(_take(), timeout)
    return out


# -- handshake -------------------------------------------------------------


async def test_hello(server: FakeServer, phone: Phone) -> None:
    assert phone.device.id == DEVICE["id"]
    assert phone.device.name == "Fake Phone"
    assert phone.device.model == "FP-1"
    assert phone.server.name == "fake"
    assert phone.capabilities == ("events", "get", "notify", "panels")
    (hello,) = server.requests("hello")
    assert hello["params"] == {
        "protocol": "phonetopc/1",
        "client": {"name": "pytest", "version": phonetopc.__version__},
    }
    assert server.received[0] is hello  # hello is the first request


async def test_unsupported_protocol(server: FakeServer) -> None:
    server.supported_protocols = ["phonetopc/2"]
    with pytest.raises(phonetopc.UnsupportedProtocol) as info:
        await phonetopc.connect(server.addr, dev=True)
    assert info.value.supported == ["phonetopc/2"]
    assert info.value.code == -32004
    assert info.value.name == "unsupported_protocol"


async def test_await_connect_and_close(server: FakeServer) -> None:
    phone = await phonetopc.connect(f"ws://{server.addr}")
    assert not phone.closed
    await phone.close()
    assert phone.closed
    with pytest.raises(phonetopc.ConnectionClosed):
        await phone.get("battery")


async def test_connect_uses_env(server: FakeServer, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("PHONETOPC_ADDR", server.addr)
    async with phonetopc.connect(dev=True) as phone:
        assert phone.url == f"ws://{server.addr}/v1/ws"


async def test_connect_refused() -> None:
    with pytest.raises(OSError):
        await phonetopc.connect("127.0.0.1:1", dev=True, open_timeout=2)


# -- requests --------------------------------------------------------------


async def test_list_sensors(phone: Phone) -> None:
    sensors = {s.source: s for s in await phone.list_sensors()}
    gyro = sensors["gyro"]
    assert gyro.types == ("rotation",)
    assert gyro.continuous and gyro.max_rate_hz == 1000
    assert not sensors["proximity"].continuous
    assert sensors["proximity"].min_rate_hz is None
    assert not sensors["game_rotation_vector"].available
    assert "thermometer" in sensors  # unknown sources are tolerated


async def test_get(server: FakeServer, phone: Phone) -> None:
    ev = await phone.get("battery")
    assert ev.value == {"level": 82, "charging": True}
    assert ev.unit == "%"
    assert ev.subscription is None and ev.seq is None
    assert ev.device == DEVICE["id"]
    assert server.requests("get")[-1]["params"] == {"source": "battery"}

    ev = await phone.get("gyro", type="rotation")
    assert ev.type == "rotation"
    assert server.requests("get")[-1]["params"] == {"source": "gyro", "type": "rotation"}


@pytest.mark.parametrize(
    ("source", "exc", "code"),
    [
        ("nope", phonetopc.UnknownSource, -32001),
        ("game_rotation_vector", phonetopc.SourceUnavailable, -32002),
        ("light", phonetopc.NoValue, -32006),
    ],
)
async def test_get_errors(phone: Phone, source: str, exc: type, code: int) -> None:
    with pytest.raises(exc) as info:
        await phone.get(source)
    assert isinstance(info.value, phonetopc.PhonetopcError)
    assert info.value.code == code
    # The connection survives error responses.
    assert (await phone.get("battery")).value["level"] == 82


async def test_method_not_found(server: FakeServer, phone: Phone) -> None:
    with pytest.raises(phonetopc.MethodNotFound):
        await phone.call("frobnicate", {})
    # Deliberately off-spec request; don't fail the schema check for it.
    assert len(server.schema_errors) == 1 and "frobnicate" in server.schema_errors[0]
    server.schema_errors.clear()


async def test_concurrent_requests_out_of_order(server: FakeServer, phone: Phone) -> None:
    # Earlier requests answer later: correlation must be by id, not order.
    server.values.update({"light": 300.0, "proximity": 5.0, "ui": True})
    server.get_delays = {"battery": 0.15, "light": 0.10, "proximity": 0.05, "ui": 0.0}
    results = await asyncio.gather(
        phone.get("battery"),
        phone.get("light"),
        phone.get("proximity"),
        phone.get("ui"),
        phone.list_sensors(),
    )
    assert [r.source for r in results[:4]] == ["battery", "light", "proximity", "ui"]
    assert results[1].value == 300.0
    assert len(results[4]) > 5


async def test_request_timeout_then_recovers(server: FakeServer) -> None:
    async with phonetopc.connect(server.addr, dev=True, request_timeout=0.05) as phone:
        server.get_delays["battery"] = 0.3
        with pytest.raises(phonetopc.RequestTimeout):
            await phone.get("battery")
        with pytest.raises(TimeoutError):  # also a builtin TimeoutError
            await phone.get("battery")
        server.get_delays.clear()
        await asyncio.sleep(0.4)  # late responses arrive and are ignored
        assert (await phone.get("battery")).value["charging"] is True
        assert phone._pending == {}


# -- subscriptions ---------------------------------------------------------


async def test_subscribe_stream(server: FakeServer, phone: Phone) -> None:
    async with phone.subscribe("gyro", rate_hz=200) as events:
        assert events.id is not None and events.id.startswith("s-")
        assert events.rate_hz == 200
        assert events.overflow == "drop_oldest"
        got = await take(events, 10)
    assert [e.seq for e in got] == list(range(10))
    assert all(e.source == "gyro" and e.unit == "rad/s" and len(e.value) == 3 for e in got)
    assert events.dropped == 0
    assert events.closed
    (sub_req,) = server.requests("subscribe")
    assert sub_req["params"] == {"source": "gyro", "rate_hz": 200}
    await server.wait_for_request("unsubscribe")
    assert server.requests("unsubscribe")[0]["params"] == {"subscription": events.id}
    # Iterating a closed subscription just ends.
    assert [e async for e in events] == []


async def test_subscribe_params(server: FakeServer, phone: Phone) -> None:
    async with phone.subscribe("gyro", type="rotation", rate_hz=5000, min_delta=0.01) as ev:
        assert ev.rate_hz == 1000  # clamped by the server
    assert server.requests("subscribe")[0]["params"] == {
        "source": "gyro",
        "type": "rotation",
        "rate_hz": 5000,
        "min_delta": 0.01,
    }


async def test_subscribe_errors(phone: Phone) -> None:
    with pytest.raises(phonetopc.UnknownSource):
        async with phone.subscribe("nope"):
            pass
    with pytest.raises(phonetopc.SourceUnavailable):
        async with phone.subscribe("game_rotation_vector"):
            pass
    with pytest.raises(phonetopc.InvalidParams):
        async with phone.subscribe("gyro", type="bogus"):
            pass


async def test_events_interleaved_with_responses(server: FakeServer, phone: Phone) -> None:
    async with phone.subscribe("gyro", rate_hz=1000) as events:
        for _ in range(20):
            assert (await phone.get("battery")).source == "battery"
        got = await take(events, 50)
    seqs = [e.seq for e in got]
    assert seqs == list(range(seqs[0], seqs[0] + 50))


async def test_two_subscriptions_same_source(server: FakeServer, phone: Phone) -> None:
    server.auto_stream = False
    async with phone.subscribe("battery") as a, phone.subscribe("battery") as b:
        assert a.id != b.id
        await server.emit("battery", {"level": 50, "charging": False}, count=3)
        ga, gb = await take(a, 4), await take(b, 4)
    # seq 0 is each subscription's initial value, then the emitted events.
    assert [e.seq for e in ga] == [0, 1, 2, 3] == [e.seq for e in gb]
    assert ga[0].value == {"level": 82, "charging": True}
    assert ga[1].value == {"level": 50, "charging": False}
    assert {e.subscription for e in ga} == {a.id}
    assert {e.subscription for e in gb} == {b.id}


async def test_on_change_initial_value(server: FakeServer, phone: Phone) -> None:
    server.values["light"] = 120.0
    async with phone.subscribe("light") as light, phone.subscribe("ui") as ui:
        (first,) = await take(light, 1)
        assert (first.seq, first.value) == (0, 120.0)
        await server.emit("light", 300.0)
        await server.emit("ui", True)
        (second,) = await take(light, 1)
        assert (second.seq, second.value) == (1, 300.0)
        # ui has no state, so its first event is the first tap, also seq 0.
        (tap,) = await take(ui, 1)
        assert (tap.seq, tap.value) == (0, True)
    assert light.dropped == 0 and ui.dropped == 0


async def test_location_and_nfc_values(server: FakeServer, phone: Phone) -> None:
    # location is a state like battery; nfc is event-like like ui (PROTOCOL.md 6).
    async with phone.subscribe("location") as location, phone.subscribe("nfc") as nfc:
        (fix,) = await take(location, 1)
        assert fix.seq == 0 and fix.type == "position" and fix.unit == ""
        assert {"lat", "lon", "accuracy_m"} <= fix.value.keys()
        with pytest.raises(phonetopc.NoValue):
            await phone.get("nfc")
        await server.emit("nfc")
        (scan,) = await take(nfc, 1)
        assert scan.seq == 0 and scan.type == "tag"
        assert scan.value["id"] and scan.value["ndef"][0]["text"] == "hi"
    heading = await phone.get("compass")
    assert heading.unit == "deg" and 0 <= heading.value < 360


async def test_min_delta_filtering_is_not_a_gap(server: FakeServer, phone: Phone) -> None:
    # Filtered readings don't consume a seq number (PROTOCOL.md section 5).
    server.values["light"] = 5.0
    async with phone.subscribe("light", min_delta=1.0) as events:
        for v in (5.5, 4.2, 7.0, 7.9, 9.0):
            await server.emit("light", v)
        got = await take(events, 3)
    assert [(e.seq, e.value) for e in got] == [(0, 5.0), (1, 7.0), (2, 9.0)]
    assert events.dropped == 0


async def test_seq_gaps_counted(server: FakeServer, phone: Phone) -> None:
    server.auto_stream = False
    server.skip_seqs = {0, 3, 4, 7}
    async with phone.subscribe("gyro") as events:
        await server.emit("gyro", count=10)
        got = await take(events, 6)
        assert [e.seq for e in got] == [1, 2, 5, 6, 8, 9]
        assert events.dropped_remote == 4
        assert events.dropped == 4
        assert events.received == 6


async def test_overflow_drops_oldest_for_continuous(server: FakeServer, phone: Phone) -> None:
    server.auto_stream = False
    async with phone.subscribe("gyro", queue_size=5) as events:
        await server.emit("gyro", count=20)
        await until(lambda: events.received == 20)
        assert events.qsize() == 5
        got = await take(events, 5)
    assert [e.seq for e in got] == [15, 16, 17, 18, 19]
    assert events.dropped_local == 15
    assert events.dropped_remote == 0
    assert events.dropped == 15


async def test_overflow_errors_for_on_change(server: FakeServer, phone: Phone) -> None:
    server.auto_stream = False
    async with phone.subscribe("battery", queue_size=2) as events:
        assert events.overflow == "error"
        await until(lambda: events.received == 1)  # the initial value
        await server.emit("battery", count=2)
        await until(lambda: events.received == 3)
        # Queued events are still delivered, then the overflow is raised.
        assert len(await take(events, 2)) == 2
        with pytest.raises(phonetopc.SubscriptionOverflow):
            await events.__anext__()
        assert events.dropped_local == 1
        await server.wait_for_request("unsubscribe")


async def test_overflow_policy_from_mode(server: FakeServer, phone: Phone) -> None:
    server.auto_stream = False
    async with phone.subscribe("thermometer") as ev:  # not in the spec's catalog
        assert ev.info is not None and ev.info.mode == "continuous"
        assert ev.mode == "continuous" and ev.overflow == "drop_oldest"
    async with phone.subscribe("ui") as ev:
        assert ev.mode == "on_change" and ev.overflow == "error"
    async with phone.subscribe("battery", overflow="drop_oldest") as ev:
        assert ev.overflow == "drop_oldest"
    async with phone.subscribe("gyro", overflow="error") as ev:
        assert ev.overflow == "error"


async def test_overflow_policy_without_mode(server: FakeServer, phone: Phone) -> None:
    # A draft 0 server sends no mode: infer it from rate_hz (continuous only).
    server.auto_stream = False
    server.omit_mode = True
    async with phone.subscribe("thermometer") as ev:
        assert ev.info is not None and ev.info.mode is None
        assert ev.mode == "continuous" and ev.overflow == "drop_oldest"
    async with phone.subscribe("ui") as ev:
        assert ev.mode == "on_change" and ev.overflow == "error"
    # The fake's result validation (by request method) caught the missing mode.
    assert len(server.schema_errors) == 2
    assert all("'mode' is a required property" in e for e in server.schema_errors)
    server.schema_errors.clear()


async def test_events_before_subscribe_response(server: FakeServer, phone: Phone) -> None:
    # Off-spec since draft 1 (the response comes first). The client doesn't
    # buffer such events; they are lost, but visibly so, as a seq gap.
    server.auto_stream = False
    server.events_before_response = 2
    async with phone.subscribe("light") as events:
        await server.emit("light", 7.0)
        got = await take(events, 1)
        assert [e.seq for e in got] == [2]
        assert events.dropped_remote == 2
        assert (await phone.get("battery")).source == "battery"  # connection is fine


async def test_unsubscribe_unknown_is_ignored(server: FakeServer, phone: Phone) -> None:
    server.forget_subscriptions = True
    async with phone.subscribe("light"):
        pass
    await server.wait_for_request("unsubscribe")


async def test_late_subscribe_response_is_cleaned_up(server: FakeServer) -> None:
    async with phonetopc.connect(server.addr, dev=True, request_timeout=0.05) as phone:
        server.delays["subscribe"] = 0.2
        with pytest.raises(phonetopc.RequestTimeout):
            async with phone.subscribe("light"):
                pass
        # The orphaned subscription is unsubscribed once its response arrives.
        await server.wait_for_request("unsubscribe")
        assert server.requests("unsubscribe")[0]["params"]["subscription"].startswith("s-")
        assert phone._subs == {}


async def test_subscription_requires_start(phone: Phone) -> None:
    sub = phone.subscribe("gyro")
    with pytest.raises(RuntimeError):
        await sub.__anext__()


# -- robustness ------------------------------------------------------------


async def test_ignores_garbage_from_server(server: FakeServer, phone: Phone) -> None:
    await server.send_raw("not json")
    await server.send_raw(b"\x00\x01")
    await server.send_raw(json.dumps([1, 2]))
    await server.send_raw(json.dumps({"jsonrpc": "2.0", "method": "future_thing", "params": {}}))
    await server.send_raw(json.dumps({"jsonrpc": "2.0", "method": "event", "params": {"x": 1}}))
    await server.send_raw(json.dumps({"jsonrpc": "2.0", "id": 9999, "result": {}}))
    await server.send_raw(
        json.dumps(
            {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "parse error"}}
        )
    )
    assert (await phone.get("battery")).source == "battery"


async def test_connection_loss_fails_pending_and_ends_iterators(
    server: FakeServer, phone: Phone
) -> None:
    server.auto_stream = False
    server.delays["get"] = 5
    async with phone.subscribe("light") as events:
        pending = asyncio.ensure_future(phone.get("battery"))
        await server.wait_for_request("get")
        await server.drop_connections(1011, "boom")
        with pytest.raises(phonetopc.ConnectionClosed) as info:
            await pending
        assert info.value.code == 1011
        assert [e async for e in events] == []  # iterator ends
        assert isinstance(events.closed_reason, phonetopc.ConnectionClosed)
    assert phone.closed
    await asyncio.wait_for(phone.wait_closed(), 1)
    with pytest.raises(phonetopc.ConnectionClosed):
        await phone.list_sensors()


async def test_server_closes_while_request_in_flight(server: FakeServer, phone: Phone) -> None:
    server.close_on.add("list_sensors")
    with pytest.raises(phonetopc.ConnectionClosed):
        await phone.list_sensors()


async def test_close_is_idempotent(phone: Phone) -> None:
    await phone.close()
    await phone.close()
    assert "closed" in repr(phone)


async def test_no_unretrieved_exceptions_on_racy_close(server: FakeServer) -> None:
    # Unsubscribes racing with the connection closing must not leave failed
    # futures behind ("Future exception was never retrieved").
    loop = asyncio.get_running_loop()
    contexts: list[dict[str, object]] = []
    loop.set_exception_handler(lambda _loop, ctx: contexts.append(ctx))
    try:
        for _ in range(10):
            phone = await phonetopc.connect(server.addr, dev=True)
            subs = [await phone.subscribe("light").start() for _ in range(3)]
            # Client starts closing; unsubscribes are sent while it's CLOSING.
            closer = asyncio.ensure_future(phone.close())
            await asyncio.sleep(0)
            tasks = [asyncio.ensure_future(s.close()) for s in subs]
            await asyncio.gather(closer, *tasks)
            del phone, subs, tasks, closer
        gc.collect()
        await asyncio.sleep(0)
    finally:
        loop.set_exception_handler(None)
    assert contexts == []
