# phonetopc for Android

The phone side of phonetopc: a foreground service that serves the phone's sensors over
WebSocket JSON-RPC, as specified in [`../spec/PROTOCOL.md`](../spec/PROTOCOL.md)
(`phonetopc/1`, draft 3), [`../spec/SECURITY.md`](../spec/SECURITY.md) (pairing, TLS and
tokens, milestone M2) and [`../spec/APPROVAL.md`](../spec/APPROVAL.md) (fingerprint approval).

The server runs in one of two modes, on the same port (default 8765):

| Mode | URL | Who can connect |
|---|---|---|
| **Secure** (default) | `wss://<phone>:8765/v1/ws` | Paired PCs only: pinned TLS plus the PC's token in `hello` |
| **Developer mode** | `ws://<phone>:8765/v1/ws` | Anyone on the network, no encryption (draft 1). Off by default, behind a warning |

## Layout

| Module | What it is |
|---|---|
| `protocol/` | Pure Kotlin/JVM. Message models, the sensor catalog, the JSON-RPC session and the hub that drives sensors and fans out events. Also the security core (`protocol.security`): SPKI pins, base64url, the pairing code, constant-time compare, tokens, the failed-hello rate limiter. And `request_approval` (`protocol.approval`): param rules, the signed message, one approval at a time, the timeout, all behind an `Approver` interface. No Android imports. |
| `server/` | Pure Kotlin/JVM. Ktor (CIO) WebSocket server on `/v1/ws`, and the TLS frontend for secure mode. |
| `pairing/` | Pure Kotlin/JVM. The phone's side of pairing: QR payload parser, pinned `wss://` WebSocket client, the pairing state machine. |
| `app/` | The Android app: sensors, foreground service, Keystore keys (TLS and approval), paired PCs, mDNS, QR scanner, approval notification and biometric prompt, Compose UI. |

## Requirements

- JDK 17 or newer (built and tested with JDK 21).
- Android SDK with platform `android-36`. Point Gradle at it with `ANDROID_HOME`, or put
  `sdk.dir=/path/to/Android/Sdk` in `local.properties` (not committed).
- Nothing else: the Gradle wrapper downloads Gradle.

The app compiles against API 36. Some of the newest AndroidX releases already need
API 37, so `gradle/libs.versions.toml` pins the latest versions that work with 36.

## Build, test, install

```sh
cd android
./gradlew assembleDebug        # app/build/outputs/apk/debug/app-debug.apk
./gradlew testDebugUnitTest    # also runs the :protocol, :server and :pairing JVM tests,
                               # including spec/conformance (server cases, secure server
                               # cases, pairing and approval vectors)
./gradlew lint
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

## Pair a PC and start the server

1. Open phonetopc and go through the short onboarding (or skip it).
2. On the PC, run `phonetopc pair`. It shows a QR code and also advertises itself on the
   network.
3. On the phone, open **Pair** and pick one:
   - **Scan QR code**: point the camera at the code. The phone checks that the PC it reaches
     has the key in the code, then your PC asks you to confirm.
   - **Find on network**: tap your PC in the list. Both screens show a 6-digit code; tap
     **Codes match** only if they are the same, and confirm on the PC too.
4. **Home → Start server.** Paired PCs connect with `wss://` and their token; `phonetopc tail`
   and friends do that for you.

**Devices** lists paired PCs with when they were paired, when they last connected and
whether they are connected now. **Revoke** forgets a PC and disconnects it at once.

**Settings → About → This phone's key** shows the phone's pin (`sha256/…` and a short hex
form). After pairing, the PC shows the same value for this phone.

Optional: change the **Port** (default 8765) and turn off sensors you don't want to share.
Turned-off sensors are reported as `available: false`.

The notification (`phonetopc · N clients connected`) has **Pause** (clients stay connected,
sensors stop) and **Stop**. Add the **PC button** Quick Settings tile to send
`ui`/`tile_tap` events.

## Fingerprint approval

A paired PC can ask you to confirm something (e.g. `examples/approve.py`, or the client
libraries' `request_approval`). The phone answers with a signature from a key that only
works right after a fingerprint check, and the PC verifies it itself (APPROVAL.md).

- **When it's offered.** Secure mode only (developer mode has no such method), and only while
  the phone has a strong (Class 3) biometric with at least one fingerprint enrolled. Then
  `hello` lists capability `approval` and returns `approval: {key, pin}`.
  **Settings → Approvals** shows whether approval is available (with a shortcut to enroll a
  fingerprint) and the approval key's pin.
- **What you see.** A heads-up notification on the *Approval requests* channel:
  "*arch-desktop* asks for approval", the reason as text, and **Approve** / **Deny**.
  Approve (or tapping it) opens the fingerprint prompt: the PC's name as title, the reason as
  subtitle, and a **Deny** button. Deny in the notification answers at once. When the request
  ends (answered, timed out, the PC disconnected), the notification and prompt disappear.
- **App in the foreground:** the prompt opens directly, without a notification.
- **Notifications blocked** (no POST_NOTIFICATIONS on Android 13+, or the channel turned off):
  requests still work while phonetopc is open; otherwise they are answered `unavailable` at
  once, because nobody could see them. Settings → Approvals says so.
- **Locked phone.** The lock screen only shows "A PC asks for approval" (the reason is hidden
  unless you allow sensitive notifications there). Tapping it asks you to unlock first, then
  shows the prompt, so a locked phone takes two touches. The prompt deliberately doesn't use
  `setShowWhenLocked`: whoever holds a locked phone can't read the request or reach the
  prompt, and a security prompt over the keyguard would be a new surface for no gain.
- **Outcomes.** Deny → `denied`; back, tapping outside or dismissing the notification →
  `cancelled`; the request's `timeout_ms` (or the sensor's own timeout) → `timeout`; too many
  wrong fingers → `lockout`; no usable biometric or no key → `unavailable`; another request
  pending → `busy`. A wrong finger just lets you try again.
- **The key.** A separate ECDSA P-256 Keystore key (never the TLS key), usable once per
  strong-biometric check and with no PIN/pattern fallback, in StrongBox where the phone has
  it. Adding or removing a fingerprint invalidates it; the phone then creates a new one with a
  new pin, and the PC refuses approvals until you accept the new key there
  (`phonetopc approval-key --accept`). Before a signature leaves the phone it is checked
  against the key that session's `hello` announced.

## Developer mode (plain ws://)

For poking at the protocol by hand: **Settings → Developer mode (unencrypted)**, confirm the
warning. The server restarts in plain `ws://` and accepts anyone, without a token, like
draft 1. Home shows "Developer mode · unencrypted" while it's on.

Over **USB** (works on networks that isolate clients; needs USB debugging):

```sh
adb forward tcp:8765 tcp:8765    # PC's 127.0.0.1:8765 now reaches the phone
websocat ws://127.0.0.1:8765/v1/ws
```

Over the **LAN**, use the address from the home screen:

```sh
websocat ws://192.168.1.23:8765/v1/ws
```

Then type requests, one JSON object per line:

```json
{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"websocat"}}}
{"jsonrpc":"2.0","id":2,"method":"list_sensors"}
{"jsonrpc":"2.0","id":3,"method":"subscribe","params":{"source":"gyro","rate_hz":50}}
{"jsonrpc":"2.0","id":4,"method":"get","params":{"source":"battery"}}
```

The phone also advertises itself over mDNS (`_phonetopc._tcp`, TXT `id`, `proto=1`, and
`tls=1` in secure mode or `dev=1` in developer mode):

```sh
avahi-browse -rt _phonetopc._tcp
```

Don't rely on that: many networks block multicast.

## Security notes

- **Key.** On first use the app generates an ECDSA P-256 key in **Android Keystore**
  (hardware-backed where the phone has a TEE or StrongBox), with the self-signed certificate
  Keystore makes for it. The private key is not exportable; the platform TLS stack (Conscrypt)
  signs handshakes through Keystore. Uninstalling the app deletes the key, and PCs then have
  to pair again.
- **TLS.** Ktor's CIO engine can't serve TLS, so in secure mode it listens on `127.0.0.1` on a
  random port, and a small TLS frontend (`server/…/Tls.kt`, a plain `SSLServerSocket`) on the
  public port terminates TLS and relays each connection to it. The frontend tells the server
  the real client address (for rate limiting and the Home screen); loopback connections that
  didn't come through it, e.g. from another app on the phone, are refused. TLS 1.3 and 1.2,
  ECDHE with AES-GCM or ChaCha20-Poly1305 only.
- **Tokens.** Pairing ends with the phone issuing a 32-byte random token. The phone stores only
  its SHA-256 (DataStore, excluded from backups); `hello` tokens are hashed and compared in
  constant time.
- **hello.** In secure mode `hello` checks the params shape (`-32602`), then the token, then
  the protocol version (`-32004`), so a client without a valid token only ever learns
  `-32010`. A missing, malformed, unknown or revoked token gets `-32010` and the connection is
  closed with 1008; so does any failed `hello` after a successful one. A connection that hasn't
  completed `hello` within 10 s is closed with 1008. After 5 failed `hello`s within a minute,
  that address is closed with 1013 (at connect, or at `hello` without an answer), even with a
  valid token.
- **Pairing.** The phone is the client of the PC's pairing listener (default port 8766 on the
  PC; the phone uses whatever port the QR code or mDNS says). QR pairing pins the PC key from
  the code before sending anything; a listed address whose pin differs is skipped like an
  unreachable one. Network pairing uses the committed 6-digit code of SECURITY.md §4.3; if the
  PC's revealed nonce doesn't match its commitment, the phone sends `match: false` and stops.
  The phone waits 5.5 minutes for the PC user's answers. The PC is stored only after it
  answered `pair_finish`. Re-pairing a known PC replaces its token and closes the sessions
  that used the old one with 1008.
- **Camera.** QR codes are decoded with ZXing on CameraX: no Google Play Services or ML Kit, so
  scanning works on de-Googled phones. The permission is asked for on the scan screen, with
  an explanation first.

## How it behaves

- A hardware sensor listener exists only while at least one subscription needs it. It runs
  at the highest rate any subscription asked for, and each subscription is downsampled to
  its own rate.
- A partial wake lock is held only while a continuous sensor (accelerometer, gyro, game
  rotation vector) is streaming. Otherwise the foreground service alone keeps the app alive.
- The `subscribe` response always comes before that subscription's first event. Subscribing
  to `light`, `proximity` or `battery` starts with the current value as `seq` 0 (for light
  and proximity, as soon as the sensor reports it); `ui` sends nothing until the tile is
  tapped. These sources only send an event when the value actually changes.
- If a client can't keep up, continuous events are dropped (visible as `seq` gaps). If
  on_change events back up, the connection is closed with 1008 (spec §5).
- `ts_mono_ns` is `SensorEvent.timestamp`; `ts` is that converted to wall-clock time.
- If the server was running when the app is updated (`adb install -r`), it starts again on
  its own, in the mode that is set. **Settings → Start on boot** (off by default) does the
  same after a reboot, once you unlock the phone. Stopping the server (in the app or from the
  notification) means it stays stopped; Pause doesn't count as stopping. Switching developer
  mode on or off while it runs restarts it in the new mode. If Android
  refuses the background start, you get a "Tap to start phonetopc" notification instead.
- Phone makers' battery managers can still kill the service. **Settings → Keep running in
  the background** asks for the battery optimization exemption and shows steps for your
  brand (Samsung, Xiaomi, OnePlus/OPPO/realme, Huawei), based on
  [dontkillmyapp.com](https://dontkillmyapp.com).
