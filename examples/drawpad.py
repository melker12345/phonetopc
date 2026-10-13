"""Draw on the phone, watch it appear live in a window on the PC.

The phone shows a full-screen canvas with a *Clear* button. Strokes stream to
the PC while you draw (one chunk per screen frame) and are mirrored into a
tkinter window with the same aspect ratio; stylus pressure sets the line
width. *Clear* on the phone clears both.

The drawing survives network trouble: the phone keeps taking ink while the
connection is down, and when the script reconnects it asks for every stroke
after the last one it drew (``attach_panel(after_pseq=...)``). Restart the
script and it picks up the drawing that is still on the phone.

Usage::

    python drawpad.py
    python drawpad.py --addr ws://127.0.0.1:8765     # the mock phone, dev mode

Needs tkinter (on Arch: ``pacman -S tk``) and *Screen panels* allowed for this
PC in the phone app.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import queue
import sys
import threading
from dataclasses import dataclass, field
from typing import Any, Union

import phonetopc
from phonetopc import ui

PANEL = "drawpad"
CANVAS = "ink"
RECONNECT_S = 3.0
INK = "#202020"
BACKGROUND = "#ffffff"
WINDOW_HEIGHT = 720


class Clear:
    """Sentinel: clear the window."""


Item = Union[phonetopc.StrokeEvent, Clear]


@dataclass
class Mirror:
    """Turns stroke events into line segments; knows nothing about tkinter.

    ``draw(segment)`` gets ``(tag, x0, y0, x1, y1, width, color)`` in window
    pixels; ``delete(tag)`` removes a cancelled stroke; ``resize(w, h)`` is
    called when the canvas's aspect ratio is first known.
    """

    width: float
    height: float
    draw: Any
    delete: Any
    resize: Any
    last: dict[int, tuple[float, float]] = field(default_factory=dict)
    aspect: float | None = None

    def handle(self, item: Item) -> None:
        if isinstance(item, Clear):
            self.last.clear()
            self.delete("all")
            return
        ev = item
        if ev.size is not None and self.aspect is None:
            self.aspect = ev.size[0] / ev.size[1]
            self.width = self.height * self.aspect
            self.resize(self.width, self.height)
        tag = f"s{ev.stroke}"
        if ev.phase == "cancel":
            self.last.pop(ev.stroke, None)
            self.delete(tag)
            return
        color = BACKGROUND if ev.tool == "eraser" else INK
        for p in ev.points:
            xy = (p.x * self.width, p.y * self.height)
            prev = self.last.get(ev.stroke, xy)
            self.draw((tag, *prev, *xy, 1 + 5 * p.pressure, color))
            self.last[ev.stroke] = xy
        if ev.phase == "end":
            self.last.pop(ev.stroke, None)


async def follow(phone: phonetopc.Phone, out: queue.Queue[Item], last_pseq: int) -> int | None:
    """Mirror the panel until the connection ends, and return the last pseq handled.

    Returns ``None`` when the user closed the drawing pad on the phone.
    """
    try:
        panel = await phone.attach_panel(PANEL, after_pseq=last_pseq)
        print("picked up the drawing pad that is on the phone")
    except phonetopc.UnknownPanel:
        layout = ui.column(
            ui.canvas(CANVAS, color=INK, background=BACKGROUND, width=3),
            ui.row(ui.button("clear", "Clear", weight=1)),
        )
        panel = await phone.show_panel(PANEL, "Drawing pad", layout, keep_awake=True)
        out.put(Clear())  # a new, empty canvas
        last_pseq = -1
    async for ev in panel:
        if isinstance(ev, phonetopc.StrokeEvent):
            out.put(ev)
        elif isinstance(ev, phonetopc.PressEvent) and ev.widget == "clear":
            await panel.clear_canvas(CANVAS)
            out.put(Clear())
        last_pseq = panel.last_pseq
    if panel.closed_reason is None:
        return None
    return last_pseq


async def connection_loop(addr: str | None, out: queue.Queue[Item]) -> None:
    last_pseq = -1  # -1: replay whatever is still on the phone's canvas
    while True:
        try:
            async with phonetopc.connect(addr, client_name="drawpad") as phone:
                print(f"connected to {phone.device.name}; draw on the phone")
                handled = await follow(phone, out, last_pseq)
            if handled is None:
                print("the drawing pad was closed on the phone")
                return
            last_pseq = handled
        except (phonetopc.NotAllowed, phonetopc.PinMismatch, phonetopc.NotPaired):
            raise  # retrying won't help
        except (phonetopc.Error, OSError) as exc:
            print(f"disconnected ({exc}); retrying in {RECONNECT_S:g} s")
        await asyncio.sleep(RECONNECT_S)


def main() -> None:
    p = argparse.ArgumentParser(description="Mirror drawing on the phone into a PC window.")
    p.add_argument(
        "--addr",
        help="phone host[:port], or ws://host:port for a phone in dev mode "
        "(default: $PHONETOPC_ADDR, else the paired phone)",
    )
    args = p.parse_args()
    try:
        import tkinter as tk
    except ImportError:
        sys.exit("drawpad: needs tkinter (on Arch: pacman -S tk)")

    items: queue.Queue[Item] = queue.Queue()
    failure: list[BaseException] = []

    def network() -> None:
        try:
            asyncio.run(connection_loop(args.addr, items))
        except BaseException as exc:  # reported by the UI thread
            failure.append(exc)

    root = tk.Tk()
    root.title("phonetopc drawing pad")
    canvas = tk.Canvas(root, width=WINDOW_HEIGHT * 9 // 16, height=WINDOW_HEIGHT, bg=BACKGROUND)
    canvas.pack(fill="both", expand=True)

    def draw(seg: tuple[str, float, float, float, float, float, str]) -> None:
        tag, x0, y0, x1, y1, width, color = seg
        canvas.create_line(
            x0, y0, x1, y1, fill=color, width=width, capstyle="round", tags=(tag, "ink")
        )

    def delete(tag: str) -> None:
        canvas.delete("ink" if tag == "all" else tag)

    mirror = Mirror(
        WINDOW_HEIGHT * 9 / 16,
        WINDOW_HEIGHT,
        draw,
        delete,
        lambda w, h: canvas.config(width=round(w), height=round(h)),
    )

    def poll() -> None:
        while True:
            try:
                mirror.handle(items.get_nowait())
            except queue.Empty:
                break
        if failure:
            root.destroy()
            return
        root.after(16, poll)

    threading.Thread(target=network, daemon=True).start()
    root.after(16, poll)
    with contextlib.suppress(KeyboardInterrupt):
        root.mainloop()
    if failure and not isinstance(failure[0], KeyboardInterrupt):
        exc = failure[0]
        sys.exit(f"drawpad: {exc}" if not isinstance(exc, SystemExit) else exc.code)


if __name__ == "__main__":
    main()
