"""The credential store reader (SECURITY.md section 6)."""

from __future__ import annotations

import json
import os
import stat
import threading
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
    accept_approval_key,
    load_devices,
    pin_approval_key,
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
    assert dev.approval_key is None


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
    # No temp files left; .lock stays (deleting a lock file would race other writers).
    assert sorted(p.name for p in path.parent.iterdir()) == [store.LOCK_NAME, "devices.json"]

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
    assert sorted(p.name for p in path.parent.iterdir()) == [store.LOCK_NAME, "devices.json"]


def test_remember_address_without_addresses_field(write_store: StoreWriter) -> None:
    entry = device_entry()
    del entry["addresses"], entry["port"]
    path = write_store(entry)
    assert remember_address("a1b2c3d4", "192.168.1.9")
    stored = json.loads(path.read_text())["devices"][0]
    assert stored["addresses"] == ["192.168.1.9"]
    assert "port" not in stored  # a default stays a default


# -- locking (SECURITY.md section 6, draft 3) ----------------------------------------


@pytest.mark.skipif(store._fcntl is None, reason="needs fcntl.flock")
def test_writes_wait_for_the_lock_and_reread(write_store: StoreWriter) -> None:
    import fcntl

    path = write_store(device_entry(addresses=["192.168.1.2"]))
    lock_path = path.parent / store.LOCK_NAME
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT, 0o600)
    done = threading.Event()

    def writer() -> None:
        remember_address("a1b2c3d4", "192.168.1.3")
        done.set()

    worker = threading.Thread(target=writer)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)  # another writer (e.g. `phonetopc pair`) holds it
        worker.start()
        assert not done.wait(0.3), "the write didn't wait for the lock"
        # The other writer changes the file before releasing the lock ...
        doc = json.loads(path.read_text())
        doc["devices"][0]["approval_key"] = PINS["pc"]
        doc["devices"].append(device_entry(id="ffff", name="Tablet"))
        path.write_text(json.dumps(doc))
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)
    worker.join(5)
    assert done.is_set()
    # ... and the waiting write re-read it instead of overwriting its changes.
    phone, tablet = load_devices()
    assert phone.addresses == ("192.168.1.3", "192.168.1.2")
    assert phone.approval_key == PINS["pc"] and tablet.name == "Tablet"
    assert mode(lock_path) == 0o600


def test_concurrent_writers_lose_nothing(write_store: StoreWriter) -> None:
    ids = [f"dev{i}" for i in range(8)]
    write_store(*(device_entry(id=i, name=i, addresses=[]) for i in ids))

    def work(device_id: str) -> None:
        for n in range(10):
            remember_address(device_id, f"10.0.0.{n}")

    threads = [threading.Thread(target=work, args=(i,)) for i in ids]
    for t in threads:
        t.start()
    for t in threads:
        t.join(10)
    for dev in load_devices():
        assert dev.addresses[0] == "10.0.0.9" and len(dev.addresses) == 10


# -- approval keys (APPROVAL.md section 3) --------------------------------------------


def test_pin_approval_key_first_sight_then_kept(write_store: StoreWriter) -> None:
    path = write_store(device_entry(extra="kept"))
    assert pin_approval_key("a1b2c3d4", PINS["phone"]) == PINS["phone"]
    (dev,) = load_devices()
    assert dev.approval_key == PINS["phone"] and dev.raw["extra"] == "kept"
    inode = path.stat().st_ino
    # A different key later is *not* stored; the accepted one is returned.
    assert pin_approval_key("a1b2c3d4", PINS["pc"]) == PINS["phone"]
    assert path.stat().st_ino == inode
    # Updating addresses keeps it.
    assert remember_address("a1b2c3d4", "192.168.1.200")
    assert load_devices()[0].approval_key == PINS["phone"]


def test_accept_approval_key(write_store: StoreWriter) -> None:
    write_store(device_entry(approval_key=PINS["phone"]))
    assert accept_approval_key("a1b2c3d4", PINS["pc"])
    assert not accept_approval_key("a1b2c3d4", PINS["pc"])
    assert load_devices()[0].approval_key == PINS["pc"]
    with pytest.raises(NotPaired):
        accept_approval_key("nope", PINS["pc"])
    with pytest.raises(NotPaired):
        pin_approval_key("nope", PINS["pc"])
    with pytest.raises(ValueError):
        accept_approval_key("a1b2c3d4", "sha256/short")


def test_malformed_approval_key(write_store: StoreWriter) -> None:
    write_store(device_entry(approval_key="md5/xyz"))
    with pytest.raises(CredentialStoreError, match="approval_key"):
        load_devices()
    with pytest.raises(CredentialStoreError, match="approval_key"):
        pin_approval_key("a1b2c3d4", PINS["phone"])
