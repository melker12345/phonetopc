from __future__ import annotations

import json
import os
import subprocess
import sys

import pytest
from fakeserver import DEVICE, FakeServer

from phonetopc.cli import format_event, main
from phonetopc.models import Event


def test_tail_human(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["tail", "gyro", "--rate", "200", "-n", "3", "--addr", threaded_server.addr, "--dev"])
    assert rc == 0
    out, err = capsys.readouterr()
    lines = out.splitlines()
    assert len(lines) == 3
    assert "#0" in lines[0] and "gyro/rotation" in lines[0] and "rad/s" in lines[0]
    assert "Fake Phone" in err and "200 Hz" in err
    assert threaded_server.requests("subscribe")[0]["params"] == {"source": "gyro", "rate_hz": 200}


def test_tail_json(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["tail", "gyro", "--json", "-n", "2", "--addr", threaded_server.addr, "--dev"])
    assert rc == 0
    events = [json.loads(line) for line in capsys.readouterr().out.splitlines()]
    assert [e["seq"] for e in events] == [0, 1]
    assert events[0]["device"] == DEVICE["id"]


def test_get_list_info(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    addr = ["--addr", threaded_server.addr, "--dev"]
    assert main(["get", "battery", "--json", *addr]) == 0
    assert json.loads(capsys.readouterr().out)["value"] == {"level": 82, "charging": True}
    assert main(["list", *addr]) == 0
    out = capsys.readouterr().out
    assert "gyro" in out and "[unavailable]" in out and "1-1000 Hz" in out
    assert main(["list", "--json", *addr]) == 0
    assert any(s["source"] == "thermometer" for s in json.loads(capsys.readouterr().out))
    assert main(["info", *addr]) == 0
    assert "Fake Phone" in capsys.readouterr().out


def test_errors_exit_1(threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["get", "nope", "--addr", threaded_server.addr, "--dev"]) == 1
    assert "unknown source" in capsys.readouterr().err
    assert main(["tail", "gyro", "--addr", "127.0.0.1:1", "--dev"]) == 1


def test_tail_reports_disconnect(
    threaded_server: FakeServer, capsys: pytest.CaptureFixture[str]
) -> None:
    threaded_server.auto_stream = False
    threaded_server.close_on.add("subscribe")
    assert main(["tail", "light", "--addr", threaded_server.addr, "--dev"]) == 1
    assert "closed" in capsys.readouterr().err


def test_format_event() -> None:
    ev = Event("ui", "tile_tap", True, "", 1760000000123, 1, "d", "s-1", 4)
    line = format_event(ev)
    assert "#4 " in line and line.endswith(" ui/tile_tap true")
    ev = Event("gyro", "rotation", [0.5, -1.0, 0.0], "rad/s", 1760000000123, 1, "d")
    assert format_event(ev).endswith(" gyro/rotation [+0.5000, -1.0000, +0.0000] rad/s")


def test_python_dash_m(threaded_server: FakeServer) -> None:
    env = {**os.environ, "PHONETOPC_ADDR": f"ws://{threaded_server.addr}"}
    proc = subprocess.run(
        [sys.executable, "-m", "phonetopc", "tail", "gyro", "-n", "2", "--json"],
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr
    assert len(proc.stdout.splitlines()) == 2
