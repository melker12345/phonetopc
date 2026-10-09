from __future__ import annotations

from pathlib import Path

import phonetopc


def test_version() -> None:
    assert phonetopc.__version__.count(".") == 2


def test_typed_marker_shipped() -> None:
    assert (Path(phonetopc.__file__).parent / "py.typed").is_file()
