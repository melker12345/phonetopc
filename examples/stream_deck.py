"""A stream deck on the phone's screen: mute the PC, toggle the mic, set the volume.

The phone shows a panel with a mute button, a mic switch and a volume slider.
They act on the PC's default output and input through ``wpctl`` (PipeWire)
or, without it, ``pactl`` (PulseAudio, or PipeWire's pulse server). Changes
made on the PC (keyboard volume keys, say) show up on the phone within a
couple of seconds.

Usage::

    python stream_deck.py
    python stream_deck.py --addr ws://127.0.0.1:8765     # the mock phone, dev mode
    python stream_deck.py --backend none                 # try it without touching audio

Allow *Screen panels* for this PC in the phone app first (off by default).
The phone asks with a notification before it shows the panel; the panel stays
on screen (``keep_awake``) while this script runs. The script reconnects when
the phone goes away.
"""

from __future__ import annotations

import argparse
import asyncio
import re
import shutil
import subprocess
import sys

import phonetopc
from phonetopc import ui

PANEL = "stream_deck"
RECONNECT_S = 5.0
POLL_S = 2.0


class Audio:
    """The PC's default output and input. Volume is in percent."""

    name = "none"

    def __init__(self) -> None:
        self._muted, self._mic_on, self._volume = False, True, 40

    def muted(self) -> bool:
        return self._muted

    def set_muted(self, muted: bool) -> None:
        self._muted = muted

    def mic_on(self) -> bool:
        return self._mic_on

    def set_mic_on(self, on: bool) -> None:
        self._mic_on = on

    def volume(self) -> int:
        return self._volume

    def set_volume(self, percent: int) -> None:
        self._volume = percent


def _run(*argv: str) -> str:
    return subprocess.run(argv, check=True, capture_output=True, text=True).stdout


class Wpctl(Audio):
    """PipeWire, through wpctl."""

    name = "wpctl"
    SINK, SOURCE = "@DEFAULT_AUDIO_SINK@", "@DEFAULT_AUDIO_SOURCE@"

    def _get(self, node: str) -> tuple[float, bool]:
        out = _run("wpctl", "get-volume", node)  # "Volume: 0.40 [MUTED]"
        match = re.search(r"Volume:\s*([\d.]+)", out)
        return (float(match.group(1)) if match else 0.0), "[MUTED]" in out

    def muted(self) -> bool:
        return self._get(self.SINK)[1]

    def set_muted(self, muted: bool) -> None:
        _run("wpctl", "set-mute", self.SINK, "1" if muted else "0")

    def mic_on(self) -> bool:
        return not self._get(self.SOURCE)[1]

    def set_mic_on(self, on: bool) -> None:
        _run("wpctl", "set-mute", self.SOURCE, "0" if on else "1")

    def volume(self) -> int:
        return round(self._get(self.SINK)[0] * 100)

    def set_volume(self, percent: int) -> None:
        _run("wpctl", "set-volume", self.SINK, f"{percent / 100:.2f}")


class Pactl(Audio):
    """PulseAudio (or pipewire-pulse), through pactl."""

    name = "pactl"
    SINK, SOURCE = "@DEFAULT_SINK@", "@DEFAULT_SOURCE@"

    def muted(self) -> bool:
        return "yes" in _run("pactl", "get-sink-mute", self.SINK)

    def set_muted(self, muted: bool) -> None:
        _run("pactl", "set-sink-mute", self.SINK, "1" if muted else "0")

    def mic_on(self) -> bool:
        return "yes" not in _run("pactl", "get-source-mute", self.SOURCE)

    def set_mic_on(self, on: bool) -> None:
        _run("pactl", "set-source-mute", self.SOURCE, "0" if on else "1")

    def volume(self) -> int:
        # "Volume: front-left: 26214 /  40% / -23.87 dB,   front-right: ..."
        match = re.search(r"(\d+)%", _run("pactl", "get-sink-volume", self.SINK))
        return int(match.group(1)) if match else 0

    def set_volume(self, percent: int) -> None:
        _run("pactl", "set-sink-volume", self.SINK, f"{percent}%")


def pick_backend(name: str) -> Audio:
    if name == "none":
        return Audio()
    if name in ("auto", "wpctl") and shutil.which("wpctl"):
        return Wpctl()
    if name in ("auto", "pactl") and shutil.which("pactl"):
        return Pactl()
    raise SystemExit(f"stream_deck: no {'wpctl or pactl' if name == 'auto' else name} found")


def mute_label(muted: bool) -> str:
    return "🔇 Unmute" if muted else "🔊 Mute"


def layout(audio: Audio) -> ui.Widget:
    muted = audio.muted()
    return ui.column(
        ui.text(f"Audio via {audio.name}", id="status", size="small", align="center"),
        ui.grid(
            2,
            ui.button("mute", mute_label(muted), tone="bad" if muted else "default"),
            ui.toggle("mic", "🎙 Mic", value=audio.mic_on()),
            weight=1,
        ),
        ui.slider("vol", label="Volume", min=0, max=100, step=1, value=audio.volume()),
    )


async def follow_pc(panel: phonetopc.Panel, audio: Audio) -> None:
    """Show changes made on the PC itself (keyboard, mixer) on the phone."""
    shown = (audio.muted(), audio.mic_on(), audio.volume())
    while True:
        await asyncio.sleep(POLL_S)
        now = await asyncio.to_thread(lambda: (audio.muted(), audio.mic_on(), audio.volume()))
        if now != shown:
            shown = now
            muted, mic, vol = now
            await panel.update_many(
                {
                    "mute": {"label": mute_label(muted), "tone": "bad" if muted else "default"},
                    "mic": {"value": mic},
                    "vol": {"value": vol},
                }
            )


async def run(phone: phonetopc.Phone, audio: Audio) -> None:
    """Show the deck and act on it until the user closes the panel.

    Raises :class:`phonetopc.ConnectionClosed` if the connection ends first.
    """
    panel = await phone.show_panel(PANEL, "Stream deck", layout(audio), keep_awake=True)
    follower = asyncio.ensure_future(follow_pc(panel, audio))
    try:
        async for ev in panel:
            if isinstance(ev, phonetopc.PressEvent) and ev.widget == "mute":
                muted = not audio.muted()
                audio.set_muted(muted)
                tone = "bad" if muted else "default"
                await panel.update("mute", label=mute_label(muted), tone=tone)
            elif isinstance(ev, phonetopc.ChangeEvent) and ev.widget == "mic":
                audio.set_mic_on(bool(ev.value))
            elif isinstance(ev, phonetopc.ChangeEvent) and ev.widget == "vol":
                audio.set_volume(round(ev.value))
    finally:
        follower.cancel()
    if panel.closed_reason is not None:  # the connection ended, not the panel
        raise panel.closed_reason
    print("panel closed on the phone")


async def main_async(args: argparse.Namespace) -> None:
    audio = pick_backend(args.backend)
    while True:
        try:
            async with phonetopc.connect(args.addr, client_name="stream_deck") as phone:
                print(f"connected to {phone.device.name}; audio via {audio.name}")
                await run(phone, audio)
            return  # the user closed the panel: done
        except (phonetopc.NotAllowed, phonetopc.PinMismatch, phonetopc.NotPaired):
            raise  # retrying won't help
        except (phonetopc.Error, OSError) as exc:
            print(f"disconnected ({exc}); retrying in {RECONNECT_S:g} s")
        await asyncio.sleep(RECONNECT_S)


def main() -> None:
    p = argparse.ArgumentParser(description="A stream deck for the PC's audio on the phone.")
    p.add_argument(
        "--addr",
        help="phone host[:port], or ws://host:port for a phone in dev mode "
        "(default: $PHONETOPC_ADDR, else the paired phone)",
    )
    p.add_argument(
        "--backend",
        choices=["auto", "wpctl", "pactl", "none"],
        default="auto",
        help="how to reach the audio system (none: pretend, for trying it out)",
    )
    args = p.parse_args()
    try:
        asyncio.run(main_async(args))
    except KeyboardInterrupt:
        pass
    except phonetopc.Error as exc:
        sys.exit(f"stream_deck: {exc}")


if __name__ == "__main__":
    main()
