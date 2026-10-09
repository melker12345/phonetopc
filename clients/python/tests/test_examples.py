"""examples/approve.py against the fake phone."""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import ModuleType

import pytest
from conftest import StoreWriter
from fakeserver import REPO_ROOT, FakeServer, device_entry

pytest.importorskip("cryptography")


def load_example(name: str) -> ModuleType:
    path = REPO_ROOT / "examples" / f"{name}.py"
    spec = importlib.util.spec_from_file_location(f"example_{name}", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_approve_runs_the_command_only_when_approved(
    threaded_secure_server: FakeServer,
    write_store: StoreWriter,
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    approve = load_example("approve")
    threaded_secure_server.enable_approval()
    write_store(device_entry(threaded_secure_server))
    marker = tmp_path / "ran"
    command = [sys.executable, "-c", f"open({str(marker)!r}, 'w'); raise SystemExit(7)"]

    assert approve.main(["Run deploy?", "--", *command]) == 7
    assert marker.exists()
    marker.unlink()
    (req,) = threaded_secure_server.requests("request_approval")
    assert req["params"]["reason"] == "Run deploy?"

    threaded_secure_server.approval_mode = "denied"
    assert approve.main(["--timeout", "10", "Run deploy?", "--", *command]) == 1
    assert not marker.exists()
    assert "not approved (denied)" in capsys.readouterr().err

    threaded_secure_server.approval_mode = "approve"
    assert approve.main(["Just checking?"]) == 0
    assert approve.approved("Plain function?")

    threaded_secure_server.rotate_approval_key()
    assert approve.main(["Run deploy?", "--", *command]) == 2
    assert "approval-key --accept" in capsys.readouterr().err
    assert not marker.exists()
