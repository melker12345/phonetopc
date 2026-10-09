"""Command line: ``python -m phonetopc <command>``.

Commands::

    tail SOURCE [--type T] [--rate HZ] [--min-delta X] [--count N] [--json]
    get SOURCE [--type T] [--json]
    list [--json]
    info [--json]
    approval-key [--accept]

Every command takes ``--device NAME_OR_ID`` and ``--addr HOST[:PORT]`` to pick
a phone paired with ``phonetopc pair`` (default: the only paired phone, or
``$PHONETOPC_ADDR``), and ``--dev`` to talk plaintext to a phone in dev mode
(then ``--addr`` defaults to ``$PHONETOPC_ADDR``, else ``127.0.0.1:8765``).
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
from collections.abc import Sequence
from typing import Any, TextIO

from ._version import __version__
from .client import ENV_ADDR, _Connect, connect
from .errors import Error
from .models import Event, SensorInfo
from .store import load_devices


def _fmt_value(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, float):
        return f"{value:+.4f}"
    if isinstance(value, list):
        return "[" + ", ".join(_fmt_value(v) for v in value) + "]"
    if isinstance(value, dict):
        return json.dumps(value, separators=(",", ":"))
    return str(value)


def format_event(ev: Event) -> str:
    """One human-readable line per event: time, seq (padded), source/type, value, unit."""
    parts = [ev.datetime.astimezone().strftime("%H:%M:%S.%f")[:-3]]
    if ev.seq is not None:
        parts.append(f"#{ev.seq:<6}")
    parts.append(f"{ev.source}/{ev.type}")
    parts.append(_fmt_value(ev.value) + (f" {ev.unit}" if ev.unit else ""))
    return " ".join(parts)


def event_json(ev: Event) -> str:
    obj: dict[str, Any] = {
        "source": ev.source,
        "type": ev.type,
        "value": ev.value,
        "unit": ev.unit,
        "ts": ev.ts,
        "ts_mono_ns": ev.ts_mono_ns,
        "device": ev.device,
    }
    if ev.subscription is not None:
        obj = {"subscription": ev.subscription, "seq": ev.seq, **obj}
    return json.dumps(obj, separators=(",", ":"))


def _hz(value: float | None) -> str:
    return "?" if value is None else f"{value:g}"


def _sensor_line(s: SensorInfo) -> str:
    rate = ""
    if s.continuous:
        rate = f"{_hz(s.min_rate_hz)}-{_hz(s.max_rate_hz)} Hz, default {_hz(s.default_rate_hz)}"
    avail = "" if s.available else "  [unavailable]"
    types = ",".join(s.types)
    return f"{s.source:<22} {types:<12} {s.shape:<7} {s.unit or '-':<6} {s.mode:<10} {rate}{avail}"


def _connect(args: argparse.Namespace) -> _Connect:
    return connect(args.addr, device=args.device, dev=args.dev, client_name="phonetopc-cli")


async def cmd_tail(args: argparse.Namespace, out: TextIO) -> int:
    status = 0
    async with _connect(args) as phone:
        sub = phone.subscribe(
            args.source,
            type=args.type,
            rate_hz=args.rate,
            min_delta=args.min_delta,
        )
        n = 0
        async with sub as events:
            if not args.json:
                rate = f" at {events.rate_hz:g} Hz" if events.rate_hz else ""
                print(
                    f"# {phone.device.name}: {args.source} ({events.id}){rate}",
                    file=sys.stderr,
                )
            async for ev in events:
                out.write((event_json(ev) if args.json else format_event(ev)) + "\n")
                out.flush()
                n += 1
                if args.count and n >= args.count:
                    break
        if phone.closed:  # the iterator ended because the connection went away
            print(f"phonetopc: {phone.closed_reason}", file=sys.stderr)
            status = 1
        if events.dropped:
            print(
                f"# {events.dropped} events dropped "
                f"({events.dropped_remote} by the server, {events.dropped_local} locally)",
                file=sys.stderr,
            )
    return status


async def cmd_get(args: argparse.Namespace, out: TextIO) -> int:
    async with _connect(args) as phone:
        ev = await phone.get(args.source, args.type)
    out.write((event_json(ev) if args.json else format_event(ev)) + "\n")
    return 0


async def cmd_list(args: argparse.Namespace, out: TextIO) -> int:
    async with _connect(args) as phone:
        sensors = await phone.list_sensors()
    if args.json:
        out.write(json.dumps([dict(s.raw) for s in sensors], indent=2) + "\n")
    else:
        for s in sensors:
            out.write(_sensor_line(s) + "\n")
    return 0


async def cmd_info(args: argparse.Namespace, out: TextIO) -> int:
    async with _connect(args) as phone:
        hello = phone.hello
    if args.json:
        out.write(json.dumps(dict(hello.raw), indent=2) + "\n")
    else:
        d = hello.device
        out.write(f"device:       {d.name} ({d.manufacturer or '?'} {d.model or '?'})\n")
        out.write(f"device id:    {d.id}\n")
        out.write(f"platform:     {d.platform} {d.platform_version or ''}\n")
        out.write(f"server:       {hello.server.name} {hello.server.version}\n")
        out.write(f"protocol:     {hello.protocol}\n")
        out.write(f"capabilities: {', '.join(hello.capabilities)}\n")
        out.write(f"url:          {phone.url}\n")
        if phone.secure:
            pc = f", phone knows this PC as {hello.pc.name!r}" if hello.pc else ""
            out.write(f"security:     pinned TLS + token{pc}\n")
        else:
            out.write("security:     none (dev mode)\n")
    return 0


async def cmd_approval_key(args: argparse.Namespace, out: TextIO) -> int:
    async with _connect(args) as phone:
        if phone.paired_device is None:
            print("phonetopc: approval keys exist only in secure mode", file=sys.stderr)
            return 1
        device_id = phone.paired_device.id
        current = phone.approval_key
        stored = next((d.approval_key for d in load_devices() if d.id == device_id), None)
        if current is None:
            out.write(f"{phone.device.name} has no approval key (no fingerprint enrolled?)\n")
            return 1
        if args.accept:
            changed = await phone.accept_approval_key()
            verb = "accepted" if changed else "already accepted"
            out.write(f"{verb}: approval key {current} of {phone.device.name}\n")
            return 0
    out.write(f"phone:    {current}\n")
    out.write(f"accepted: {stored or '(none yet: trusted on first approval)'}\n")
    if stored is not None and stored != current:
        out.write(
            "The phone's approval key changed (fingerprints added or removed?). If you did that, "
            "trust the new key with --accept.\n"
        )
    return 0


def build_parser() -> argparse.ArgumentParser:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument(
        "--addr",
        help=f"phone host[:port], wss:// or (dev mode) ws:// URL (default: ${ENV_ADDR}, "
        "else the paired phone's known addresses; with --dev 127.0.0.1:8765)",
    )
    common.add_argument(
        "--device",
        metavar="NAME_OR_ID",
        help="which paired phone (needed when several are paired)",
    )
    common.add_argument(
        "--dev",
        action="store_true",
        help="dev mode: plaintext ws://, no pairing (the phone's developer setting)",
    )
    common.add_argument("--json", action="store_true", help="machine-readable JSON output")

    parser = argparse.ArgumentParser(
        prog="python -m phonetopc", description="phonetopc command line client"
    )
    parser.add_argument("--version", action="version", version=f"phonetopc {__version__}")
    sub = parser.add_subparsers(dest="command", metavar="COMMAND")
    sub.required = True

    p = sub.add_parser("tail", parents=[common], help="print events from a sensor")
    p.add_argument("source", help="sensor source, e.g. gyro")
    p.add_argument("--type", help="only this event type")
    p.add_argument("--rate", type=float, metavar="HZ", help="sample rate (continuous sensors)")
    p.add_argument("--min-delta", type=float, metavar="X", help="server-side change filter")
    p.add_argument("-n", "--count", type=int, default=0, help="stop after N events")
    p.set_defaults(func=cmd_tail)

    p = sub.add_parser("get", parents=[common], help="read the latest value once")
    p.add_argument("source")
    p.add_argument("--type")
    p.set_defaults(func=cmd_get)

    p = sub.add_parser("list", parents=[common], help="list the phone's sensors")
    p.set_defaults(func=cmd_list)

    p = sub.add_parser("info", parents=[common], help="show device and server info")
    p.set_defaults(func=cmd_info)

    p = sub.add_parser(
        "approval-key", parents=[common], help="show or accept the phone's approval key"
    )
    p.add_argument(
        "--accept",
        action="store_true",
        help="trust the phone's current approval key (after fingerprints changed on it)",
    )
    p.set_defaults(func=cmd_approval_key)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        status: int = asyncio.run(args.func(args, sys.stdout))
    except KeyboardInterrupt:
        return 130
    except BrokenPipeError:  # e.g. `| head`
        return 0
    except (Error, OSError, ValueError) as exc:
        print(f"phonetopc: {exc}", file=sys.stderr)
        return 1
    return status


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
