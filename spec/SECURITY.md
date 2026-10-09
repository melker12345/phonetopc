# phonetopc security — pairing, TLS and tokens (draft 3)

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md). It is milestone M2 of [`plan.md`](../plan.md).
The key words MUST, SHOULD and MAY are used as in RFC 2119.

**Changes in draft 3** (from the first three implementations and the first real pairing): default pairing port
8766; results of `pair_qr` and `pair_sas_confirm`; long-pending pairing answers; order of checks for pairing and
for secure `hello`; close codes; the listener survives refusals; re-pairing closes old sessions; exact QR encoding;
trying several addresses; locking and updating `devices.json`; the stored `approval_key`. Fingerprint approval
itself is in [`APPROVAL.md`](APPROVAL.md).

## 1. Goals and threat model

- **Attacker:** anyone on the same network, including one who can see, drop, change and inject traffic
  (café Wi-Fi, a compromised router). They may also glimpse the PC screen.
- **Must hold:** after pairing, an attacker can neither read the sensor stream, nor connect to the phone as a
  client, nor impersonate the phone to a PC. A phone only serves PCs the user explicitly paired, and the user can
  revoke any of them.
- **Out of scope:** a compromised PC user account or a rooted phone. Local malware running as the same PC user
  can read the stored token, as it could read an SSH key.
- **No custom cryptography:** only TLS, SHA-256, HMAC-SHA-256 and random numbers, which every language has.
  Implementations MUST use a cryptographically secure random generator for every secret and nonce.

## 2. Identities and keys

| Who | Key | Used for |
|---|---|---|
| Phone | Long-term TLS key pair, ECDSA P-256, generated on first run | The `wss://` server certificate |
| PC | Long-term TLS key pair, ECDSA P-256, generated on first `phonetopc pair` | The pairing listener's certificate (§4) |
| Phone → PC | One **token** per paired PC: 32 random bytes | The PC authenticates itself in `hello` |

- Certificates are self-signed X.509 with any subject and a long validity. **Nobody validates chains, expiry or
  host names**; trust comes only from the pin.
- **Pin** = `sha256/` + base64url, no padding, of SHA-256 over the certificate's DER-encoded
  `SubjectPublicKeyInfo`. Example: `sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg`. Pinning the public key
  rather than the certificate lets a side re-issue its certificate without re-pairing.
- **Device ids** (`device.id` from `hello`, and a PC id) are random, at least 64 bits, stable, and not secret.
- The phone SHOULD keep its private key in Android Keystore, hardware-backed where available. Tokens are
  stored only as received/issued; the phone MAY store a hash of each token instead of the token.

## 3. Transport modes

The phone server runs in exactly one mode at a time, on the same port (default 8765):

| Mode | URL | Auth | When |
|---|---|---|---|
| **secure** (default) | `wss://<phone>:8765/v1/ws` | Pinned TLS + token in `hello` | Normal use |
| **dev** | `ws://<phone>:8765/v1/ws` | None | Explicit, off-by-default developer setting, as in draft 1 |

- TLS 1.3 SHOULD be used; TLS 1.2 with ECDHE and an AEAD cipher MUST be accepted (Android 8–9 lack 1.3).
- A client connecting in secure mode MUST verify that the server's pin equals the stored pin and MUST NOT fall back
  to any other check or to plaintext. A pin mismatch is a hard failure that tells the user to re-pair.
- mDNS (`_phonetopc._tcp`) TXT records gain `tls=1` in secure mode; `dev=1` stays for dev mode.

## 4. Pairing

Pairing creates trust between one phone and one PC. It runs **once**. Afterwards the PC holds
`{phone id, phone pin, token}` and the phone holds `{PC id, PC name, token}`.

### 4.1 Roles and channel

- The PC runs `phonetopc pair`, which opens a short-lived **pairing listener**: a TLS WebSocket server, path
  `/v1/pair`, using the PC's long-term certificate. It closes after one successful pairing, on Ctrl-C, or after
  5 minutes (close code 1001 for an idle phone connection).
- Default port **8766**, so users can open it once in a firewall; implementations SHOULD allow choosing another
  port and SHOULD print a firewall hint if a host firewall is detected. If 8766 is busy, a random port MAY be used.
- The phone is the **client** of the pairing listener. Messages are JSON-RPC 2.0 like `PROTOCOL.md` §2, but with
  the methods below, and protocol name `phonetopc-pair/1`.
- The pairing listener accepts one pairing at a time; a second phone connecting meanwhile is closed with 1013.
  It SHOULD rate-limit failed attempts to 5 per minute per address; once over the limit, connections are closed
  with 1013, and after an error response that exceeds it, with 1008.
- After `-32020`, `-32021` or `-32023` the listener stays open for another attempt; all three count as failures.
- **Long answers:** the answers to `pair_qr` and `pair_sas_confirm` wait for the PC user. Phones MUST allow at
  least 5 minutes for them.
- **Order of checks** for each pairing request: (1)–(4) as in `PROTOCOL.md` §2; (5) unknown method → `-32601`;
  (6) method not valid at this step → `-32022`, without changing the state; (7) invalid params, including
  wrongly sized secrets, nonces or tokens → `-32602`; (8) unknown `protocol` → `-32004`; (9) method-specific errors.
- The PC offers **both** ways in at once; the user picks on the phone.

### 4.2 Way A — QR code (no discovery needed)

The PC prints a QR code (in the terminal; optionally as PNG) encoding a URI:

```
phonetopc://pair?v=1&id=<pc id>&n=<pc name, percent-encoded>&h=<host1>,<host2>&p=<port>&pin=<pc pin>&s=<secret>
```

- `h` lists every LAN address of the PC (IPv4 and IPv6, link-local excluded). `s` is 16 random bytes, base64url.
- **Encoding:** producers percent-encode `n` as UTF-8 per RFC 3986 (space is `%20`), write the commas in `h`
  literally, and write IPv6 addresses without brackets. Consumers decode each value before splitting `h` on
  commas, and MUST also accept `+` for space, `%2C` for comma and bracketed IPv6.
- Container and VM bridge interfaces (docker, veth, virbr, …) SHOULD be left out of `h`.
- The phone scans it, connects to `wss://<h>:<p>/v1/pair` trying each host in order, and **verifies the PC's pin**
  equals `pin`; a host with a different pin is skipped like an unreachable one. The QR is the trust anchor: the phone now knows it talks to that PC.
- The phone sends `pair_qr`; the secret proves to the PC that it's the phone that scanned the QR:

```jsonc
{"jsonrpc":"2.0","id":1,"method":"pair_qr","params":{
  "protocol":"phonetopc-pair/1","secret":"<s>",
  "phone":{"id":"635f3a01db1859bc","name":"Melker's S25 Ultra","pin":"sha256/…","port":8765,
           "addresses":["192.168.1.153"]}}}
```

- The PC compares `secret` in constant time. On mismatch: error `-32020` and the listener stays open.
- On match, the PC asks its user to confirm (`Pair with "Melker's S25 Ultra"? [Y/n]`), because a QR on screen
  might be photographed by someone else. Refusal: `-32021`.
- On confirmation the PC answers `{"pc": {"id", "name"}}`; the phone's last step is issuing the token, see §4.4.

### 4.3 Way B — network discovery and a 6-digit code

For when the user doesn't want to scan, and mDNS works.

- While pairing, the PC advertises `_phonetopc-pair._tcp` with TXT `id=<pc id>`, `n=<pc name>`, `v=1`.
- The phone's *Find on network* list shows these. The user taps one; the phone connects to `/v1/pair`
  **without** a pin (it can't know one yet) and records the pin it sees, `pc_pin_seen`.
- The two sides then run a **committed numeric comparison** (the scheme Bluetooth uses), so a man in the middle
  can't make the codes match except by a 1-in-a-million guess per attempt:

| Step | Message | Content |
|---|---|---|
| 1 | phone → `pair_sas_begin` | `protocol`, `phone` (as in §4.2) |
| 2 | ← result | `pc: {id, name}`, `commitment` = base64url(SHA-256(`Npc`)), where `Npc` is 32 random bytes |
| 3 | phone → `pair_sas_nonce` | `nonce` = `Nphone`, 32 random bytes, base64url |
| 4 | ← result | `nonce` = `Npc`. The phone MUST check SHA-256(`Npc`) equals `commitment`, else abort |

- Both sides compute the code:

```
code = decimal( uint32_be( SHA-256( "phonetopc-sas/1" ‖ pc_pin ‖ phone_pin ‖ Npc ‖ Nphone )[0..4] ) mod 1 000 000 ), zero-padded to 6 digits
```

where the pins are their ASCII strings (`sha256/…`), `Npc` and `Nphone` are the raw 32 bytes (not their base64url
text), and `‖` is plain concatenation with no separators. The phone uses `pc_pin_seen`, the PC uses its own pin.
[`conformance/vectors/vectors.json`](conformance/vectors/vectors.json) has worked examples for the pin, the
commitment and the code; every implementation MUST test against them.
A man in the middle presents a different PC pin to the phone than the PC's real one, so the codes differ.

- Both screens show the code. The user confirms on **both**: the phone sends `pair_sas_confirm`
  `{"match": true|false}`; the PC asks `Does the phone show 123 456? [y/N]` and answers `{}` once its user said
  yes. If either says no: error `-32023`, and both sides discard everything.
- If the phone's commitment check fails, it sends `pair_sas_confirm {"match": false}` and then aborts.
- If both confirm, the phone stores `pc_pin_seen` as the PC's pin and continues with §4.4.

### 4.4 Finishing: the token

Both ways end with the phone issuing a token to the PC:

```jsonc
// phone → PC
{"jsonrpc":"2.0","id":9,"method":"pair_finish","params":{"token":"<32 random bytes, base64url>"}}
// PC → phone
{"jsonrpc":"2.0","id":9,"result":{"pc":{"id":"9d1c…","name":"arch-desktop"}}}
```

- The PC stores the pairing (§6) and only then answers. The phone stores the PC (id, name, pin, token)
  only after that answer, and shows it under *Paired devices*.
- `pair_finish` is only valid after a successful `pair_qr` confirmation or a mutual SAS confirmation;
  otherwise `-32022`.
- Re-pairing a PC that the phone already knows (same PC id) replaces its old token, and the phone closes sessions
  that used the old token with 1008.

### 4.5 Pairing errors

| Code | Name | When |
|---|---|---|
| `-32020` | `bad_secret` | `pair_qr` secret doesn't match |
| `-32021` | `rejected` | The PC user declined |
| `-32022` | `out_of_order` | A pairing method sent at the wrong step |
| `-32023` | `sas_mismatch` | A user said the codes differ, or the commitment check failed |
| `-32024` | `expired` | The pairing listener timed out |
| `-32004` | `unsupported_protocol` | Unknown `protocol` value, `data.supported` lists the known ones |

## 5. Authenticated sessions

In secure mode, `hello` gains an `auth` member:

```jsonc
{"jsonrpc":"2.0","id":1,"method":"hello","params":{
  "protocol":"phonetopc/1","client":{"name":"my-script"},
  "auth":{"token":"<token>"}}}
```

- The server compares the token in constant time against the tokens of paired PCs.
- **Order of checks** for a secure `hello`: params shape (`-32602`), then `auth` (any problem, including a
  malformed `auth`, is `-32010`), then `protocol` (`-32004`). Checking auth before the protocol version means an
  unauthenticated client learns nothing.
- Missing, unknown or revoked token: error `-32010` (`unauthorized`), then the server closes the connection
  with code 1008. Before a successful `hello`, the `-32003` rule of `PROTOCOL.md` §3 still applies.
- The server SHOULD close connections that haven't sent a successful `hello` within 10 seconds (code 1008), and
  SHOULD rate-limit failed `hello`s to 5 per minute per source address; a limited address is closed with 1013,
  even with a valid token. Conformance runners need the limit off or at least 20 per minute.
- A failed `hello` after a successful one also closes the connection with 1008.
- The `hello` result gains `pc: {"id", "name"}`: which pairing the server recognised.
- Revoking a PC on the phone closes its open sessions immediately with code 1008.
- In dev mode, `auth` is ignored.

## 6. Credential store on the PC

So that any language can use a pairing made by `phonetopc pair`, the PC stores pairings in a documented file:

- Path: `$XDG_CONFIG_HOME/phonetopc/devices.json` (default `~/.config/phonetopc/devices.json`), mode `0600`,
  directory `0700`. The PC's own key lives next to it as `pc-key.pem` (`0600`) with `pc.json` holding its
  `id` and `name`.
- Content:

```json
{
  "version": 1,
  "devices": [
    {
      "id": "635f3a01db1859bc",
      "name": "Melker's S25 Ultra",
      "pin": "sha256/n4bQgYhMfWWaL-qgxVrQFaO_TxsrC4Is0V1sFbDwCgg",
      "token": "Zm9vYmFy…",
      "port": 8765,
      "addresses": ["192.168.1.153"],
      "paired_at": "2026-10-09T16:40:00Z",
      "approval_key": "sha256/…"
    }
  ]
}
```

- `id`, `name`, `pin` and `token` are required. `port` defaults to 8765, `addresses` to `[]`; `paired_at` and
  `approval_key` (see `APPROVAL.md` §3) are optional. Readers MUST keep unknown fields when rewriting.
- **Writing:** take an exclusive `flock` on the file `.lock` in the same directory, re-read `devices.json`, change
  it, write a temp file in the same directory with mode `0600`, rename it over `devices.json`, release the lock.
  On platforms without `flock`, skip the lock.
- **Finding a paired phone:** browse mDNS for `_phonetopc._tcp` with TXT `id` equal to the device id; if that
  fails, try `addresses` in order. Every attempt verifies the pin; a pin mismatch at one address does not stop the
  others, and is reported only if no address works. Once connected, move the working address to the front of
  `addresses` (never storing loopback addresses, e.g. from `adb forward`), and update `port` only from mDNS.
- If `hello.device.id` differs from the stored id, clients SHOULD warn; the pin already proved the key.
- In milestone M3, `phonetopcd` becomes the main user of this file, and scripts talk to the daemon instead.

## 7. Error codes added to PROTOCOL.md §7

| Code | Name | When |
|---|---|---|
| `-32010` | `unauthorized` | Secure mode `hello` without a valid token |

## 8. What this doesn't solve yet

- A phone with many PCs, or a PC with many phones, works, but there's no UI for choosing a default yet beyond
  "the only one" or `--device`.
