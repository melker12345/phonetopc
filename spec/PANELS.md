# phonetopc panels — the phone as a screen for the PC

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md). The key words MUST, SHOULD and MAY are used
as in RFC 2119.

Everything else in the protocol flows from the phone to the PC. This document goes the other way: a PC program
shows things on the phone and gets the user's input back.

- **Notifications** (§3): a phone notification with up to three action buttons.
- **Panels** (§4–§7): a full-screen page that the PC describes as a tree of widgets (buttons, toggles, sliders,
  text, a text field, a drawing canvas). The phone draws it with native widgets; taps, slider moves, submitted
  text and canvas strokes come back as events. A stream deck, a status screen and a drawing pad are all panels.

The phone never runs code from the PC: a panel is data, and the PC only learns what the user does on it.

## 1. Availability and permission

- The phone lists capability `notify` when it implements §3, and `panels` when it implements §4–§7. A capability
  says the method exists, not that this PC may use it.
- **Both are off by default for every PC.** The user turns them on per paired PC in the phone app
  (*Notifications* and *Screen panels*). In dev mode all clients count as one PC, the *dev-mode PC*, with its own
  two settings, also off by default.
- Without permission, every method of that feature fails with `-32030` (`not_allowed`), with `data.permission`
  `"notify"` or `"panels"`. Its event source (`notification` or `panel`, §2) is listed as `available: false`, and
  subscribing to it gets `-32002`, as for a sensor the user turned off.
- If the user turns panels off for a PC, the phone closes all of that PC's panels at once (a `state` event with
  `"reason": "revoked"` goes to each attached owner, §6). Turning notifications off removes that PC's notifications.

**Who is "the PC".** Panels and notifications belong to the PC, not to a connection. In secure mode the PC is
`hello.pc.id`; several connections of the same PC (two scripts, or a script that reconnected) share its panels
and notifications. In dev mode it's the dev-mode PC.

**What the user sees, always.** Anything shown for a PC carries the PC's name as the phone knows it from
pairing, never a name chosen by the PC program. In dev mode the phone shows *Unverified PC (dev mode)* instead.
A panel can't hide this frame (§5), so a PC can't pass off a panel as part of the phone or of another app.

## 2. Events

Input comes back as ordinary `event` notifications (`PROTOCOL.md` §5) from two new sources. Both are event-like
(`on_change`, no initial value, like `ui`) with `shape` `object` and unit `""`.

| `source` | `type`s | Sent to |
|---|---|---|
| `notification` | `action`, `tap`, `dismiss` (§3.2) | every session **of the PC that posted the notification** with a matching subscription |
| `panel` | `state`, `press`, `change`, `submit`, `stroke` (§6) | the panel's **owner** session (§4.2) only, if it has a matching subscription |

Events of these sources are never dropped for backpressure (`PROTOCOL.md` §5 rule for `on_change`). Events that
happen while no matching subscription exists are not queued, except canvas strokes, which the phone keeps and
replays on request (§7). So a client SHOULD subscribe to `panel` **before** calling `panel_show`.

Other PCs never see these events, even with a subscription.

## 3. Notifications

### 3.1 `notify`

```jsonc
{"jsonrpc":"2.0","id":8,"method":"notify","params":{
  "title":"Claude Code is done",
  "body":"Refactor finished in ~/code/app (3 files changed).",
  "id":"claude",                      // optional: same id replaces the earlier notification
  "actions":[{"id":"open","label":"Open terminal"},{"id":"ok","label":"OK"}],
  "priority":"high"}}
{"jsonrpc":"2.0","id":8,"result":{"id":"claude"}}
```

| Param | Rules |
|---|---|
| `title` | Required. Display text (§8), 1–100 code points, no newlines |
| `body` | Optional, default `""`. Display text, 0–1000 code points, newlines allowed |
| `id` | Optional. An id (§8). Posting with an id this PC already used replaces that notification. Omitted: the phone picks a new unique id |
| `actions` | Optional, 0–3 items, each `{"id": <id>, "label": <display text, 1–30 code points, no newlines>}`. Action ids unique within the notification |
| `priority` | Optional, `"default"` or `"high"`. `high` MAY show as a heads-up notification |

The result has the notification's `id`. The phone shows `title` and `body` as given and the PC's name (§1) as the
notification's source, on its own notification channel *PC notifications*.

- A PC has at most **16** notifications on the phone; posting a 17th removes its oldest.
- The phone MAY limit the rate per PC; above its limit `notify` fails with `-32033` (`rate_limited`). A phone that
  limits SHOULD allow at least 30 per minute.

### 3.2 Events

The value of every `notification` event has `notification` (the id). `action` also has `action` (the action id).

| `type` | When |
|---|---|
| `action` | The user pressed one of the `actions`. The notification goes away |
| `tap` | The user tapped the notification itself. It goes away; the phone doesn't open anything |
| `dismiss` | The user swiped it away |

Nothing is sent when the PC replaces or cancels a notification, or when it's removed for §1 or the limit of 16.

```jsonc
{"jsonrpc":"2.0","method":"event","params":{"subscription":"s-2","seq":0,
  "source":"notification","type":"action","value":{"notification":"claude","action":"open"},
  "unit":"","ts":1760000000123,"ts_mono_ns":81234567890123,"device":"635f3a01db1859bc"}}
```

### 3.3 `notify_cancel`

Params: `id` (required, an id). Removes this PC's notification with that id. The result is `{}`, also when there
is no such notification (it may have been dismissed already).

## 4. Panels

### 4.1 Lifetime

A panel has an id chosen by the PC (`panel`, an id per §8), a `title` and a layout (§5). A PC has at most
**8** panels on the phone.

- **`panel_show`** creates the panel, or replaces this PC's panel with the same id (new layout, empty canvases,
  `pseq` back to 0). The calling session becomes its **owner**.
- **Attached / detached.** A panel is *attached* while its owner session is open. When the owner session closes,
  the panel is *detached*: it stays on screen greyed out with *Disconnected* in its frame, its widgets are
  disabled, but **canvases keep taking ink** (§7). Another session of the same PC takes it over with
  `panel_attach` (§4.4) or replaces it with `panel_show`.
- A detached panel is removed **10 minutes** after it was detached if nobody attached it (a phone MAY keep it
  longer). After that it's as if it never existed (`-32031`).
- **Closing.** The PC closes a panel with `panel_close`. The user closes it with the close button in its frame;
  the owner then gets a `state` event `closed` (§6.1). Either way the panel and its canvas content are gone.
- Panels live in the phone app's memory. They don't survive the app being killed or restarted.

### 4.2 Owner

Only the owner session gets the panel's events. Any session of the same PC may call `panel_update`,
`panel_close`, `canvas_clear` and `panel_list` without becoming the owner. `panel_show` and `panel_attach`
make the caller the owner, taking it from another session of the same PC if there was one.

### 4.3 `panel_show`

```jsonc
{"jsonrpc":"2.0","id":9,"method":"panel_show","params":{
  "panel":"deck","title":"Stream deck",
  "layout":{"type":"grid","cols":3,"children":[
    {"type":"button","id":"mute","label":"🔇 Mute"},
    {"type":"toggle","id":"mic","label":"Mic","value":true},
    {"type":"slider","id":"vol","label":"Volume","min":0,"max":100,"step":1,"value":40}]},
  "keep_awake":true}}
{"jsonrpc":"2.0","id":9,"result":{"panel":"deck","visible":false}}
```

| Param | Rules |
|---|---|
| `panel` | Required. An id (§8) |
| `title` | Required. Display text, 1–60 code points, no newlines. Shown in the frame |
| `layout` | Required. The root widget (§5) |
| `keep_awake` | Optional, default `false`. Keep the screen on while the panel is visible and attached |
| `orientation` | Optional: `"any"` (default), `"portrait"` or `"landscape"`. Applied while the panel is visible |

The result has `panel` and `visible`, whether the panel is on screen now (§6.1).

**Bringing it on screen.** Android doesn't let an app in the background open a screen by itself. So:

- If the phone is already showing a panel of **the same PC** in the foreground, it switches to the new panel
  (or redraws it in place, when it's the same panel).
- Otherwise the phone posts a notification *"<PC name> wants to show <title>"*; tapping it opens the panel.
  A panel of another PC on screen is never replaced: the new panel is only added to the frame's panel switcher.
- The `state` event `visible` (§6.1) tells the owner when the user actually sees it.

Errors: `-32030`, `-32032` (`too_many_panels`) when this would be the PC's 9th panel, `-32602` for an invalid
layout.

### 4.4 `panel_attach`

Makes the calling session the owner of an existing panel, typically after a reconnect, and re-enables it.

```jsonc
{"jsonrpc":"2.0","id":10,"method":"panel_attach","params":{"panel":"pad","after_pseq":41}}
{"jsonrpc":"2.0","id":10,"result":{"panel":"pad","title":"Sketch","visible":true,"last_pseq":57,
  "values":{"mic":true,"vol":72}}}
```

| Param | Rules |
|---|---|
| `panel` | Required |
| `after_pseq` | Optional integer ≥ -1, default -1. The last `pseq` (§6) the client has processed |

The result has `panel`, `title`, `visible`, `last_pseq` (the `pseq` of the panel's latest event so far, -1 if it
had none) and `values`: the current value of every toggle and slider, by widget id (the user may have changed one
just before the connection dropped).

**Right after the response**, the phone replays the canvas strokes it keeps (§7.3) with `pseq` > `after_pseq`, in
`pseq` order, to this session's `panel` subscriptions, then sends new events as usual. Replayed values are
identical to the first time except for `"replayed": true`.

Errors: `-32030`, `-32031` (`unknown_panel`).

### 4.5 `panel_update`

Changes properties of widgets in place, without resetting anything else.

```jsonc
{"jsonrpc":"2.0","id":11,"method":"panel_update","params":{"panel":"status","updates":[
  {"widget":"msg","set":{"text":"Claude: done ✅","tone":"good"}},
  {"widget":"bar","set":{"value":1}}]}}
{"jsonrpc":"2.0","id":11,"result":{}}
```

- `updates`: 1–64 items, each `{"widget": <id>, "set": {...}}`. `set` holds properties of that widget's type
  (§5) with the same rules as in a layout; `type`, `id` and `children` can't be set, and unknown members are
  ignored. To change the structure, call `panel_show` again.
- The update is **atomic**: if any item is invalid nothing changes and the error is `-32602`. Invalid includes a
  widget id the panel doesn't have and a value that breaks the rules of that widget's type.
- Setting `value` of a toggle, slider or text field from the PC does not produce a `change` event.
- Works on detached panels too (the change shows, the panel stays detached).

Errors: `-32030`, `-32031`, `-32602`.

### 4.6 `panel_close`, `panel_list`, `canvas_clear`

- **`panel_close`** — params `panel`. Removes the panel. Result `{}`. Errors `-32030`, `-32031`.
- **`panel_list`** — no params. Result `{"panels":[{"panel":"deck","title":"Stream deck","attached":true,
  "visible":false}]}`: this PC's panels, in creation order (a panel replaced by `panel_show` keeps its place). `attached` is true while some session owns it.
  Errors `-32030`.
- **`canvas_clear`** — params `panel` and `widget` (a canvas). Erases the canvas and the strokes kept for it
  (§7.3). Result `{}`. Errors `-32030`, `-32031`, and `-32602` if the widget doesn't exist or isn't a canvas.
  Strokes the user drew just before may still arrive after the response.

## 5. Layout

A layout is one widget object, the root, which fills the screen below the frame. Every widget has `type`; the
other members depend on it. Unknown members are ignored. Any rule broken anywhere in the tree makes the request
`-32602`.

- **`id`** — an id (§8), unique within the panel. **Required** for `button`, `toggle`, `slider`, `text_input` and
  `canvas` (they produce events); optional for the others (needed only to `panel_update` them).
- **`weight`** — optional number > 0, in the children of `row` and `column`: after children without a weight get
  their natural size, the space left is shared between the weighted ones in proportion. A `canvas` without a
  `weight` counts as `weight` 1.
- **`enabled`** — optional, default `true`, on interactive widgets. A disabled widget is shown greyed and sends
  nothing.
- **`tone`** — optional on `text`, `button` and `progress`: `"default"`, `"accent"`, `"good"`, `"warn"` or
  `"bad"`. The phone maps it to its theme's colors.
- At most **256** widgets in a layout, and at most **8** levels deep (the root is level 1).

| `type` | Members (besides the common ones) | Events |
|---|---|---|
| `column`, `row` | `children`: 1–64 widgets, top to bottom / start to end | — |
| `grid` | `cols`: integer 1–8; `children`: 1–64 widgets, filled row by row. Cells are all the same width. A grid given a height (the root, or a `weight`) shares it equally between its rows and widgets fill their cell; otherwise each row is as tall as its tallest widget | — |
| `text` | `text`: display text, 0–4000 code points, newlines allowed; `size`: `"small"`, `"body"` (default), `"title"`, `"display"`; `align`: `"start"` (default), `"center"`, `"end"` | — |
| `button` | `label`: display text, 1–100 code points, no newlines | `press` |
| `toggle` | `label`: as `button`; `value`: boolean, default `false` | `change` |
| `slider` | `label`: display text, 0–100, no newlines; `min` (default 0) and `max` (default 1), numbers with `min` < `max`; `step`: optional number > 0; `value`: number, default `min`; a value outside `[min, max]` is clamped to it | `change` |
| `progress` | `label`: as `slider`; `value`: number in `[0, 1]`, or `null` for indeterminate (default) | — |
| `text_input` | `label` and `placeholder`: display text, 0–100, no newlines; `value`: display text, default `""` (newlines allowed even when single-line); `multiline`: boolean, default `false`; `max_length`: integer 1–4000, default 1000 (`value` must not be longer); `clear_on_submit`: boolean, default `true` | `submit` |
| `canvas` | `ink`: boolean, default `true`; `color`: color (§8), default `"#000000"`; `width`: stroke width in dp, 0.5–64, default 4; `background`: color, default `"#ffffff"` | `stroke` |

Unknown `type` values are `-32602`, so a typo isn't silently dropped.

There is no password field on purpose. A `text_input` always shows *Sent to <PC name>* under it.

## 6. Panel events

Every `panel` event value has `panel` (the panel id) and **`pseq`**: an integer that starts at 0 when the panel
is created by `panel_show` and goes up by one for every event of the panel, whichever session receives it (or
none). Events about a widget also have `widget` (its id). Unlike the event's own `seq`, `pseq` survives
reconnects, which is what `panel_attach` uses (§4.4).

### 6.1 `state`

`{"panel":"deck","pseq":0,"state":"visible"}`, with `state`:

| `state` | When |
|---|---|
| `visible` | The panel is now on screen: the app is in front, the screen is on and this panel is the one shown |
| `hidden` | It no longer is (the user switched panel or app, or the screen went off) |
| `closed` | The panel was removed by the phone; `reason` is `"user"` (close button) or `"revoked"` (§1). Nothing follows |

### 6.2 Input

| `type` | Widget | Extra value members |
|---|---|---|
| `press` | `button` | — |
| `change` | `toggle` | `value`: the new boolean |
| `change` | `slider` | `value`: the new number (snapped to `step` if set, within `[min, max]`); `final`: `true` on release |
| `submit` | `text_input` | `value`: the text. Sent when the user presses the send key (single line) or the send button |
| `stroke` | `canvas` | §7 |

While dragging, a slider sends at most about 20 `change` events per second, and always one with `"final": true`
when released. The phone updates toggles and sliders on screen at once, without waiting for the PC.

## 7. Canvas strokes

### 7.1 Event value

Each finger or stylus touching a canvas draws one **stroke**. A stroke arrives as a series of `stroke` events,
each carrying the points recorded since the previous one:

```jsonc
{"panel":"pad","widget":"ink","pseq":57,
 "stroke":12,"phase":"move","tool":"finger",
 "points":[[0.4120,0.2331,0.61,16],[0.4153,0.2402,0.63,24]]}
```

| Member | Meaning |
|---|---|
| `stroke` | Integer, unique within the canvas, increasing in the order strokes start. Several fingers draw several strokes at once, so chunks of different strokes interleave |
| `phase` | `start` (first chunk: the touch-down point, maybe more), `move`, `end` (the finger lifted), or `cancel` (the system took the touch away, e.g. palm rejection; the PC SHOULD discard the stroke). Exactly one `start` and then exactly one `end` or `cancel` per stroke |
| `tool` | `"finger"`, `"stylus"`, `"eraser"` (the stylus's eraser end), `"mouse"` or `"unknown"` |
| `points` | 0–128 points `[x, y, pressure, t]`. `x` and `y` are in `[0, 1]`, relative to the canvas's width and height, origin top-left, clamped when the finger leaves the canvas; 4 decimals are enough. `pressure` is in `[0, 1]` (1 when the device can't measure it). `t` is integer milliseconds since the stroke's first point, never decreasing. `start` has at least one point; `end` and `cancel` may have none |
| `size` | Only in `start`: `[width, height]` of the canvas in dp when the stroke began, so the PC can keep the aspect ratio |

With `"ink": false` the phone draws nothing and the canvas is a touchpad; the events are the same.

### 7.2 Timing

The phone draws ink locally at once; the PC sees the stroke while it's being drawn.

- The phone SHOULD send a chunk about once per display frame while points are pending: no sooner than ~16 ms
  after the previous chunk of the stroke, and no later than ~33 ms after the oldest unsent point. Android
  delivers touch input batched per frame (`MotionEvent` history), so this is one event per frame and finger.
- It MUST send `end` (or `cancel`) as soon as the touch ends, without waiting.
- Servers SHOULD disable Nagle's algorithm (`TCP_NODELAY`) on their sockets, so TCP doesn't hold small frames back.

### 7.3 Keeping strokes across disconnects

The phone keeps the `stroke` events of each canvas since its last clear (`canvas_clear` or `panel_show`): the
canvas content is made of them anyway. They're what `panel_attach` replays (§4.4), so:

- Strokes drawn while the panel is detached are kept and replayed after `panel_attach`.
- A client that remembers the last `pseq` it processed reattaches with that `after_pseq` and gets exactly what it
  missed. A client that starts from scratch passes -1 (or nothing) and gets the whole canvas content.
- Only strokes are kept. `press`, `change`, `submit` and `state` events that found no subscription are lost;
  `panel_attach`'s `values` covers toggles and sliders.
- A phone MUST keep at least **100 000 points** per canvas. When a canvas reaches the phone's limit it stops taking
  ink, shows *Canvas full*, and sends nothing until it's cleared.

## 8. Value rules

- **Id**: a string matching `^[A-Za-z0-9_.-]{1,64}$`.
- **Display text**: counted in Unicode code points (not UTF-16 units). It MUST NOT contain control characters
  (category Cc), except U+000A (newline) where a field allows newlines, nor bidirectional controls (U+061C,
  U+200E, U+200F, U+202A–U+202E, U+2066–U+2069). The phone shows it as given, without trimming.
- **Color**: a string `#rrggbb` (hex digits, either case).
- Numbers are JSON numbers; where the table says integer, `5.5` and `"5"` are `-32602`.

## 9. Order of checks and errors

These methods follow `PROTOCOL.md` §2. Step 7 (`-32602`) covers everything in §3–§5 and §8 that can be checked
from the request alone, including the whole layout. Step 8 then checks, in this order: permission (`-32030`),
the panel exists (`-32031`) or the limit (`-32032`), what depends on the panel's widgets (`-32602`, e.g. an
unknown widget in `panel_update`), then `-32033`.

`PROTOCOL.md` §7 lists these codes with every other code.

| Code | Name | When |
|---|---|---|
| `-32030` | `not_allowed` | The user hasn't allowed this feature for this PC; `data.permission` is `"notify"` or `"panels"` |
| `-32031` | `unknown_panel` | No panel of this PC has that id (never created, closed, or expired) |
| `-32032` | `too_many_panels` | `panel_show` of a new panel when the PC already has 8 |
| `-32033` | `rate_limited` | `notify` above the phone's rate limit |

## 10. Known gaps

- Images and icons in widgets; the PC drawing strokes onto a canvas; undo.
- Showing a panel on the lock screen.
- A way for the PC to learn which widget types a phone knows, once there are optional ones.
