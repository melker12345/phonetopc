# phonetopc for Android

The phone side of phonetopc: a foreground service that serves the phone's sensors over
WebSocket JSON-RPC, as specified in [`../spec/PROTOCOL.md`](../spec/PROTOCOL.md)
(`phonetopc/1`, draft 1).

Draft 1 is **plaintext and unauthenticated**, so the server only runs after you turn on
**Developer mode (unencrypted)** in the app. Pairing and TLS come in a later version.

## Layout

| Module | What it is |
|---|---|
| `protocol/` | Pure Kotlin/JVM. Message models, the sensor catalog, the JSON-RPC session and the hub that drives sensors and fans out events. No Android imports. |
| `server/` | Pure Kotlin/JVM. Ktor (CIO) WebSocket server on `/v1/ws`. |
| `app/` | The Android app: sensors, foreground service, mDNS, Compose UI. |

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
./gradlew testDebugUnitTest    # also runs the :protocol and :server JVM tests
./gradlew lint
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

## Enable developer mode and start the server

1. Open phonetopc. Go through the short onboarding (or skip it).
2. **Settings → Developer mode (unencrypted)** and confirm the warning.
3. Optional: change the **Port** (default 8765) and turn off sensors you don't want to share.
   Turned-off sensors are reported as `available: false`.
4. **Home → Start server.** The card shows the address to use from the PC, for example
   `192.168.1.23:8765`.

The notification (`phonetopc · N clients connected`) has **Pause** (clients stay connected,
sensors stop) and **Stop**. Add the **PC button** Quick Settings tile to send
`ui`/`tile_tap` events.

## Connect from the PC

Over **USB** (works on networks that isolate clients; needs USB debugging):

```sh
adb reverse tcp:8765 tcp:8765    # PC's 127.0.0.1:8765 now reaches the phone
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

The phone also advertises itself over mDNS (`_phonetopc._tcp`, TXT `id`, `proto=1`, `dev=1`):

```sh
avahi-browse -rt _phonetopc._tcp
```

Don't rely on that: many networks block multicast.

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
- Phone makers' battery managers can still kill the service. **Settings → Keep running in
  the background** asks for the battery optimization exemption and shows steps for your
  brand (Samsung, Xiaomi, OnePlus/OPPO/realme, Huawei), based on
  [dontkillmyapp.com](https://dontkillmyapp.com).
