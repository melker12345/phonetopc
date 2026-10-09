from __future__ import annotations

import datetime as dt

import pytest

import phonetopc
from phonetopc.errors import error_from_response

EVENT = {
    "subscription": "s-7f3a",
    "seq": 1042,
    "source": "gyro",
    "type": "rotation",
    "value": [0.01, -0.32, 0.05],
    "unit": "rad/s",
    "ts": 1760000000123,
    "ts_mono_ns": 81234567890123,
    "device": "a1b2c3d4",
    "future_field": 1,
}


@pytest.mark.parametrize(
    ("addr", "url"),
    [
        ("192.168.1.20", "ws://192.168.1.20:8765/v1/ws"),
        ("192.168.1.20:9000", "ws://192.168.1.20:9000/v1/ws"),
        ("phone.local:9000/", "ws://phone.local:9000/v1/ws"),
        ("localhost:1/custom", "ws://localhost:1/custom"),
        ("[::1]:8765", "ws://[::1]:8765/v1/ws"),
        ("::1", "ws://[::1]:8765/v1/ws"),
        ("ws://192.168.1.20:8765/v1/ws", "ws://192.168.1.20:8765/v1/ws"),
        ("ws://192.168.1.20:8765", "ws://192.168.1.20:8765/v1/ws"),
        ("wss://phone.example/x", "wss://phone.example/x"),
        ("  10.0.0.2  ", "ws://10.0.0.2:8765/v1/ws"),
    ],
)
def test_resolve_url(addr: str, url: str) -> None:
    assert phonetopc.resolve_url(addr) == url


@pytest.mark.parametrize("addr", ["", "http://x", "host:notaport", "ws://", "host:99999"])
def test_resolve_url_invalid(addr: str) -> None:
    with pytest.raises(ValueError):
        phonetopc.resolve_url(addr)


def test_resolve_url_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("PHONETOPC_ADDR", raising=False)
    assert phonetopc.resolve_url() == "ws://127.0.0.1:8765/v1/ws"
    monkeypatch.setenv("PHONETOPC_ADDR", "10.1.2.3")
    assert phonetopc.resolve_url() == "ws://10.1.2.3:8765/v1/ws"


def test_event_parse() -> None:
    ev = phonetopc.Event.from_json(EVENT)
    assert ev.subscription == "s-7f3a" and ev.seq == 1042
    assert ev.value == [0.01, -0.32, 0.05]
    assert ev.datetime == dt.datetime(2025, 10, 9, 8, 53, 20, 123000, tzinfo=dt.timezone.utc)


def test_get_result_parse() -> None:
    body = {k: v for k, v in EVENT.items() if k not in ("subscription", "seq")}
    ev = phonetopc.Event.from_json(body, notification=False)
    assert ev.subscription is None and ev.seq is None


@pytest.mark.parametrize(
    "broken",
    [
        {k: v for k, v in EVENT.items() if k != "value"},
        {k: v for k, v in EVENT.items() if k != "seq"},
        {**EVENT, "seq": "1"},
        {**EVENT, "seq": True},
        {**EVENT, "ts": 1.5},
        {**EVENT, "unit": None},
        [1, 2, 3],
    ],
)
def test_event_parse_rejects(broken: object) -> None:
    with pytest.raises(phonetopc.ProtocolError):
        phonetopc.Event.from_json(broken)


def test_hello_parse() -> None:
    hello = phonetopc.HelloResult.from_json(
        {
            "protocol": "phonetopc/1",
            "server": {"name": "phonetopc-android", "version": "0.1.0"},
            "device": {"id": "a", "name": "Galaxy", "platform": "android"},
            "capabilities": ["events", "get", "commands"],
        }
    )
    assert hello.device.manufacturer is None
    assert hello.capabilities == ("events", "get", "commands")
    with pytest.raises(phonetopc.ProtocolError):
        phonetopc.HelloResult.from_json({"protocol": "phonetopc/1"})


def test_sensor_info_parse() -> None:
    info = phonetopc.SensorInfo.from_json(
        {
            "source": "x",
            "types": ["t"],
            "unit": "",
            "shape": "hologram",
            "mode": "sometimes",
            "available": True,
        }
    )
    assert info.shape == "hologram" and not info.continuous  # new enum values tolerated
    with pytest.raises(phonetopc.ProtocolError):
        phonetopc.SensorInfo.from_json({**info.raw, "available": "yes"})


def test_subscribe_result_parse() -> None:
    base = {"subscription": "s-1", "source": "gyro", "types": ["rotation"]}
    res = phonetopc.SubscribeResult.from_json({**base, "mode": "continuous", "rate_hz": 100})
    assert res.mode == "continuous" and res.rate_hz == 100.0
    res = phonetopc.SubscribeResult.from_json({**base, "source": "light", "mode": "on_change"})
    assert res.mode == "on_change" and res.rate_hz is None
    # Draft 0 servers don't send mode.
    assert phonetopc.SubscribeResult.from_json(base).mode is None
    with pytest.raises(phonetopc.ProtocolError):
        phonetopc.SubscribeResult.from_json({**base, "mode": 1})
    with pytest.raises(phonetopc.ProtocolError):
        phonetopc.SubscribeResult.from_json({"subscription": "s-1", "source": "gyro"})


@pytest.mark.parametrize(
    ("code", "cls"),
    [
        (-32700, phonetopc.ParseError),
        (-32600, phonetopc.InvalidRequest),
        (-32601, phonetopc.MethodNotFound),
        (-32602, phonetopc.InvalidParams),
        (-32603, phonetopc.InternalError),
        (-32001, phonetopc.UnknownSource),
        (-32002, phonetopc.SourceUnavailable),
        (-32003, phonetopc.HelloRequired),
        (-32004, phonetopc.UnsupportedProtocol),
        (-32005, phonetopc.UnknownSubscription),
        (-32006, phonetopc.NoValue),
        (-32099, phonetopc.PhonetopcError),
    ],
)
def test_error_mapping(code: int, cls: type) -> None:
    err = error_from_response({"code": code, "message": "m", "data": {"x": 1}})
    assert type(err) is cls
    assert isinstance(err, phonetopc.Error)
    assert (err.code, err.message, err.data) == (code, "m", {"x": 1})


def test_error_mapping_by_name_and_malformed() -> None:
    err = error_from_response({"code": -32050, "message": "m", "data": {"name": "no_value"}})
    assert isinstance(err, phonetopc.NoValue) and err.code == -32050
    err = error_from_response("bad")
    assert type(err) is phonetopc.PhonetopcError
    err = error_from_response({"code": "x"})
    assert err.code == -32603 and err.message == "unknown error"
    assert type(err) is phonetopc.InternalError
    assert "InternalError(code=-32603" in repr(err)
