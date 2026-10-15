# phonetopc — Plan

This plan builds on [README.md](README.md). Where it changes or tightens the README, it says so.

## 1. What we're building

phonetopc is a dependable, language-neutral bridge that turns an Android phone into an event source and actuator for a Linux PC. It has three parts:

| Part | Runs on | Job |
|---|---|---|
| **Phone daemon** (`android/`) | Android 8.0+ (any vendor; Samsung is the primary test device) | Reads sensors, serves the API, does pairing and crypto, executes phone-side commands |
| **PC daemon `phonetopcd`** (`pcd/`) | Any Linux distro (Arch is the primary dev machine); Windows later. Written in Go | Discovers and pairs with the phone, keeps one authenticated connection open, and re-exposes it on a local socket that's easy to use |
| **Protocol spec** (`spec/`) | n/a | The actual product. JSON Schema and AsyncAPI documents that any language can implement |

The client libraries (Python first) and the example recipes are thin layers on top of these.

### Compatibility goal

phonetopc should work on as many phones and PCs as possible, not just the developer's setup.

- **Phone:** any Android phone from 8.0 (API 26) up. That's roughly 95%+ of active devices. Samsung, Pixel, Xiaomi, OnePlus, and the rest differ mostly in how aggressively they kill background apps, which the onboarding screens handle per vendor (see §3).
- **PC, Linux:** any distro, any init system, x86_64 and arm64.
  - `phonetopcd` is a single static Go binary with no runtime dependencies.
  - systemd is supported but not required. Ship a systemd user unit, and document running the daemon from OpenRC/runit/s6 or a session autostart.
  - Works on both Wayland and X11. Input injection goes through `uinput`, which sits below the display server.
  - Platform-specific actions (lock the session, etc.) sit behind small adapters with fallbacks: `loginctl` (systemd-logind or elogind), then `xdg-screensaver`, then a user-configurable command.
- **PC, Windows (later, not now):** Go makes this a port of a few files rather than a rewrite. Keep OS-specific code in `pcd/internal/platform/{linux,windows}` from day one. On Windows, the local socket becomes a named pipe (or the localhost WS) and `uinput` becomes `SendInput`.

### Change from the README: add a PC-side daemon

In the README, every PC script talks to the phone directly. That puts mDNS, pairing, TLS certificate pinning, reconnect logic, and key storage into every client in every language, which works against the "any language" goal.

Instead, `phonetopcd` handles all of the hard parts once and exposes:

- `$XDG_RUNTIME_DIR/phonetopc.sock`: a Unix socket serving a **plain HTTP API** (`spec/LOCAL-API.md`). Unix file permissions make it user-only, and it carries no TLS and no tokens.
- `http://127.0.0.1:8767`: the same API for languages or tools that can't use Unix sockets (browsers, some scripting environments). It requires the token stored in `$XDG_RUNTIME_DIR/phonetopc.token`.

*Decided while building it:* plain HTTP rather than the phone protocol (JSON-RPC over WebSocket) on the local side. Every language's standard library can do an HTTP request, so a program needs no SDK at all: `GET /v1/events?source=gyro` streams one JSON event per line (or Server-Sent Events, or a WebSocket, as the request asks), and actions are ordinary POSTs. `curl` in bash counts as a client. Several scripts share one phone connection, and identical streams share one phone subscription, which saves phone battery. Clients can still connect straight to the phone if they want, because the protocol is public. Going through the daemon is just the easy path.

```
 ┌──────────── Phone ────────────┐          ┌──────────────────── PC ─────────────────────┐
 │ sensors ─┐                    │  wss://  │                     ┌─ python script         │
 │ biometric├─► phone daemon ◄───┼──────────┼─► phonetopcd ◄──────┼─ rust/go/node program  │
 │ commands ┘   (FGS, Ktor)      │  LAN/USB │   (unix sock / ws)  └─ bash + websocat       │
 └───────────────────────────────┘          └─────────────────────────────────────────────┘
```

## 2. The API: language-agnostic by design

### 2.1 Wire format: JSON-RPC 2.0 over WebSocket

- **Why JSON-RPC 2.0:** it's a small, stable standard with existing libraries in practically every language. It defines request/response correlation (`id`), errors, and one-way notifications, so we don't have to invent any of that.
- **Requests** go client → phone and get a response: `subscribe`, `unsubscribe`, `list_sensors`, `get`, `command`.
- **Events** go phone → client as JSON-RPC *notifications* (no `id`), with method `event`.
- Every message is one UTF-8 JSON text frame. Binary frames are reserved for a possible future high-rate encoding.

```jsonc
// client → phone
{"jsonrpc":"2.0","id":1,"method":"subscribe",
 "params":{"source":"gyro","type":"rotation","rate_hz":100}}
// phone → client
{"jsonrpc":"2.0","id":1,"result":{"subscription":"s-7f3a"}}
// phone → client (stream)
{"jsonrpc":"2.0","method":"event","params":{
  "subscription":"s-7f3a","seq":1042,
  "source":"gyro","type":"rotation","value":[0.01,-0.32,0.05],"unit":"rad/s",
  "ts":1760000000123,"ts_mono_ns":81234567890123,"device":"pixel-ab12"}}
```

### 2.2 Event envelope (Layer 2)

This tightens the README's `source/type/value/timestamp`:

| Field | Purpose |
|---|---|
| `source`, `type` | As in the README. `source` is a sensor or subsystem; `type` is what happened |
| `value` | Shape defined **per source/type** in JSON Schema (bool, number, array, object) |
| `unit` | Explicit units, so nobody has to guess `rad/s` vs `deg/s` |
| `ts` | Wall-clock Unix ms, for humans and logs |
| `ts_mono_ns` | Android `SensorEvent.timestamp` (elapsed-realtime ns), for computing correct deltas in air-mouse and similar math |
| `seq` | Per-subscription counter, so clients can detect dropped events |
| `device` | Which phone. Leaves room for multiple phones later |

### 2.3 Surface area (v1)

| Method | Description |
|---|---|
| `hello` | Version negotiation (`protocol: "phonetopc/1"`) and capability list. Always the first call |
| `list_sensors` | What this specific phone has, with supported rates |
| `subscribe` / `unsubscribe` | Params: `source`, `type?`, `rate_hz?`, `min_delta?` (server-side filter to save bandwidth and battery), `batch_ms?` |
| `get` | One-shot read (battery, proximity, last known value) |
| `command` | PC → phone actions: `vibrate`, `notify`, `request_biometric`, `flashlight`, `ring` |

**HTTP, secondary.** `GET /v1/sensors`, `GET /v1/sensors/{source}`, `POST /v1/commands/{name}`, and `GET /v1/events?source=…` as Server-Sent Events. This lets curl and one-liners work without a WebSocket library. It uses the same auth and the same JSON bodies.

### 2.4 Change from the README: the API is bidirectional

The README only describes phone → PC events. Several of the motivating use cases also need **PC → phone** calls. Unlock is the main example: the PC has to *ask* the phone to authenticate (see §4.3). That's why `command` is part of v1.

### 2.5 Spec and client deliverables

- `spec/asyncapi.yaml` plus `spec/schemas/*.json`. This is the source of truth.
- `spec/conformance/`: recorded message transcripts that any client implementation can be tested against.
- Official Python client (`clients/python`, asyncio, typed events). Generated types for other languages can come later from the JSON Schemas.
- `docs/` shows the same "subscribe to gyro" example in Python, Node, Rust, Go, and bash/websocat, which demonstrates the language-agnostic claim.

## 3. Phone daemon

- **Stack:** Kotlin, Jetpack Compose with Material 3 for the UI, Ktor server (CIO engine) for WS and HTTP, `NsdManager` for mDNS (`_phonetopc._tcp`), Android Keystore for keys.
- **SDK levels:** `minSdk 26` (Android 8.0), `targetSdk` latest. Use `androidx.biometric` and the androidx compat libraries so older versions don't need separate code paths. Branch on API level only where Android forces it (FGS types on 29+/34+, notification permission on 33+).
- **Foreground service type:** `connectedDevice` (Android 14+ requires a declared type). Test early whether Samsung accepts it without extra hoops. `dataSync` has a 6-hour limit on Android 15, so it isn't usable here.
- **Wake lock policy:** don't hold a permanent wake lock. Hold a `PARTIAL_WAKE_LOCK` only while at least one subscription needs continuous data. Otherwise rely on the FGS alone. This turns the README's flat battery cost into one that scales with use.
- **Sensor rates:** Android 12+ caps sensors at about 200 Hz unless the app declares `HIGH_SAMPLING_RATE_SENSORS`. Declare it and expose the real caps through `list_sensors`.
- **Change from the README, "no real UI":** the app needs a *minimal* UI for:
  - pairing (QR scan)
  - the paired-devices list and revocation
  - the battery-optimization exemption flow, plus per-vendor guidance detected from `Build.MANUFACTURER` (Samsung "Never sleeping apps", Xiaomi autostart, OnePlus/Oppo battery settings; [dontkillmyapp.com](https://dontkillmyapp.com) is the reference)
  - permission grants

  It's still a daemon, just not a headless one.

### 3.1 Phone UI

Goal: modern, clean and simple. The app should be opened rarely, and when it is, it should be obvious what's going on.

- **Design system:** Material 3 with **dynamic color** (Material You) on Android 12+, falling back to a fixed brand palette on older versions. Light and dark themes follow the system. Use edge-to-edge layout and large, readable type.
- **Screens.** There are only four:
  1. **Home / status.** One large status card: *Running · 2 PCs connected · gyro streaming at 100 Hz*. A big on/off toggle for the service. Below it, the connected clients and what each is subscribed to.
  2. **Pair a PC.** Two options, side by side: *Scan QR code* (camera) and *Find on network* (mDNS list). Then the 6-digit confirmation code in large type.
  3. **Paired devices.** List with name, last seen, and a revoke action (swipe or overflow menu).
  4. **Settings.** Port, which sensors may be exposed (per-sensor allow toggles, so the user stays in control of privacy), battery mode, and the vendor-specific "keep me alive" guide.
- **First-run onboarding:** three or four short steps: what this app does → notification permission → battery exemption (vendor-specific) → pair your first PC. Skippable, resumable.
- **Persistent notification:** short and useful (*phonetopc · 1 PC connected*) with *Pause* and *Disconnect all* actions.
- **Not in the app:** charts, logs, rule editors. Debugging belongs on the PC (`phonetopc tail gyro`).
- **Extra trigger surfaces:** a Quick Settings tile, a notification action, and optionally a home-screen widget. These are also event sources (`source: "ui", type: "tile_tap"`) and a cheap way to get "a button on my phone that does X on my PC".

## 4. Security and trust

### 4.1 Pairing

Pairing happens once per phone/PC pair. Running `phonetopc pair` on the PC opens a short-lived pairing listener and offers both ways at once; the user picks one on the phone. The exact protocol is in [`spec/SECURITY.md`](spec/SECURITY.md) §4.

- **QR code (works without mDNS):** the PC prints a QR code with its addresses, its key pin and a one-time secret. The phone scans it, connects to the PC with that pin, and proves it saw the secret. The PC user confirms the phone's name.
- **Network discovery and a 6-digit code:** the phone lists PCs in pairing mode (`_phonetopc-pair._tcp`). Both screens then show a 6-digit code from a committed numeric comparison (as in Bluetooth), and the user confirms on both that they match. This replaces the earlier idea of typing a code into a SPAKE2 exchange: it is as strong against a man in the middle, needs only SHA-256, and is quicker.
- Both ways end with the phone issuing a token to the PC. The PC stores `{phone id, pin, token, addresses}` in `~/.config/phonetopc/devices.json`; the phone shows the PC under *Paired devices*, where it can be revoked.

### 4.2 Transport security

- The phone serves `wss://` with a **self-signed certificate**. The PC pins its public key (SPKI SHA-256) at pairing; no CA, expiry or host name checks.
- The client sends its per-PC token **inside `hello`** rather than as an HTTP header, because some WebSocket clients (browsers) can't set headers. Each token can be revoked from the phone, which also closes that PC's open sessions.
- **Why not the Noise protocol or a custom handshake:** every language already has TLS with WebSockets. Noise would force each direct client to implement it, which breaks the any-language goal. `phonetopcd` hides pinning from local clients anyway.
- Unencrypted dev mode remains as an explicit, off-by-default developer setting.

### 4.3 High-value events, such as unlock: signed challenge-response

A JSON `{"source":"fingerprint","type":"match","value":true}` must never unlock anything. Anyone who obtains a token could forge it. Instead:

1. The PC lock screen (via a PAM module) sends `command: request_biometric` with a random nonce and a short expiry.
2. The phone shows a **BiometricPrompt** bound to a Keystore key with `setUserAuthenticationRequired(true)` and biometric-only auth. The prompt appears as a heads-up notification the user taps, or as a full-screen intent.
3. On success, the Keystore key signs `nonce ‖ pc_id ‖ expiry`. The key cannot be used without a fresh biometric.
4. The PC verifies the signature against the public key pinned at pairing.

This makes unlock replay-proof, and safe even if the transport token leaks.

> **Landmine to verify first:** BiometricPrompt needs a visible Activity. The phone *cannot* passively notice that you touched the fingerprint sensor. The flow has to be prompt-driven, either PC-initiated (above) or user-initiated from the tile or notification. This changes the README's "unlock when fingerprint matches on the phone" into "PC asks, phone prompts, finger confirms". In practice that's better UX and the only secure version.

## 5. Discovery and transports

- **mDNS** advertises `_phonetopc._tcp` with TXT records `id=<device id>` and `proto=1`. Because the cert is pinned, a spoofed advertisement fails to authenticate.
- **Fallbacks are mandatory:** many networks (café, hotel, corporate, some mesh routers) block multicast or isolate clients from each other. Fallback order: mDNS → last known IP → QR re-scan (§4.1 path B) → manual `host:port` in the CLI.
- **USB:** `adb forward tcp:PORT tcp:PORT` gives TCP over USB for free, with no protocol change. It's good for low-latency air-mouse use and for networks with client isolation. The trade-off is that USB debugging must be enabled.
- **Bluetooth: deferred.** RFCOMM and BLE aren't IP, so they would need their own framing of the same JSON-RPC messages. The README's "transport agnostic" claim applies to the message layer, not the WebSocket. That's true but costly to deliver, so it's post-v1.

## 6. Use-case recipes (`examples/`), as proof that the API is sufficient

| Recipe | PC side | Notes |
|---|---|---|
| Air mouse / clicker | `python-evdev` → `uinput` | Needs a udev rule or `input` group membership. Use `game_rotation_vector` (no magnetometer drift) and `rate_hz≈100–200`. Wi-Fi jitter is real, so test USB vs Wi-Fi |
| Lock on leave | `loginctl lock-session` | Heartbeat loss is a weak signal: the phone may be in Doze or roaming. Prefer **PC-side BLE RSSI** of the phone, or combine both with a grace period |
| Unlock | `pam_phonetopc.so`, a **small C** PAM module that asks `phonetopcd` over the Unix socket. Used by any PAM-based locker or login (hyprlock, swaylock, i3lock, GDM, SDDM, greetd, sudo) | Uses the §4.3 flow. Always keep the password as a fallback in the PAM stack. The module is C because PAM loads it into other processes (sudo, the locker); a Go shared library would drag the Go runtime into those, which is fragile |
| Wake PC | Phone sends a **Wake-on-LAN** magic packet | A suspended PC can't receive WebSocket messages. This needs a `command`-like phone-side action plus WoL support in the BIOS/NIC |
| Phone button | Quick Settings tile event → run any script | The simplest "hello world" |

## 7. Milestones

**M0. Spikes (de-risk before building)**

First results, 2026-10-09, Galaxy S25 Ultra (SM-S938B, Android 16 / API 36), screen on, charging:
- Accelerometer and gyro reach about 480 Hz (`HIGH_SAMPLING_RATE_SENSORS` works); game rotation vector caps at 200 Hz. Delivered: 47 Hz at 50 requested, 200 at 200, 471 at 480, with no dropped events over USB.
- Request round trip (`get`): USB via `adb forward` median 1.3 ms / max 2.4 ms; Wi-Fi median 4.8 ms / max 9.4 ms.
- mDNS: `avahi-browse` sees `_phonetopc._tcp` with the right TXT records.
- Still open: screen-off behaviour over an hour or more, battery cost, and BiometricPrompt from a notification.
- On the target Samsung phone: does an FGS with type `connectedDevice` keep gyro events flowing with the screen off for over 1 hour, and what is the battery cost per hour at 50/100/200 Hz?
- Can BiometricPrompt be launched from a notification or full-screen intent while the phone is locked or idle?
- Does `NsdManager` advertise reliably after Wi-Fi reconnects?
- Measured gyro → PC latency and jitter over Wi-Fi vs `adb forward`.

**M1. Protocol v0 and walking skeleton**
- Draft `spec/` (envelope, `hello`, `list_sensors`, `subscribe`).
- Phone: FGS, Ktor WS on a manual port, gyro / accelerometer / battery / proximity. Plaintext, behind a "dev mode" toggle only.
- Python: a 30-line script that prints the gyro stream.

**M2. Security and phone UI**
- Both pairing paths (QR scan and mDNS + code), `wss://` with a pinned cert, per-client tokens, revocation UI.
- Compose UI: home/status, pairing, paired devices, settings, onboarding.
- Remove plaintext mode from release builds.

**M3. `phonetopcd` and the Python client**
- Go, single static binary (`CGO_ENABLED=0`), built for linux/amd64 and linux/arm64. OS-specific code isolated in `pcd/internal/platform/`.
- systemd user unit plus docs for non-systemd setups. Unix socket and localhost WS, reconnect with backoff, subscription multiplexing (dedupe identical subscriptions to the phone).
- `phonetopc` CLI: `pair` (shows the terminal QR code and the mDNS pairing code at the same time), `status`, `list`, `tail <source>`.
- Conformance transcripts, plus the same example in five languages.

**M4. Discovery and commands**
- mDNS on both sides, manual fallback, USB via `adb forward`.
- `command`: vibrate, notify, ring, Quick Settings tile events.

**M5. Biometric challenge-response and unlock**
- Keystore-bound signing key, `request_biometric`.
- C PAM module plus integration guides for common lockers and display managers.

**M6. Recipes and packaging**
- Air mouse, presence lock, WoL, clicker.
- Packages: static binary tarball (works on any distro), AUR, `.deb` and `.rpm` via goreleaser + nfpm. APK via GitHub Releases, with F-Droid later.
- CI: test matrix of Android emulators at API 26, 30, 34 and latest; run the daemon on Arch, Debian/Ubuntu, Fedora and a non-systemd distro (Alpine or Void).

**Post-v1:** Windows port of `phonetopcd`, a possible C port (see §10), Bluetooth transport, multiple phones, binary/CBOR encoding for high-rate streams, more sensors (camera frames? mic level?), a Home Assistant integration.

## 8. Proposed repo layout

```
spec/            asyncapi.yaml, schemas/, conformance/
android/         Kotlin app (Gradle)
pcd/             phonetopcd + CLI (Go)
pam/             pam_phonetopc.so (C)
clients/python/  official client
examples/        recipes
docs/            protocol guide, multi-language quickstarts
```

## 9. Decisions

### Decided

- **PC daemon language:** Go, for now. A C port may come later (§10).
- **Minimum Android version:** API 26 (Android 8.0), for the widest compatibility.
- **Platforms:** any Linux first; Windows later; iOS never.
- **Pairing:** mDNS + code, and QR code as the manual fallback (§4.1).
- **Phone UI:** Material 3, Compose, four screens (§3.1).

### Still open

1. **License.** It matters early if you want community clients. MIT or Apache-2.0 for the spec and clients makes adoption easiest.
2. **Is the Layer 3 rules engine ever in scope?** Recommendation: **no**. Clients *are* the rules engine. At most, ship a tiny optional Python helper (`@on("gyro")` decorators) and let Home Assistant / Node-RED integrations cover the no-code crowd.
3. **App name and icon.** "phonetopc" works for a repo, but the phone app may want a friendlier name.

## 10. Porting `phonetopcd` to C later

This section records what a C port would involve, so the Go code can be written in a way that keeps the option open.

**What Go gives for free, and what C would need instead:**

| Need | Go | C |
|---|---|---|
| WebSocket client/server | `coder/websocket` (small, pure Go) | libwebsockets (large) or wslay plus your own event loop |
| TLS with certificate pinning | stdlib `crypto/tls` | OpenSSL or mbedTLS; pinning is a custom verify callback |
| JSON | stdlib `encoding/json` | cJSON or jansson, with manual memory management for every message |
| mDNS | `hashicorp/mdns` or `grandcat/zeroconf` (pure Go) | Avahi client library (needs the Avahi daemon running, which isn't on every distro) or a hand-rolled responder |
| Concurrency (many clients, reconnects, timers) | goroutines and channels | epoll or libuv / libevent, callbacks, explicit state machines |
| SPAKE2, Ed25519, ECDSA | `x/crypto` and stdlib | libsodium (no SPAKE2) plus OpenSSL, or a vetted standalone implementation |
| QR code in the terminal | small pure-Go library | libqrencode |
| Cross-compiling and Windows | `GOOS=windows`, done | Different socket, TLS and build setup per platform |

**Rough effort:** expect the C version to take about **3–5× the code and time** of the Go version for the same features. The bigger cost is that this daemon is network-facing, parses untrusted input, and guards unlock. In C, every JSON parse, buffer and lifetime is a possible memory-safety bug in a security-critical component. That needs fuzzing (AFL++/libFuzzer) and sanitizers (ASan/UBSan) in CI to be responsible.

**What C would actually gain:** a smaller binary (roughly hundreds of KB instead of about 8–10 MB), lower idle memory, and trivial embedding into other C programs. None of these matter much for a desktop daemon. Where C really is the right tool, the plan already uses it: the PAM module.

**How to keep the option open:**
- The protocol spec and conformance transcripts (§2.5) are the contract. A C port is "another implementation that passes the conformance suite", not a translation of Go code.
- Keep the Go daemon's internals boring: no heavy reflection-based frameworks, and clear module boundaries (transport, pairing, session, local API, platform adapters) that map onto C modules one-to-one.
- Keep the dependency list short and favor libraries that have well-known C equivalents.
