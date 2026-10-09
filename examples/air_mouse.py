"""Air mouse: turn the phone's gyroscope into relative mouse movement.

Hold the phone like a remote, screen up, top edge pointing at the monitor.
Turning it left/right (yaw) moves the pointer horizontally, tilting the top
up/down (pitch) moves it vertically. Tapping the phonetopc Quick Settings tile
sends a left click.

How it works: the pointer is driven through a virtual input device created
with Linux ``uinput``. uinput sits below the display server, so this works the
same on Wayland (Hyprland, Sway, GNOME, KDE) and X11, and even on a text
console. Angular speed (rad/s) is integrated over the phone's own monotonic
timestamps (``ts_mono_ns``), so Wi-Fi jitter doesn't change how far the
pointer moves, only when.

Requirements::

    pip install -e clients/python evdev

``/dev/uinput`` must be writable by you. Either run as root (not
recommended), or add a udev rule and join the ``input`` group::

    # /etc/udev/rules.d/99-uinput.rules
    KERNEL=="uinput", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"

    sudo usermod -aG input "$USER"      # then log out and back in
    sudo modprobe uinput                # if /dev/uinput doesn't exist
    sudo udevadm control --reload && sudo udevadm trigger

Note: members of ``input`` can also *read* every keyboard and mouse on the
system. On a shared machine, prefer a dedicated group in the udev rule.

Usage::

    python air_mouse.py --addr 192.168.1.20 --scale 900 --deadzone 0.04

Tune ``--scale`` (pixels per radian of rotation) for speed and ``--deadzone``
(rad/s) until the pointer rests when you hold the phone still. Use USB
(``adb forward tcp:8765 tcp:8765`` and ``--addr 127.0.0.1``) for the lowest
latency.
"""

from __future__ import annotations

import argparse
import asyncio
import sys

import phonetopc

try:
    from evdev import UInput
    from evdev import ecodes as e
except ImportError:  # pragma: no cover - depends on the environment
    sys.exit("air_mouse needs python-evdev: pip install evdev")

MAX_DT = 0.05  # s; caps the step after a gap so the pointer never jumps


class AirMouse:
    def __init__(self, ui: UInput, scale: float, deadzone: float, invert_x: bool, invert_y: bool):
        self.ui = ui
        self.scale = scale
        self.deadzone = deadzone
        self.sign_x = 1.0 if invert_x else -1.0
        self.sign_y = 1.0 if invert_y else -1.0
        self.last_ns: int | None = None
        self.rest_x = 0.0  # sub-pixel remainders, so slow motion isn't lost
        self.rest_y = 0.0

    def feed(self, ev: phonetopc.Event) -> None:
        gx, _gy, gz = ev.value  # rad/s around the device x (pitch) and z (yaw) axes
        if self.last_ns is None:
            self.last_ns = ev.ts_mono_ns
            return
        dt = min(max((ev.ts_mono_ns - self.last_ns) / 1e9, 0.0), MAX_DT)
        self.last_ns = ev.ts_mono_ns
        if abs(gz) < self.deadzone:
            gz = 0.0
        if abs(gx) < self.deadzone:
            gx = 0.0
        self.rest_x += self.sign_x * gz * dt * self.scale
        self.rest_y += self.sign_y * gx * dt * self.scale
        dx, dy = int(self.rest_x), int(self.rest_y)
        if dx == 0 and dy == 0:
            return
        self.rest_x -= dx
        self.rest_y -= dy
        if dx:
            self.ui.write(e.EV_REL, e.REL_X, dx)
        if dy:
            self.ui.write(e.EV_REL, e.REL_Y, dy)
        self.ui.syn()

    def click(self) -> None:
        self.ui.write(e.EV_KEY, e.BTN_LEFT, 1)
        self.ui.syn()
        self.ui.write(e.EV_KEY, e.BTN_LEFT, 0)
        self.ui.syn()


async def run(args: argparse.Namespace, mouse: AirMouse) -> None:
    async with phonetopc.connect(args.addr, client_name="air_mouse") as phone:
        print(f"air mouse: connected to {phone.device.name}, Ctrl-C to stop")

        async def clicks() -> None:
            try:
                async with phone.subscribe("ui", type="tile_tap") as taps:
                    async for _ in taps:
                        mouse.click()
            except phonetopc.PhonetopcError as exc:
                print(f"(no tile clicks: {exc.message})")

        click_task = asyncio.ensure_future(clicks())
        try:
            # Small queue: for a pointer, fresh data beats complete data.
            async with phone.subscribe("gyro", rate_hz=args.rate, queue_size=8) as gyro:
                print(f"gyro at {gyro.rate_hz or args.rate:g} Hz")
                async for ev in gyro:
                    mouse.feed(ev)
            if gyro.dropped:
                print(f"{gyro.dropped} gyro events dropped")
        finally:
            click_task.cancel()


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument(
        "--addr",
        help="phone host[:port], or ws://host:port for a phone in dev mode "
        "(default: $PHONETOPC_ADDR, else the paired phone)",
    )
    p.add_argument("--scale", type=float, default=800.0, help="pixels per radian (800)")
    p.add_argument("--deadzone", type=float, default=0.03, help="ignore rad/s below this (0.03)")
    p.add_argument("--rate", type=float, default=100.0, help="gyro rate in Hz (100)")
    p.add_argument("--invert-x", action="store_true")
    p.add_argument("--invert-y", action="store_true")
    args = p.parse_args()

    caps = {e.EV_REL: [e.REL_X, e.REL_Y], e.EV_KEY: [e.BTN_LEFT, e.BTN_RIGHT]}
    try:
        ui = UInput(caps, name="phonetopc-air-mouse")
    except (PermissionError, FileNotFoundError) as exc:
        sys.exit(
            f"cannot open /dev/uinput ({exc}).\n"
            "Add the udev rule and join the 'input' group as described at the top of this file."
        )
    with ui:
        mouse = AirMouse(ui, args.scale, args.deadzone, args.invert_x, args.invert_y)
        try:
            asyncio.run(run(args, mouse))
        except KeyboardInterrupt:
            pass
        except (phonetopc.Error, OSError) as exc:
            sys.exit(f"air_mouse: {exc}")


if __name__ == "__main__":
    main()
