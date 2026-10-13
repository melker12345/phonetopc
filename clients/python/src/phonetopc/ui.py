"""Builders for panel layouts (spec/PANELS.md section 5).

Each function returns the JSON object of one widget, ready for
:meth:`Phone.show_panel <phonetopc.Phone.show_panel>`::

    from phonetopc import ui

    layout = ui.grid(
        3,
        ui.button("mute", "🔇 Mute", tone="bad"),
        ui.toggle("mic", "Mic", value=True),
        ui.slider("vol", label="Volume", min=0, max=100, step=1, value=40),
    )

Members left as ``None`` are left out, so the phone's defaults apply. The
result is a plain ``dict``: change it, or write layouts by hand, as you like.
Nothing is checked here; ``show_panel`` validates the whole layout before
sending it.
"""

from __future__ import annotations

from typing import Any, Literal

__all__ = [
    "Align",
    "Size",
    "Tone",
    "Widget",
    "button",
    "canvas",
    "column",
    "grid",
    "progress",
    "row",
    "slider",
    "text",
    "text_input",
    "toggle",
]

#: A widget's JSON object.
Widget = dict[str, Any]
Tone = Literal["default", "accent", "good", "warn", "bad"]
Size = Literal["small", "body", "title", "display"]
Align = Literal["start", "center", "end"]


def _widget(type: str, **members: Any) -> Widget:
    return {"type": type, **{k: v for k, v in members.items() if v is not None}}


def column(*children: Widget, id: str | None = None, weight: float | None = None) -> Widget:
    """Children top to bottom. Weighted children share the space left over."""
    return _widget("column", id=id, weight=weight, children=list(children))


def row(*children: Widget, id: str | None = None, weight: float | None = None) -> Widget:
    """Children from start to end. Weighted children share the space left over."""
    return _widget("row", id=id, weight=weight, children=list(children))


def grid(
    cols: int, *children: Widget, id: str | None = None, weight: float | None = None
) -> Widget:
    """Equal cells, ``cols`` (1-8) per row, filled row by row; widgets fill their cell."""
    return _widget("grid", id=id, weight=weight, cols=cols, children=list(children))


def text(
    text: str,
    *,
    id: str | None = None,
    size: Size | None = None,
    align: Align | None = None,
    tone: Tone | None = None,
    weight: float | None = None,
) -> Widget:
    """Text (up to 4000 characters, newlines allowed). Give it an ``id`` to update it."""
    return _widget("text", id=id, text=text, size=size, align=align, tone=tone, weight=weight)


def button(
    id: str,
    label: str,
    *,
    tone: Tone | None = None,
    enabled: bool | None = None,
    weight: float | None = None,
) -> Widget:
    """A button; pressing it sends a ``press`` event."""
    return _widget("button", id=id, label=label, tone=tone, enabled=enabled, weight=weight)


def toggle(
    id: str,
    label: str,
    *,
    value: bool | None = None,
    enabled: bool | None = None,
    weight: float | None = None,
) -> Widget:
    """An on/off switch; flipping it sends a ``change`` event with the new boolean."""
    return _widget("toggle", id=id, label=label, value=value, enabled=enabled, weight=weight)


def slider(
    id: str,
    *,
    label: str | None = None,
    min: float | None = None,
    max: float | None = None,
    step: float | None = None,
    value: float | None = None,
    enabled: bool | None = None,
    weight: float | None = None,
) -> Widget:
    """A slider from ``min`` (default 0) to ``max`` (default 1); sends ``change`` events."""
    return _widget(
        "slider",
        id=id,
        label=label,
        min=min,
        max=max,
        step=step,
        value=value,
        enabled=enabled,
        weight=weight,
    )


def progress(
    value: float | None = None,
    *,
    id: str | None = None,
    label: str | None = None,
    tone: Tone | None = None,
    weight: float | None = None,
) -> Widget:
    """A progress bar: ``value`` in [0, 1], or ``None`` for an indeterminate one."""
    w = _widget("progress", id=id, label=label, tone=tone, weight=weight)
    w["value"] = value  # null is meaningful here: indeterminate
    return w


def text_input(
    id: str,
    *,
    label: str | None = None,
    placeholder: str | None = None,
    value: str | None = None,
    multiline: bool | None = None,
    max_length: int | None = None,
    clear_on_submit: bool | None = None,
    enabled: bool | None = None,
    weight: float | None = None,
) -> Widget:
    """A text field; the phone sends a ``submit`` event with the text when the user sends it."""
    return _widget(
        "text_input",
        id=id,
        label=label,
        placeholder=placeholder,
        value=value,
        multiline=multiline,
        max_length=max_length,
        clear_on_submit=clear_on_submit,
        enabled=enabled,
        weight=weight,
    )


def canvas(
    id: str,
    *,
    ink: bool | None = None,
    color: str | None = None,
    width: float | None = None,
    background: str | None = None,
    enabled: bool | None = None,
    weight: float | None = None,
) -> Widget:
    """A drawing surface that streams ``stroke`` events; ``ink=False`` makes it a touchpad.

    ``color`` and ``background`` are ``#rrggbb``; ``width`` is the stroke width in dp.
    """
    return _widget(
        "canvas",
        id=id,
        ink=ink,
        color=color,
        width=width,
        background=background,
        enabled=enabled,
        weight=weight,
    )
