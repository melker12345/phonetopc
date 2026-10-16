# phonetopc audit — 2026-10-09

Four read-only reviews of `main` at `3fd5d88`: Android app, Go PC side, Python client, protocol/crypto design.
Each finding was checked against the code; "tested" means the reviewer reproduced it.

## Status (re-checked 2026-10-16)

| Finding | Status |
|---|---|
| H1 | Fixed (`a43e7ba`). Leftover: `store.PutDevice` doesn't re-check the old pin between the prompt and the save. |
| H2 | Fixed (`58a812c`). |
| H3 | Fixed (`82be0a3`). Leftover: in dev mode Ktor may keep a dropped socket open; nothing is queued for it. |
| M1 | Fixed (`1223f32`). Leftover: tty input isn't flushed right before each prompt. |
| M2 | Fixed (`66329e8`); the optional check code on both screens wasn't built. |
| M3 | Fixed (`50e483f`, `5aec614`). |
| M4 | Fixed (`a80d69e`). |
| M5 | Fixed (`53b22d2`). |
| L-A1 | Fixed (`5ba8591`). |
| L-A2 | Fixed (`eecf6ed`). |
| L-A3 | Fixed (`9526a01`). |
| L-A4 | Fixed (`65c5337`). |
| L-A5 | Fixed (`103c407`). |
| L-G1 | Fixed (`91abdb3`). |
| L-G2 | Fixed (`a43e7ba`, `bf45249`). |
| L-G3 | Fixed (`2ee3cba`). |
| L-P1–L-P4 | Gone with the Python client and examples. |
| L-S1 | Claim softened (`9de3d93`). Binding the key at pairing and key attestation are still to do. |
| L-S2 | Fixed (`2ea44ae`). |
| L-S3 | Fixed (`a43e7ba`, `65c5337`). |
| L-S4 | Fixed (`58a812c`). |
| L-S5 | Fixed (`a370a79`). |
| L-S6 | Loopback exempt from the failed-`hello` limit (`e8bf2d7`). **Open:** one pending approval per phone, so one stolen token keeps every PC's approvals `busy`. |
| L-S7 | Fixed (`9de3d93`). |
| L-S8 | Fixed (`9de3d93`), except `plan.md` §4.3, which still describes the old approval message. |

**Summary:** 3 high, 5 medium, ~20 low. The core cryptography holds up in all three implementations
(pinning, tokens, approval signatures). The problems are in limits, timeouts, prompts and races around it.

## High

| # | Area | Finding | Where |
|---|---|---|---|
| H1 | Go / pairing | **`pair --yes` auto-confirms the 6-digit code**, so anyone on the LAN can pair as your phone, **replacing its entry** (pin, token, approval key). `phonetopc approve` would then trust the attacker. Tested end to end. | `pcd/cmd/phonetopc/pair.go:303-306`, `:104`; `pcd/internal/store/store.go:267-286` |
| H2 | Spec / pairing | **The 6-digit code can be ground down.** Runs aborted after the PC reveals `Npc` don't count as failures, and limits are per address (an IPv6 /64 is unlimited). A MITM can retry against the PC until its code matches the phone's: a few % chance in 5 minutes. | `SECURITY.md` §4.1, §4.3; `pcd/internal/pairing/listener.go` (`sasNonce`) |
| H3 | Android / server | **Pre-auth memory exhaustion.** A client that sends without reading makes replies pile up in an unbounded queue; the hello timeout never closes the session. Tested: open after 40 s with a 2 s timeout, ~530 MB heap. Ends in OOM, killing the service. | `android/protocol/.../Session.kt:59`, `:139`, `:249-251`; `android/server/.../PhonetopcServer.kt:153-165` |

**Fixes**
- H1: `--yes` must not confirm SAS pairing (refuse SAS or don't advertise via mDNS). Refuse or ask when a pairing would replace a device with a different pin. Restrict id characters.
- H2: count every SAS run where `Npc` was revealed (disconnect, timeout, mismatch); small global limit per listener (e.g. 3), then close it. Spec: "MUST NOT reveal a new `Npc` after N runs".
- H3: on close, give the close frame ~1 s then cancel the socket; set `ending` on hello timeout and stop reading; cap pending responses (close 1008 above ~256); allow only a few frames before `hello`.

## Medium

| # | Area | Finding | Where |
|---|---|---|---|
| M1 | Go / pairing | **Stray input answers a later prompt**, and an empty line means *Yes* for QR. Pressing Enter while waiting lets someone with a photographed QR pair unseen. Tested. | `pcd/cmd/phonetopc/pair.go:281-331` |
| M2 | Spec / pairing | **A photographed QR stays valid after a refusal.** The attacker can retry with the same secret and the victim's phone name until the user gives in. | `SECURITY.md` §1, §4.1, §4.2 |
| M3 | Go / pairing | **Any LAN host can block pairing** by holding the single slot open until expiry (idle timer resets on any frame; never counts as a failure). | `pcd/internal/pairing/listener.go:240-247`, `:323-337` |
| M4 | Android / service | **Stop during startup leaves an untracked server running.** The UI says stopped; in dev mode it's an open unauthenticated server; next Start fails with "port in use". | `android/app/.../service/BridgeService.kt:158-168` |
| M5 | Android / TLS | **Slow-handshake slot exhaustion.** `soTimeout` is per read, not a deadline; one byte/s keeps a handshake alive forever. 64 such sockets lock out all PCs. Tested. | `android/server/.../Tls.kt:141`, `:161-163` |

**Fixes**
- M1: flush pending tty input before each prompt / ignore lines typed before it; require explicit `y` for QR on a terminal.
- M2: rotate the secret (new QR) after any `pair_qr` that reached the prompt; warn when a second phone connects during a pending confirmation; optionally a short check code on both screens derived from secret + both pins.
- M3: short deadline (10–30 s) for the first valid pairing request; reset the idle timer only on requests that advance the state.
- M4: run `start()` non-cancellably and stop the server if the job was cancelled meanwhile, or assign `server` before starting.
- M5: hard deadline for handshake + upgrade; per-IP limit on unauthenticated connections.

## Low

### Android
- **L-A1 Loopback check bypass from `127.0.0.2`** (tested). Peers are looked up by source port only; a local app can bind `127.0.0.2:<port>`, be served as the real PC, and get it rate-limited. Fix: also require remote address `127.0.0.1`, key map by (address, port). `PhonetopcServer.kt` `serve()`, `Tls.kt:94`, `:176`.
- **L-A2 Malicious pairing server crashes the app** (tested). Negative 64-bit frame length → `NegativeArraySizeException`, not caught → process killed. Reachable via a fake `_phonetopc-pair._tcp` PC. Fix: reject `v < 0`, catch `Exception` in the reader. `pairing/.../PinnedWebSocket.kt:68-77`, `PairingChannel.kt`.
- **L-A3 Revocation races.** (a) `touch()` and `remove()` can race so a revoked PC reappears in memory while the UI shows it removed; (b) a session authenticated just before revocation can be missed. Fix: Mutex around `_loaded`; set `pc` under the hub lock; re-check token after auth. `PairedPcs.kt:132-138`, `Session.kt:327`, `:339-342`.
- **L-A4 Same-id PC overwrites another pairing.** `upsert` replaces by id without comparing pins; the newcomer inherits the victim's `pc id` used in approvals. Fix: warn or refuse on id match with different pin. `PairedPcs.kt:47-50`.
- **L-A5 Other apps can start the server.** Exported `MainActivity` honours `ACTION_START_SERVER` and persists `desiredRunning`. Fix: non-exported trampoline or own-PendingIntent check. `MainActivity.kt:273-278`.

### Go
- **L-G1 Library can send the token in plaintext:** `Dial(ws://…, WithToken(tok))` is accepted. Fix: error unless `wss://` with a pin. `pcd/client/client.go:274-283`, `:304-306`.
- **L-G2 Terminal escape injection** from phone-supplied id/name/model/values in `pair`, `devices`, `info`, `list`, `tail`. Fix: strip control characters; validate id charset. `pair.go:154`, `:223`; `main.go:320-323`, `:365`.
- **L-G3 PC identity creation not locked:** two first-run processes can each create an id/key; a phone paired by the loser holds an identity that no longer exists. Fix: flock in `PC()` and re-read. `store.go:455-520`.

### Python
- **L-P1 Subscription leak on cancellation:** if `start()` is cancelled right as the response arrives, the subscription stays registered and is never unsubscribed (tested). Fix: on exception in `start()` with `info` set, detach and unsubscribe. `client.py:272`, `:686-708`, `:795-818`.
- **L-P2 Proxy variables reroute connections** (`websockets` ≥ 15 honours `http(s)_proxy`, even for 127.0.0.1) (tested). Pinning still holds; dev-mode traffic goes to the proxy. Fix: `proxy=None` on ≥ 15. `client.py:1030`, `:1121`.
- **L-P3 Store write replaces a symlink and can change owner** (e.g. under `sudo -E`, `devices.json` becomes root-owned). Fix: write through `realpath`; keep uid/gid or refuse when euid differs. `store.py:252-260`, `:280`.
- **L-P4 `examples/air_mouse.py` trusts values** (crash on non-3-lists / NaN; in dev mode any LAN host can move the pointer and click). Fix: validate; warn when not secure.

### Spec / design
- **L-S1 Approval-key trust is weaker than `APPROVAL.md` claims.** Trusted on first sight from `hello`, not bound at pairing, no key attestation. Fix: soften the claim, or carry the key in `pair_finish` and/or verify Android key attestation.
- **L-S2 Ambiguous ids in the signed message:** §5 says use `hello` values but "never values echoed by the phone"; implementations differ. Fix: stored phone id and the PC's own id; fail if `hello` differs.
- **L-S3 PC identity is just a claim** (ids are public); same as L-A4 on the spec level. Define behaviour for duplicate ids on both sides.
- **L-S4 Pairing state not tied to one connection** in the spec. Fix: all steps on one connection; disconnect discards state and counts toward H2's limit.
- **L-S5 Display-text gaps:** `reason` filter misses U+2028/2029, zero-width and other format characters (Cf), U+00AD; PC and phone names have no rules. Fix: reject Cc, Cf, Zl, Zp; same rules for names.
- **L-S6 Denial of service:** one stolen token keeps every approval `busy`; the hello rate limit locks out all `adb forward` clients (all from 127.0.0.1). Fix: one pending approval per PC; exempt or separately count loopback.
- **L-S7 Plaintext selection not specified:** Python silently uses dev mode for a `ws://` `PHONETOPC_ADDR` even with pairings, and suggests `dev=True` after a spoofable mDNS `dev=1`. Fix: dev mode only via an explicit flag; never from env/mDNS; persistent indicator on the phone.
- **L-S8 Inconsistencies:** §4 intro vs §4.4 (pin stored on phone); §3 "re-pair" advice vs §6 address fallback (re-pair advice helps H2/M2 attacks); schema lacks `pair_*` methods, token length (43 chars) and an id pattern (e.g. `^[0-9a-f]{16,64}$`); `plan.md` §4.3 out of date.

## Checked and sound

- **Pinning:** in Go, Python (incl. uvloop paths) and Android, nothing (token or data) is sent before the pin is verified; no plaintext fallback; proxies don't bypass it.
- **Python DER walker** extracts the leaf's exact SubjectPublicKeyInfo; strict DER, no parser differential found.
- **Approval:** fresh nonces; signed message built from the verifier's own copies; key pin recomputed; changed keys need explicit acceptance; `approved` only after verification; exit codes correct; `approve.py` fails closed and runs commands without a shell.
- **Android approval key:** per-use `BIOMETRIC_STRONG` only, no PIN fallback, invalidated on enrollment change, StrongBox; approval components not exported; PendingIntents explicit and immutable.
- **Tokens:** 256-bit, constant-time compare, stored hashed on the phone, never printed on the PC.
- **Credential store:** 0600/0700, flock + atomic rename, unknown fields kept.
- **Android manifest:** backups off, tile and service protected; wake lock and sensor listeners released properly; 64 KB frame limit enforced.
- **SAS design:** commitment order correct (as in Bluetooth numeric comparison); domain-separated; bias negligible; no downgrade or reflection.

## Ideas: sensors and features not yet covered

**Phone → PC**
- Sensors: magnetometer/compass, rotation vector (true north), barometer (altitude/floor), step counter/detector, significant motion, pickup/flip gestures, ambient temperature/humidity where present.
- Phone state: screen on/off, locked/unlocked, Wi-Fi network, Bluetooth devices nearby/connected, charging source, ringer/DND mode, orientation.
- Events: incoming calls, mirrored notifications (needs notification access — sensitive), clipboard sync, share-to-PC from the share sheet (links, text, files), media play/pause/next.
- Input: touchpad mode (screen as trackpad), keyboard mode, volume buttons as clicker buttons, S Pen as presentation pointer.
- Location: GPS (sensitive).
- Camera/mic: phone as webcam, QR scanner for the PC, mic level or audio (heavy and sensitive).

**PC → phone**
- Vibrate, show a notification, ring ("find my phone"), flashlight, open a URL, send a file.

## Open decisions

1. **Tests and demos off GitHub:** keep locally and publish a filtered history (recommended), delete entirely, or move to a private repo? Do the conformance suites, mock phone and `plan.md` count as essential?
2. **Remove Claude from commits:** rewrite the 117 unpushed commits without the `Co-Authored-By` line (safe, nothing pushed).
3. **Order:** fix High and Medium findings (+ spec update) before cleaning history and pushing.
