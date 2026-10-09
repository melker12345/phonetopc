"""The credential store reader (SECURITY.md section 6)."""

from __future__ import annotations

import json
import os
import stat
import warnings
from pathlib import Path

import pytest
from conftest import StoreWriter
from fakeserver import PINS, TOKEN, device_entry

from phonetopc import store
from phonetopc.errors import AmbiguousDevice, CredentialStoreError, NotPaired
from phonetopc.store import (
    InsecurePermissionsWarning,
    PairedDevice,
    load_devices,
    remember_address,
    select_device,
)


def mode(path: Path) -> int:
    return stat.S_IMODE(path.stat().st_mode)


def test_paths(config_home: Path, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    assert store.devices_path() == config_home / "phonetopc" / "devices.json"
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("XDG_CONFIG_HOME")
    assert store.config_dir() == tmp_path / ".config" / "phonetopc"
    monkeypatch.setenv("XDG_CONFIG_HOME", "relative/dir")  # ignored, per the XDG spec
    assert store.config_dir() == tmp_path / ".config" / "phonetopc"


def test_missing_file_means_no_pairings() -> None:
    assert load_devices() == []


def test_load(write_store: StoreWriter) -> None:
    write_store(
        device_entry(addresses=["192.168.1.153", "fe80::1"], extra="kept"),
        device_entry(id="ffff", name="Tablet", port=9000, addresses=[]),
    )
    phone, tablet = load_devices()
    assert phone.id == "a1b2c3d4" and phone.name == "Fake Phone"
    assert phone.pin == PINS["phone"] and phone.token == TOKEN
    assert phone.addresses == ("192.168.1.153", "fe80::1")
    assert phone.port == 8765 and tablet.port == 9000
    assert phone.paired_at == "2026-10-09T16:40:00Z"
    assert phone.raw["extra"] == "kept"
    assert TOKEN not in repr(phone)  # the token never ends up in logs


def test_optional_fields(write_store: StoreWriter) -> None:
    entry = device_entry()
    for key in ("port", "addresses", "paired_at"):
        del entry[key]
    write_store(entry)
    (dev,) = load_devices()
    assert dev.port == 8765 and dev.addresses == () and dev.paired_at is None


@pytest.mark.parametrize(
    ("content", "match"),
    [
        ("not json", "not valid JSON"),
        ("[]", "expected a JSON object"),
        ('{"version": 2, "devices": []}', "version 2"),
        ('{"devices": []}', "version None"),
        ('{"version": 1, "devices": {}}', "'devices' must be a list"),
        ('{"version": 1, "devices": [1]}', "must be a JSON object"),
    ],
)
def test_malformed_files(write_store: StoreWriter, content: str, match: str) -> None:
    write_store().write_text(content)
    with pytest.raises(CredentialStoreError, match=match):
        load_devices()


@pytest.mark.parametrize(
    ("fields", "match"),
    [
        ({"id": ""}, "'id'"),
        ({"token": None}, "'token'"),
        ({"pin": "sha1/abc"}, "pin must start"),
        ({"port": "8765"}, "'port'"),
        ({"port": 0}, "'port'"),
        ({"addresses": "192.168.1.2"}, "'addresses'"),
        ({"addresses": [1]}, "'addresses'"),
    ],
)
def test_malformed_devices(write_store: StoreWriter, fields: dict[str, object], match: str) -> None:
    write_store(device_entry(**fields))
    with pytest.raises(CredentialStoreError, match=match):
        load_devices()


@pytest.mark.skipif(os.name != "posix", reason="POSIX permissions")
def test_loose_permissions_warn_but_work(write_store: StoreWriter) -> None:
    path = write_store(device_entry(), mode=0o644)
    with pytest.warns(InsecurePermissionsWarning, match="chmod 600"):
        (dev,) = load_devices()
    assert dev.token == TOKEN
    assert mode(path) == 0o644  # reading never changes the file


@pytest.mark.skipif(os.name != "posix", reason="POSIX permissions")
def test_strict_permissions_dont_warn(write_store: StoreWriter) -> None:
    write_store(device_entry())
    with warnings.catch_warnings():
        warnings.simplefilter("error")
        load_devices()


@pytest.mark.skipif(os.name != "posix" or os.geteuid() == 0, reason="needs a non-root POSIX user")
def test_unreadable_file(write_store: StoreWriter) -> None:
    write_store(device_entry(), mode=0o000)
    with pytest.raises(CredentialStoreError, match="can't read"):
        load_devices()


# -- selecting a device ---------------------------------------------------------


def dev(id: str, name: str) -> PairedDevice:
    return PairedDevice(id=id, name=name, pin=PINS["phone"], token="t")


def test_select_device() -> None:
    a, b, c = dev("aaaa", "Pixel"), dev("bbbb", "Galaxy"), dev("cccc", "galaxy")
    with pytest.raises(NotPaired, match="run `phonetopc pair`"):
        select_device([], None)
    assert select_device([a], None) is a
    with pytest.raises(AmbiguousDevice, match="pass device="):
        select_device([a, b], None)
    assert select_device([a, b], "bbbb") is b
    assert select_device([a, b], "Pixel") is a
    assert select_device([a, b], "pixel") is a  # case-insensitive as a last resort
    assert select_device([a, b, c], "galaxy") is c  # an exact name wins
    with pytest.raises(AmbiguousDevice, match="pass the device id"):
        select_device([a, b, c], "GALAXY")
    with pytest.raises(NotPaired, match="'nope'"):
        select_device([a, b], "nope")
    # An id beats a name.
    d = dev("Pixel", "Other")
    assert select_device([a, d], "Pixel") is d


def test_find_by_address() -> None:
    a = PairedDevice("a", "A", PINS["phone"], "t", 8765, ("192.168.1.2", "FE80::0001"))
    assert store.find_by_address([a], "192.168.1.2", 8765) == [a]
    assert store.find_by_address([a], "[fe80::1]", 8765) == [a]
    assert store.find_by_address([a], "192.168.1.2", 9000) == []
    assert store.find_by_address([a], "192.168.1.3", 8765) == []


# -- updating addresses -----------------------------------------------------------


def test_remember_address_is_atomic_and_keeps_everything(write_store: StoreWriter) -> None:
    other = device_entry(id="ffff", name="Tablet", addresses=["10.0.0.9"])
    path = write_store(
        device_entry(addresses=["192.168.1.2", "192.168.1.3"], extra={"x": 1}),
        other,
        future_top_level="kept",
    )
    before = json.loads(path.read_text())
    inode = path.stat().st_ino

    assert remember_address("a1b2c3d4", "192.168.1.3")
    after = json.loads(path.read_text())
    assert after["devices"][0]["addresses"] == ["192.168.1.3", "192.168.1.2"]
    assert after["devices"][0]["extra"] == {"x": 1}
    assert after["devices"][1] == other
    assert after["future_top_level"] == "kept"
    assert {k: v for k, v in after.items() if k != "devices"} == {
        k: v for k, v in before.items() if k != "devices"
    }
    assert path.stat().st_ino != inode  # replaced by rename, not rewritten in place
    assert mode(path) == 0o600
    assert sorted(p.name for p in path.parent.iterdir()) == ["devices.json"]  # no temp files

    assert remember_address("a1b2c3d4", "10.1.1.1", port=9000)
    (dev_a, _) = load_devices()
    assert dev_a.addresses == ("10.1.1.1", "192.168.1.3", "192.168.1.2")
    assert dev_a.port == 9000


def test_remember_address_no_change_no_write(write_store: StoreWriter) -> None:
    path = write_store(device_entry(addresses=["192.168.1.2"]))
    inode = path.stat().st_ino
    assert not remember_address("a1b2c3d4", "192.168.1.2")
    assert not remember_address("unknown", "192.168.1.9")
    assert path.stat().st_ino == inode
    path.unlink()
    assert not remember_address("a1b2c3d4", "192.168.1.9")  # no file: nothing to update
    assert not path.exists()


@pytest.mark.skipif(os.name != "posix", reason="POSIX permissions")
def test_remember_address_never_loosens_permissions(write_store: StoreWriter) -> None:
    path = write_store(device_entry(), mode=0o644)
    with pytest.warns(InsecurePermissionsWarning):
        remember_address("a1b2c3d4", "192.168.1.77")
    assert mode(path) == 0o600


def test_failed_write_leaves_the_file_alone(
    write_store: StoreWriter, monkeypatch: pytest.MonkeyPatch
) -> None:
    path = write_store(device_entry())
    original = path.read_bytes()

    def boom(src: str, dst: str) -> None:
        raise OSError("disk full")

    monkeypatch.setattr(store.os, "replace", boom)
    with pytest.raises(OSError, match="disk full"):
        remember_address("a1b2c3d4", "192.168.1.77")
    assert path.read_bytes() == original
    assert sorted(p.name for p in path.parent.iterdir()) == ["devices.json"]
