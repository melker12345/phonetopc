"""Find a paired phone on the LAN via mDNS (spec/SECURITY.md section 6).

Optional: needs the ``zeroconf`` package (``pip install 'phonetopc[discovery]'``).
Without it, :func:`locate` returns ``None`` and the client falls back to the
addresses stored at pairing time.
"""

from __future__ import annotations

import asyncio
import contextlib
import importlib
import ipaddress
import logging
from dataclasses import dataclass
from typing import Any

__all__ = ["SERVICE_TYPE", "Located", "locate"]

logger = logging.getLogger("phonetopc")

SERVICE_TYPE = "_phonetopc._tcp.local."
DEFAULT_TIMEOUT = 2.0


@dataclass(frozen=True)
class Located:
    """Where mDNS says a device is."""

    addresses: tuple[str, ...]
    port: int
    #: The phone advertises ``dev=1``: it serves plaintext, not TLS.
    dev_mode: bool = False


def _load_zeroconf() -> tuple[Any, Any] | None:
    try:
        zc = importlib.import_module("zeroconf")
        azc = importlib.import_module("zeroconf.asyncio")
    except ImportError:
        return None
    return zc, azc


def _usable(addr: str) -> bool:
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return False
    # Link-local IPv6 needs a zone id, which doesn't survive into a URL.
    return not (ip.version == 6 and ip.is_link_local)


def _sort_key(addr: str) -> int:
    return 0 if ipaddress.ip_address(addr).version == 4 else 1


async def locate(device_id: str, timeout: float = DEFAULT_TIMEOUT) -> Located | None:
    """Browse ``_phonetopc._tcp`` for the service whose TXT ``id`` is ``device_id``.

    Returns ``None`` if ``zeroconf`` isn't installed, nothing matched within
    ``timeout`` seconds, or mDNS failed (it's best effort by design).
    """
    mods = _load_zeroconf()
    if mods is None:
        logger.debug("zeroconf not installed; skipping mDNS")
        return None
    try:
        return await asyncio.wait_for(_browse(mods, device_id), timeout)
    except asyncio.TimeoutError:
        logger.debug("mDNS: %s not found within %s s", device_id, timeout)
    except Exception as exc:  # mDNS is best effort: sockets, permissions, ...
        logger.debug("mDNS lookup failed: %r", exc)
    return None


async def _browse(mods: tuple[Any, Any], device_id: str) -> Located:
    zc, azc = mods
    names: asyncio.Queue[str] = asyncio.Queue()
    want = device_id.encode("utf-8")

    def on_change(zeroconf: Any, service_type: str, name: str, state_change: Any) -> None:
        if state_change in (zc.ServiceStateChange.Added, zc.ServiceStateChange.Updated):
            names.put_nowait(name)

    aiozc = azc.AsyncZeroconf()
    browser = None
    try:
        browser = azc.AsyncServiceBrowser(aiozc.zeroconf, SERVICE_TYPE, handlers=[on_change])
        while True:
            name = await names.get()
            info = azc.AsyncServiceInfo(SERVICE_TYPE, name)
            if not await info.async_request(aiozc.zeroconf, 1000):
                continue
            props = info.properties or {}
            if props.get(b"id") != want:
                continue
            addresses = sorted((a for a in info.parsed_addresses() if _usable(a)), key=_sort_key)
            if not addresses or not info.port:
                continue
            return Located(tuple(addresses), int(info.port), props.get(b"dev") == b"1")
    finally:
        with contextlib.suppress(Exception):
            if browser is not None:
                await browser.async_cancel()
        with contextlib.suppress(Exception):
            await aiozc.async_close()
