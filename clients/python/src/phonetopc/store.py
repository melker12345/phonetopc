"""Reader for the PC credential store (spec/SECURITY.md section 6).

``phonetopc pair`` (the Go CLI) writes one entry per paired phone to
``$XDG_CONFIG_HOME/phonetopc/devices.json`` (default
``~/.config/phonetopc/devices.json``). This module reads it, picks a device,
and updates a device's ``addresses`` when the phone turns up somewhere new.
It never creates pairings.

Permissions: the file holds secrets and must be mode ``0600``. A file that
group or others can read still works (refusing would lock users out), but a
:class:`InsecurePermissionsWarning` is issued. Updates are written atomically
(temp file + rename) and always with mode ``0600``.
"""

from __future__ import annotations

import contextlib
import ipaddress
import json
import os
import stat
import tempfile
import warnings
from collections.abc import Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import AmbiguousDevice, CredentialStoreError, NotPaired
from .pin import check_pin_format

__all__ = [
    "STORE_VERSION",
    "InsecurePermissionsWarning",
    "PairedDevice",
    "config_dir",
    "devices_path",
    "find_by_address",
    "load_devices",
    "normalize_host",
    "remember_address",
    "select_device",
]

STORE_VERSION = 1
DEFAULT_PORT = 8765

_PAIR_HINT = "run `phonetopc pair` to pair a phone"


class InsecurePermissionsWarning(UserWarning):
    """``devices.json`` (or its directory) is readable by other users."""


@dataclass(frozen=True)
class PairedDevice:
    """One paired phone from ``devices.json``. ``token`` is kept out of ``repr``."""

    id: str
    name: str
    pin: str
    token: str = field(repr=False)
    port: int = DEFAULT_PORT
    addresses: tuple[str, ...] = ()
    paired_at: str | None = None
    raw: dict[str, Any] = field(default_factory=dict, repr=False, compare=False)

    @classmethod
    def from_json(cls, data: Any, where: str) -> PairedDevice:
        if not isinstance(data, dict):
            raise CredentialStoreError(f"{where}: a device must be a JSON object")

        def string(key: str) -> str:
            value = data.get(key)
            if not isinstance(value, str) or not value:
                raise CredentialStoreError(f"{where}: {key!r} must be a non-empty string")
            return value

        pin = string("pin")
        try:
            check_pin_format(pin)
        except ValueError as exc:
            raise CredentialStoreError(f"{where}: {exc}") from None
        port = data.get("port", DEFAULT_PORT)
        if not isinstance(port, int) or isinstance(port, bool) or not 0 < port < 65536:
            raise CredentialStoreError(f"{where}: 'port' must be an integer 1-65535")
        addresses = data.get("addresses", [])
        if not isinstance(addresses, list) or not all(isinstance(a, str) and a for a in addresses):
            raise CredentialStoreError(f"{where}: 'addresses' must be a list of strings")
        paired_at = data.get("paired_at")
        return cls(
            id=string("id"),
            name=string("name"),
            pin=pin,
            token=string("token"),
            port=port,
            addresses=tuple(addresses),
            paired_at=paired_at if isinstance(paired_at, str) else None,
            raw=data,
        )

    def label(self) -> str:
        return f"{self.name!r} ({self.id})"


def config_dir() -> Path:
    """``$XDG_CONFIG_HOME/phonetopc``, or ``~/.config/phonetopc``.

    Per the XDG spec, a relative ``XDG_CONFIG_HOME`` is ignored.
    """
    base = os.environ.get("XDG_CONFIG_HOME", "")
    root = Path(base) if base and os.path.isabs(base) else Path.home() / ".config"
    return root / "phonetopc"


def devices_path() -> Path:
    """Path of ``devices.json``."""
    return config_dir() / "devices.json"


def _check_permissions(path: Path, st: os.stat_result) -> None:
    if os.name != "posix":
        return
    if st.st_mode & 0o077:
        warnings.warn(
            f"{path} is readable by other users (mode {stat.S_IMODE(st.st_mode):04o}); it holds "
            f"secret tokens. Fix it with: chmod 600 {path}",
            InsecurePermissionsWarning,
            stacklevel=4,
        )


def _read_raw(path: Path) -> dict[str, Any] | None:
    """The parsed file, or ``None`` if it doesn't exist."""
    try:
        with open(path, "rb") as f:
            _check_permissions(path, os.fstat(f.fileno()))
            text = f.read()
    except FileNotFoundError:
        return None
    except OSError as exc:
        raise CredentialStoreError(f"can't read {path}: {exc}") from exc
    try:
        doc = json.loads(text)
    except ValueError as exc:
        raise CredentialStoreError(f"{path} is not valid JSON: {exc}") from None
    if not isinstance(doc, dict):
        raise CredentialStoreError(f"{path}: expected a JSON object")
    version = doc.get("version")
    if version != STORE_VERSION:
        raise CredentialStoreError(
            f"{path} has version {version!r}, this client reads version {STORE_VERSION}; "
            "upgrade the phonetopc Python package"
        )
    if not isinstance(doc.get("devices", []), list):
        raise CredentialStoreError(f"{path}: 'devices' must be a list")
    return doc


def load_devices(path: Path | None = None) -> list[PairedDevice]:
    """All paired devices. A missing file means no pairings (an empty list)."""
    path = path or devices_path()
    doc = _read_raw(path)
    if doc is None:
        return []
    return [
        PairedDevice.from_json(d, f"{path}: devices[{i}]")
        for i, d in enumerate(doc.get("devices", []))
    ]


def _names(devices: Sequence[PairedDevice]) -> str:
    return ", ".join(d.label() for d in devices)


def select_device(devices: Sequence[PairedDevice], query: str | None) -> PairedDevice:
    """Pick a device by id or name, or the only one when ``query`` is ``None``.

    An exact id match wins, then an exact name match, then a case-insensitive
    name match.
    """
    if not devices:
        raise NotPaired(f"no paired phone: {_PAIR_HINT}")
    if query is None:
        if len(devices) == 1:
            return devices[0]
        raise AmbiguousDevice(
            f"several paired phones ({_names(devices)}): pass device=<name or id> "
            "(--device on the command line)"
        )
    for matches in (
        [d for d in devices if d.id == query],
        [d for d in devices if d.name == query],
        [d for d in devices if d.name.casefold() == query.casefold()],
    ):
        if len(matches) == 1:
            return matches[0]
        if len(matches) > 1:
            raise AmbiguousDevice(
                f"several paired phones are called {query!r} ({_names(matches)}): "
                "pass the device id instead"
            )
    raise NotPaired(
        f"no paired phone with id or name {query!r} (paired: {_names(devices)}); {_PAIR_HINT}"
    )


def normalize_host(host: str) -> str:
    """Canonical form of a host for comparisons (lower case, IPs normalized)."""
    host = host.strip().strip("[]").lower()
    with contextlib.suppress(ValueError):
        return str(ipaddress.ip_address(host))
    return host


def find_by_address(devices: Sequence[PairedDevice], host: str, port: int) -> list[PairedDevice]:
    """Devices whose stored ``addresses`` contain ``host`` on ``port``."""
    h = normalize_host(host)
    return [
        d for d in devices if d.port == port and any(normalize_host(a) == h for a in d.addresses)
    ]


def _write_atomic(path: Path, doc: dict[str, Any]) -> None:
    data = (json.dumps(doc, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
    # mkstemp creates the file with mode 0600, in the same directory so the
    # rename is atomic.
    fd, tmp = tempfile.mkstemp(prefix=".devices.json.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        if os.name == "posix":
            os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(tmp)
        raise
    if os.name == "posix":
        with contextlib.suppress(OSError):
            dfd = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(dfd)
            finally:
                os.close(dfd)


def remember_address(
    device_id: str, host: str, *, port: int | None = None, path: Path | None = None
) -> bool:
    """Record that ``device_id`` was reached at ``host`` (SECURITY.md section 6).

    Moves (or adds) ``host`` to the front of the device's ``addresses``, and
    sets ``port`` if given. The file is re-read first and every other field is
    kept as is. Returns ``True`` if the file was rewritten.
    """
    path = path or devices_path()
    doc = _read_raw(path)
    if doc is None:
        return False
    entry = next(
        (d for d in doc.get("devices", []) if isinstance(d, dict) and d.get("id") == device_id),
        None,
    )
    if entry is None:
        return False
    old = entry.get("addresses")
    old_list = [a for a in old if isinstance(a, str)] if isinstance(old, list) else []
    h = normalize_host(host)
    new_list = [host] + [a for a in old_list if normalize_host(a) != h]
    changed = new_list != old
    if port is not None and entry.get("port", DEFAULT_PORT) != port:
        entry["port"] = port
        changed = True
    if not changed:
        return False
    entry["addresses"] = new_list
    _write_atomic(path, doc)
    return True
