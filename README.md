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

Milestone M1 (walking skeleton) from [plan.md](plan.md): protocol draft 1 in plaintext **dev mode only**. No pairing or encryption yet, so only use it on a network you trust.

## Repository layout

| Path | What |
|---|---|
| [`spec/`](spec/) | The protocol: [`PROTOCOL.md`](spec/PROTOCOL.md), [`PANELS.md`](spec/PANELS.md) (notifications and panels), JSON Schema, conformance transcripts and cases |
| [`android/`](android/) | Phone app (Kotlin, Jetpack Compose, Ktor) |
| [`pcd/`](pcd/) | PC side in Go: `phonetopc` CLI, `phonetopc-mock` fake phone, Go client library |
| [`clients/python/`](clients/python/) | Python client library |
| [`examples/`](examples/) | Python recipes: print gyro, compass, air mouse, lock PC from a phone tile, stream deck, drawing pad, notify when a command is done |

## Quick start

No phone needed — use the mock:

```sh
cd pcd && CGO_ENABLED=0 go build -o bin/ ./cmd/...
bin/phonetopc-mock &                 # fake phone on 127.0.0.1:8765
bin/phonetopc tail gyro --rate 10    # stream from the Go CLI

pip install -e clients/python
python examples/print_gyro.py ws://127.0.0.1:8765   # same stream from Python (ws:// = dev mode, no pairing)
python examples/stream_deck.py --addr ws://127.0.0.1:8765 --backend none   # a panel; the mock's simulated user presses its buttons (none: leave your real audio alone)
```

With a real phone: build and install the app (see [`android/README.md`](android/README.md)), enable *Developer mode (unencrypted)* in its settings, then point the CLI or a script at the address shown in the app, e.g. `phonetopc --addr 192.168.1.20:8765 list`. Over USB, `adb forward tcp:8765 tcp:8765` makes the phone reachable on `127.0.0.1:8765`.

Any language with a WebSocket library can talk to the phone: send JSON-RPC 2.0 to `ws://<phone>:8765/v1/ws`, starting with `hello`. See [`spec/PROTOCOL.md`](spec/PROTOCOL.md).
