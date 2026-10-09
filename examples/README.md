# Examples

Small scripts built on the Python client in [`clients/python`](../clients/python). Install it, and pair your phone once with the Go CLI from [`pcd/`](../pcd):

```sh
pip install -e clients/python           # add [discovery] to find the phone via mDNS
phonetopc pair                          # scan the QR code with the phone app
python examples/print_gyro.py           # no address needed: the paired phone, over pinned TLS
```

With several paired phones, set the address of the one you want (`--addr host[:port]`; `print_gyro.py` takes it as its only argument) or `PHONETOPC_ADDR`. Over USB, run `adb forward tcp:8765 tcp:8765` and use `127.0.0.1`.

For a phone in dev mode, or the mock phone (`phonetopc-mock`), pass a `ws://` URL, which means plaintext with no pairing:

```sh
python examples/print_gyro.py ws://127.0.0.1:8765
export PHONETOPC_ADDR=ws://192.168.1.20:8765   # or for every script at once
```

| Script | What it does | Extra requirements |
|---|---|---|
| [`print_gyro.py`](print_gyro.py) | Hello world: prints gyroscope readings at 50 Hz | none |
| [`air_mouse.py`](air_mouse.py) | Moves the mouse pointer with the gyroscope; a Quick Settings tile tap sends a left click. `--scale` sets speed in pixels per radian, `--deadzone` sets the rad/s threshold below which movement is ignored | `pip install evdev`, plus write access to `/dev/uinput` (udev rule + `input` group, see the docstring) |
| [`lock_on_tile.py`](lock_on_tile.py) | Locks the PC when you tap the phonetopc Quick Settings tile. Uses `loginctl lock-session`, then `xdg-screensaver lock`, or your own `--command`. Reconnects when the phone drops off | a locker that reacts to logind's `Lock` signal (hypridle, swayidle, xss-lock, GNOME, KDE), or `--command` |

`air_mouse.py` uses Linux `uinput`, which sits below the display server, so it works the same on Wayland and X11.

For a quick look at any sensor without writing code:

```sh
python -m phonetopc list
python -m phonetopc tail gyro --rate 50
python -m phonetopc get battery
python -m phonetopc list --dev --addr 127.0.0.1   # dev mode / the mock
```
