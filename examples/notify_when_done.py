"""Get a phone notification when a long command finishes (or just send one).

Usage::

    python notify_when_done.py -- make -j8 world
    python notify_when_done.py --panel -- ./train.py --epochs 50
    python notify_when_done.py --title "Claude Code is done"

With a command, it runs the command, then posts "✅ make finished" or
"❌ make failed (exit 2)" with how long it took. ``--panel`` also shows a status
panel while the command runs: the command line, elapsed time and a busy bar,
turning green or red at the end. Without a command it just posts ``--title``
(and ``--body``), which suits hooks: for example a Claude Code ``Stop`` hook
running ``python examples/notify_when_done.py --title "Claude Code is done"``.

Exit status: the command's own status; 0 after just a notification; 2 when
the notification couldn't be sent (the command's status wins if it failed).

Allow *Notifications* (and, for ``--panel``, *Screen panels*) for this PC in
the phone app first; both are off by default.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import shlex
import sys
import time

import phonetopc
from phonetopc import ui

PANEL = "job"


def elapsed(seconds: float) -> str:
    s = round(seconds)
    if s < 60:
        return f"{s} s"
    if s < 3600:
        return f"{s // 60} min {s % 60} s"
    return f"{s // 3600} h {s % 3600 // 60} min"


def shorten(text: str, limit: int) -> str:
    return text if len(text) <= limit else text[: limit - 1] + "…"


async def run_command(command: list[str], panel: phonetopc.Panel | None) -> int:
    proc = await asyncio.create_subprocess_exec(*command)
    start = time.monotonic()
    while True:
        try:
            return await asyncio.wait_for(proc.wait(), 1.0)
        except asyncio.TimeoutError:
            if panel is not None:
                await panel.update("time", text=f"running for {elapsed(time.monotonic() - start)}")


async def main_async(args: argparse.Namespace, command: list[str]) -> int:
    status = 0
    title, body = args.title, args.body
    if command:
        name = os.path.basename(command[0])
        line = shorten(shlex.join(command), 300)
        start = time.monotonic()
        if args.panel:
            async with phonetopc.connect(args.addr, client_name="notify_when_done") as phone:
                panel = await phone.show_panel(
                    PANEL,
                    shorten(f"Running {name}", 60),
                    ui.column(
                        ui.text(line, size="title"),
                        ui.text(f"in {os.getcwd()}", size="small"),
                        ui.text("starting", id="time"),
                        ui.progress(id="bar"),
                    ),
                )
                status = await run_command(command, panel)
                took = elapsed(time.monotonic() - start)
                tone = "good" if status == 0 else "bad"
                result = "finished" if status == 0 else f"failed (exit {status})"
                await panel.update_many(
                    {
                        "time": {"text": f"{result} after {took}", "tone": tone},
                        "bar": {"value": 1, "tone": tone},
                    }
                )
        else:
            try:
                status = await run_command(command, None)
            except FileNotFoundError:
                print(f"notify_when_done.py: no such command: {command[0]}", file=sys.stderr)
                return 127
        took = elapsed(time.monotonic() - start)
        if title is None:
            title = f"✅ {name} finished" if status == 0 else f"❌ {name} failed (exit {status})"
        if body is None:
            body = f"{line}\nafter {took} in {os.getcwd()}"
    try:
        async with phonetopc.connect(args.addr, client_name="notify_when_done") as phone:
            await phone.notify(shorten(title or "Done", 100), shorten(body or "", 1000), id=args.id)
    except (phonetopc.Error, OSError, ValueError) as exc:
        print(f"notify_when_done.py: no notification sent: {exc}", file=sys.stderr)
        return status or 2
    return status


def main(argv: list[str]) -> int:
    command: list[str] = []
    if "--" in argv:
        i = argv.index("--")
        argv, command = argv[:i], argv[i + 1 :]
    p = argparse.ArgumentParser(
        prog="notify_when_done.py",
        description="Post a phone notification when a command finishes.",
        epilog="example: notify_when_done.py -- make -j8",
    )
    p.add_argument("--addr", help="phone host[:port], or ws://host:port (dev mode)")
    p.add_argument("--title", help="notification title (default: the command's result)")
    p.add_argument("--body", help="notification text (default: command, time taken, directory)")
    p.add_argument("--id", help="notification id: a new one replaces the old one with this id")
    p.add_argument("--panel", action="store_true", help="show a status panel while it runs")
    args = p.parse_args(argv)
    if not command and args.title is None:
        p.error("give a command after --, or --title")
    try:
        return asyncio.run(main_async(args, command))
    except KeyboardInterrupt:
        return 130
    except (phonetopc.Error, OSError, ValueError) as exc:
        print(f"notify_when_done.py: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
