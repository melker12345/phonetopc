# phonetopc

Use your phone's sensors from programs on your PC.

Install the app on your Android phone and pair it with your PC once. After that, any program on the PC can
read the phone's gyroscope, battery, location, NFC tags and more, ask you to approve something with your
fingerprint, decrypt files only after your fingerprint, post notifications on the phone, or show buttons and sliders there. Programs do this with plain
HTTP requests to a daemon on the PC, so any language works and no phone-specific code is needed.

```sh
curl -N 'localhost:8767/v1/events?source=gyro'
{"seq":0,"source":"gyro","type":"rotation","value":[0.01,-0.32,0.05],"unit":"rad/s",...}
{"seq":1,"source":"gyro","type":"rotation","value":[0.02,-0.30,0.04],"unit":"rad/s",...}
```

## How it works

```
 your program  ──HTTP──▶  phonetopcd  ──encrypted WebSocket (Wi-Fi or USB)──▶  phone app
   (on the PC)            (on the PC)                                          (Android)
```

- **The phone app** runs a small server in the background. It only accepts PCs you have paired with it.
- **`phonetopcd`** keeps the connection to the phone. It deals with finding the phone on the network,
  encryption and reconnecting, and offers the phone to local programs as an HTTP API.
- **Your program** talks to `phonetopcd`, using whatever HTTP client its language has.

## What's in this repository

| Path | What |
|---|---|
| [`android/`](android/) | The phone app (Kotlin) |
| [`pcd/`](pcd/) | The PC side (Go): the `phonetopcd` daemon, the `phonetopc` command line tool, a fake phone for testing without one, and a Go library |
| [`spec/`](spec/) | The specifications: [`LOCAL-API.md`](spec/LOCAL-API.md) is the HTTP API your programs use; the rest describe the protocol between the PC and the phone |

## Getting started

You need Go 1.27 or newer on a Linux PC, and an Android 8.0 or newer phone on the same network.

```sh
cd pcd && go build -o bin/ ./cmd/...   # builds phonetopcd, phonetopc and phonetopc-mock
```

**With a phone:** build and install the app ([`android/README.md`](android/README.md)), then:

```sh
sudo bin/phonetopc daemon install --system   # phonetopcd as a system service, as its own user
sudo bin/phonetopc pair                      # shows a QR code; scan it in the app
curl localhost:8767/v1/status                # check that it works (after logging in again)
```

The system service starts when a program first connects, the way `ssh.socket` starts sshd. It keeps the pairing
where programs running as you can't read it, and lets members of the group `phonetopc-users` use the phone;
`install` offers to add you, which takes effect at your next login. Without root, `bin/phonetopc pair` keeps the
pairing in your home directory and offers to run `phonetopcd` as you instead.

**Without a phone**, the fake phone stands in for it:

```sh
bin/phonetopc-mock &
bin/phonetopcd --insecure-dev &
curl -N 'localhost:8767/v1/events?source=gyro'
```

Then write your program against [`spec/LOCAL-API.md`](spec/LOCAL-API.md).

## Status

Works: pairing, the sensors, fingerprint approval, file decryption with age, notifications and panels, the daemon. Not yet: released
builds of the app and the PC tools.
