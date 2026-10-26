# phonetopc for Android

The phone side of phonetopc: an app with a foreground service that serves the phone's sensors to paired PCs, lets
a PC ask for fingerprint approval, and shows the PC's notifications and panels. The protocol is in
[`../spec/`](../spec/): [`PROTOCOL.md`](../spec/PROTOCOL.md), [`SECURITY.md`](../spec/SECURITY.md),
[`APPROVAL.md`](../spec/APPROVAL.md) and [`PANELS.md`](../spec/PANELS.md).

Needs Android 8.0 (API 26) or newer.

## Install

Download `phonetopc-<version>.apk` from the [releases](https://github.com/melker12345/phonetopc/releases) and
install it (`adb install -r phonetopc-<version>.apk`, or open it on the phone and allow installs from your
browser or file manager). Releases are signed with a key whose certificate has the SHA-256 fingerprint

```
95:F1:4A:58:F8:71:C2:90:C4:39:A7:5D:24:76:C8:CD:FF:C4:CF:BB:7E:F2:2F:3E:E3:1D:BB:AD:44:4C:A6:75
```

Check it with `apksigner verify --print-certs phonetopc-<version>.apk`. A build you make yourself is signed with
another key, so Android won't update one with the other: uninstall first (which also removes the pairings).

## Build and install

You need JDK 17 or newer and the Android SDK with platform `android-36`. Point Gradle at the SDK with
`ANDROID_HOME`, or put `sdk.dir=/path/to/Android/Sdk` in `local.properties`. The Gradle wrapper downloads Gradle.

```sh
cd android
./gradlew assembleDebug        # app/build/outputs/apk/debug/app-debug.apk
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

A release build (`./gradlew assembleRelease`) is signed when `PHONETOPC_KEYSTORE`, `PHONETOPC_KEYSTORE_PASSWORD`,
`PHONETOPC_KEY_ALIAS` and `PHONETOPC_KEY_PASSWORD` are set, and left unsigned otherwise.

## Pair a PC and start the server

1. Open phonetopc and go through the short onboarding (or skip it).
2. On the PC, run `phonetopc pair`. It shows a QR code and also advertises itself on the network.
3. On the phone, tap **Pair a PC** and pick one:
   - **Scan QR code**: point the camera at the code, then confirm on the PC.
   - **Find on network**: tap your PC in the list. Both screens show a 6-digit code; tap **Codes match** only if
     they are the same, and confirm on the PC too.
4. **Home → Start server.**

**Devices** lists paired PCs and whether they are connected. **Revoke** forgets a PC and disconnects it at once.
**Settings → About → This phone's key** shows the phone's pin; after pairing, the PC shows the same value.

Optional: change the **Port** (default 8765) and turn off sensors you don't want to share (they are reported as
unavailable). **Location** is off until you turn it on.

The service's notification has **Pause** (clients stay connected, sensors stop) and **Stop**. Add the **PC button**
Quick Settings tile to send `ui`/`tile_tap` events.

## Fingerprint approval

A paired PC can ask you to confirm something (`phonetopc approve`, or `POST /v1/approvals` on the PC). You get a
notification naming the program and the PC, with the reason and **Approve** / **Deny**. Approve shows three numbers:
pick the one the PC shows, then confirm with your fingerprint. A wrong number refuses the request. If the app is in
front, the numbers open directly. After three refusals from a PC within 10 minutes, its requests are ignored for 5
minutes. On a locked phone the reason is hidden and you unlock first.

It needs a strong (Class 3) biometric with at least one fingerprint enrolled, and is not available in developer
mode. **Settings → Approvals** shows whether it is available. Adding or removing a fingerprint replaces the
approval key; the PC then refuses approvals until you run `phonetopc approval-key --accept` there. The phone's
secure hardware attests the key, so the PC knows it needs a fingerprint for every use; with an unlocked bootloader
or a custom OS the PC refuses it unless you accept it with `phonetopc approval-key --accept --unattested`. Details
are in [`APPROVAL.md`](../spec/APPROVAL.md).

## Notifications and panels

A paired PC can post notifications and show full-screen panels of buttons, sliders, text and a drawing canvas.
Both are **off for every PC** until you turn them on under **Devices → *PC* → Notifications / Screen panels**
(and off again after re-pairing).

Android doesn't let a background app open a screen, so a new panel usually arrives as a "*PC* wants to show
*title*" notification. Open panels are also listed as cards on **Home**. When the PC disconnects, its panels grey
out and are removed after 10 minutes; they don't survive the app being killed.

## Developer mode

**Settings → Developer mode (unencrypted)** restarts the server as plain `ws://` that accepts anyone, without
pairing. It is for trying the protocol by hand; Home shows "Developer mode · unencrypted" while it's on.

```sh
adb forward tcp:8765 tcp:8765    # over USB; or use the address shown on Home
websocat ws://127.0.0.1:8765/v1/ws
```

Then type one JSON request per line:

```json
{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"websocat"}}}
{"jsonrpc":"2.0","id":2,"method":"list_sensors"}
{"jsonrpc":"2.0","id":3,"method":"subscribe","params":{"source":"gyro","rate_hz":50}}
```

## Troubleshooting

- **The server stops in the background.** Some phone makers' battery managers kill it. **Settings → Keep running
  in the background** asks for the battery optimization exemption and shows steps for your brand.
- **Start after a reboot.** Turn on **Settings → Start on boot**. It starts once you unlock the phone. After an app
  update it restarts by itself if it was running.
- **The PC can't find the phone.** Many networks block mDNS; the PC then uses the phone's last known address. On
  networks that isolate clients, use USB: `adb forward tcp:8765 tcp:8765` and `--addr 127.0.0.1:8765` on the PC.
- **No location.** Grant location access and turn location on. After a reboot with only "while using the app"
  access, location flows once you open the app.
- **No NFC events.** NFC must be on in the system settings, and tags are read only while the screen is on and
  unlocked. Tags that another app claims (such as URL stickers that open the browser) may not reach phonetopc
  unless it is in front.
- **Approvals answer `unavailable`.** Enroll a fingerprint, and allow notifications for phonetopc (requests can't
  be shown otherwise unless the app is open).
- **Uninstalling** deletes the phone's key; PCs then have to pair again.

## Development

| Module | What it is |
|---|---|
| `protocol/` | Pure Kotlin/JVM: messages, sensor catalog, JSON-RPC session, security core, approvals, panels |
| `server/` | Pure Kotlin/JVM: Ktor WebSocket server and the TLS frontend |
| `pairing/` | Pure Kotlin/JVM: the phone's side of pairing |
| `app/` | The Android app |

```sh
./gradlew testDebugUnitTest    # unit tests, including spec/conformance cases and vectors
./gradlew lint
```
