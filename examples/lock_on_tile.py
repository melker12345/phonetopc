"""Lock the PC when you tap the phonetopc Quick Settings tile on the phone.

Locking tries, in order:

1. ``loginctl lock-session`` (systemd-logind or elogind). logind emits a
   ``Lock`` signal; your idle/lock daemon has to act on it. hypridle
   (``lock_cmd``), swayidle (``lock`` event), xss-lock, GNOME and KDE all do.
   When run outside a login session (for example from a systemd user service)
   there's no session to name, so ``loginctl lock-sessions`` is tried next.
2. ``xdg-screensaver lock`` (X11 desktops without logind).

``--command 'hyprlock'`` replaces the chain with your own command.

Usage::

    python lock_on_tile.py
    python lock_on_tile.py --addr 192.168.1.20
    python lock_on_tile.py --command 'swaylock -f'

The script reconnects when the phone goes away (Wi-Fi roaming, Doze, app
restart), so it can run in the background for the whole session.
"""

from __future__ import annotations

import argparse
import asyncio
import shlex
import shutil
import sys
import time

import phonetopc

DEFAULT_CHAIN = [
    ["loginctl", "lock-session"],
    ["loginctl", "lock-sessions"],
    ["xdg-screensaver", "lock"],
]
DEBOUNCE_S = 2.0
RECONNECT_S = 5.0


async def run_command(argv: list[str]) -> bool:
    if shutil.which(argv[0]) is None:
        return False
    proc = await asyncio.create_subprocess_exec(
        *argv, stdout=asyncio.subprocess.DEVNULL, stderr=asyncio.subprocess.PIPE
    )
    _, err = await proc.communicate()
    if proc.returncode != 0:
        print(f"  {shlex.join(argv)} failed: {err.decode(errors='replace').strip()}")
        return False
    print(f"  locked with {shlex.join(argv)}")
    return True


async def lock(chain: list[list[str]]) -> None:
    for argv in chain:
        if await run_command(argv):
            return
    print("  could not lock: no locking method worked (try --command)")


async def watch(addr: str | None, chain: list[list[str]]) -> None:
    async with phonetopc.connect(addr, client_name="lock_on_tile") as phone:
        print(f"connected to {phone.device.name}; tap the phonetopc tile to lock")
        last = 0.0
        async with phone.subscribe("ui", type="tile_tap") as taps:
            async for _ in taps:
                now = time.monotonic()
                if now - last < DEBOUNCE_S:
                    continue
                last = now
                print("tile tapped, locking")
                await lock(chain)


async def main_async(args: argparse.Namespace) -> None:
    chain = [shlex.split(args.command)] if args.command else DEFAULT_CHAIN
    while True:
        try:
            await watch(args.addr, chain)
        except (
            phonetopc.PhonetopcError,  # e.g. tile source disabled, or this PC was revoked
            phonetopc.PinMismatch,
            phonetopc.NotPaired,
            phonetopc.AmbiguousDevice,
            phonetopc.CredentialStoreError,
        ):
            raise  # retrying won't help
        except (phonetopc.Error, OSError) as exc:
            print(f"disconnected ({exc}); retrying in {RECONNECT_S:g} s")
        await asyncio.sleep(RECONNECT_S)


def main() -> None:
    p = argparse.ArgumentParser(description="Lock the PC from the phone's Quick Settings tile.")
    p.add_argument(
        "--addr",
        help="phone host[:port], or ws://host:port for a phone in dev mode "
        "(default: $PHONETOPC_ADDR, else the paired phone)",
    )
    p.add_argument("--command", help="lock command to run instead of loginctl/xdg-screensaver")
    args = p.parse_args()
    try:
        asyncio.run(main_async(args))
    except KeyboardInterrupt:
        pass
    except phonetopc.Error as exc:
        sys.exit(f"lock_on_tile: {exc}")


if __name__ == "__main__":
    main()
