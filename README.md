# phonetopc

Project: a device-agnostic bridge between phone sensors and a PC. Target platforms are Android, specifically Samsung, and Linux, specifically Arch. iOS is explicitly out of scope.

Overview and goals. Build the infrastructure and protocol that make it easy to wire phone sensors to a PC, not a collection of prebuilt features. Users write their own logic, ideally Python on the PC, without touching Android code. Motivating examples: unlock or wake the PC when a fingerprint matches on the phone; use the phone's gyroscope as an air-mouse or presentation clicker; presence detection, where the PC locks itself when the phone leaves the room. It also works the other way: a PC program can post notifications on the phone and show panels there, such as a stream deck, a status screen or a drawing pad, and get the user's input back as events.

Non-goals. Not building each feature as a toggle inside the app. No iOS support. Not requiring users to write or compile Android apps.

Architecture. The phone app is a daemon: a long-lived background process with no real UI. It's implemented as an Android foreground service, with a persistent notification, and it can hold a wake lock, so it survives the screen being off and Android or Samsung battery killing. It exposes a local network server, a WebSocket, and likely plain HTTP too, listening on the phone's LAN address. Using a network socket deliberately sidesteps the Android app sandbox: both other apps on the phone and the PC connect the same way. The chosen integration model is daemon-with-API. The alternatives considered were a Kotlin SDK or library, which was too high a bar since every user would have to build an Android app, and an embedded rule or scripting engine. In the chosen model the phone app is a boring, reliable bridge, and all user logic lives on the PC in any language. Users never write Android code.

Protocol layers, three of them, inspired by the network stack. Layer one, transport: Wi-Fi, Bluetooth, or USB agnostic; it just moves bytes. Layer two, event vocabulary: every event is an object with source, type, value, and timestamp, as JSON on the wire, so all sensors look the same. For example, source fingerprint, type match, value true; or source gyro, type rotation, value an x-y-z array. Layer three, trigger-to-action rules: the "what, when, where", and that's where the user lives, comparable to IFTTT, Tasker, or Home Assistant. Clients subscribe to event types, for example "subscribe to gyro" or "notify me when fingerprint fires", over the WebSocket, and receive JSON event streams.

Phone app responsibilities. Run the foreground service plus wake lock. Read sensors only via normal Android APIs; for example, the fingerprint result comes through BiometricPrompt, so you get the match signal, not raw data. The app owns the moment a sensor fires, not the sensor itself. Advertise itself for discovery via mDNS, the same mechanism that makes printers and Chromecasts auto-appear. Serve the WebSocket and HTTP API, handle subscriptions, and emit events.

PC client responsibilities. Discover the phone via mDNS and open a WebSocket to it. Subscribe to event types, read the JSON stream, and run user logic, with Python emphasized. Perform actions, like wake or lock the PC, move the cursor, or run macros.

Security and trust model. The LAN itself is not the barrier; devices on the same Wi-Fi can talk, and no router holes are needed. Trust and identity are mandatory: device pairing with a key exchange, so a stranger on café Wi-Fi can't send unlock events. This must not be skipped.

Known constraints and landmines. The real wall is the phone side: Android and Samsung aggressive background-process killing. It's mitigated with the foreground service plus wake lock, at a real battery cost that is the user's choice. Sensors are reachable only through their normal Android APIs; no raw sensor hardware access.

Prior art. Apple Handoff and Continuity, the same idea but walled to same-vendor only. Home Assistant, the closest open event-bus analog. IFTTT and Tasker, the trigger-action layer. The market gap exists because no vendor profits from a cross-vendor bridge, and the open community has the least control over the phone, which is the sensor-rich side.

## Status

Protocol `phonetopc/1`, draft 4 ([`spec/`](spec/)). What works:

- **Secure mode by default.** A PC pairs once with `phonetopc pair`, by scanning a QR code or by picking the PC on the phone and comparing a 6-digit code. After that every connection is `wss://` with the phone's key pinned and a per-PC token, and the phone can revoke any PC. Unencrypted dev mode is an opt-in setting in the app.
- **Sensors:** accelerometer, gyroscope, orientation, magnetic field, compass, light, proximity, battery, barometer, location, NFC tags and a Quick Settings tile.
- **Fingerprint approval:** a PC program asks for a fingerprint on the phone and verifies the phone's signature itself.
- **Notifications and panels:** a PC program posts notifications on the phone or shows a panel there (buttons, toggles, sliders, text, a drawing canvas) and gets the input back as events.
- **The local API:** the `phonetopcd` daemon keeps one connection to the paired phone and offers it to programs on the PC as plain HTTP ([`spec/LOCAL-API.md`](spec/LOCAL-API.md)). Any language's HTTP client is enough: `GET /v1/events?source=gyro` streams events, `POST /v1/notifications` posts one. No pairing, TLS pinning or JSON-RPC in your program.
- **On the PC:** the `phonetopc` CLI, the `phonetopc-mock` fake phone and a Go client library, plus a Python client library.

Not there yet (see [plan.md](plan.md)): `phonetopc pair` setting up and starting `phonetopcd` by itself (today: the systemd unit in [`pcd/systemd/`](pcd/systemd/)); unlocking the PC with a fingerprint (the PAM module); packages (AUR, `.deb`, `.rpm`, APK releases).

## Repository layout

| Path | What |
|---|---|
| [`spec/`](spec/) | The local API for programs on the PC: [`LOCAL-API.md`](spec/LOCAL-API.md). The phone protocol: [`PROTOCOL.md`](spec/PROTOCOL.md), [`SECURITY.md`](spec/SECURITY.md) (pairing, TLS and tokens), [`APPROVAL.md`](spec/APPROVAL.md) (fingerprint approval), [`PANELS.md`](spec/PANELS.md) (notifications and panels), JSON Schema, conformance transcripts and cases |
| [`android/`](android/) | Phone app (Kotlin, Jetpack Compose, Ktor) |
| [`pcd/`](pcd/) | PC side in Go: `phonetopcd` daemon, `phonetopc` CLI, `phonetopc-mock` fake phone, Go client library |
| [`clients/python/`](clients/python/) | Python client library |
| [`examples/`](examples/) | Python recipes: print gyro, compass, air mouse, lock PC from a phone tile, stream deck, drawing pad, notify when a command is done |

## Quick start

No phone needed — use the mock:

```sh
(cd pcd && CGO_ENABLED=0 go build -o bin/ ./cmd/...)
pcd/bin/phonetopc-mock &             # fake phone on 127.0.0.1:8765, in dev mode
pcd/bin/phonetopc --insecure-dev tail gyro --rate 10   # stream from the Go CLI

pcd/bin/phonetopcd --insecure-dev &   # the local API, on $XDG_RUNTIME_DIR/phonetopc.sock and 127.0.0.1:8767
curl -sN --unix-socket $XDG_RUNTIME_DIR/phonetopc.sock 'http://phonetopc/v1/events?source=gyro&rate=10'
curl -s --unix-socket $XDG_RUNTIME_DIR/phonetopc.sock http://phonetopc/v1/notifications -d '{"title":"Hello"}'

pip install -e clients/python
python examples/print_gyro.py ws://127.0.0.1:8765   # same stream from Python (ws:// = dev mode, no pairing)
python examples/stream_deck.py --addr ws://127.0.0.1:8765 --backend none   # a panel; the mock's simulated user presses its buttons (none: leave your real audio alone)
```

With a real phone: build and install the app (see [`android/README.md`](android/README.md)), run `pcd/bin/phonetopc pair` on the PC and pair from the app, start the server in the app, then run e.g. `pcd/bin/phonetopc tail gyro`. The CLI and the client libraries find the paired phone themselves (see [`pcd/README.md`](pcd/README.md)). Over USB, `adb forward tcp:8765 tcp:8765` makes the phone reachable on `127.0.0.1:8765` (`phonetopc --addr 127.0.0.1:8765 …`).

Any language with a WebSocket library can talk to the phone: send JSON-RPC 2.0 to `wss://<phone>:8765/v1/ws` with the phone's key pinned, starting with `hello` and the token from pairing ([`spec/SECURITY.md`](spec/SECURITY.md)), or to `ws://<phone>:8765/v1/ws` in dev mode. See [`spec/PROTOCOL.md`](spec/PROTOCOL.md).
