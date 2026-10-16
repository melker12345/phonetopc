# phonetopc security — pairing, TLS and tokens

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md).
The key words MUST, SHOULD and MAY are used as in RFC 2119.

Fingerprint approval is in [`APPROVAL.md`](APPROVAL.md).

## 1. Goals and threat model

The full threat model, attacker by attacker, is in [`THREAT-MODEL.md`](THREAT-MODEL.md).

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
  A phone id is 16 to 64 lowercase hex digits (`^[0-9a-f]{16,64}$`); a PC refuses any other `phone.id` in
  pairing with `-32602`. PC ids SHOULD have the same form.
- **Names** (`phone.name`, `pc.name`, the `n` of a pairing code, `device.name`) are display text without
  newlines (`PANELS.md` §8), at most 100 code points. A PC refuses any other `phone.name` in pairing with
  `-32602`. A name that breaks these rules elsewhere is treated as missing: the id is shown instead. Names are
  only labels; trust comes only from pins.
- The phone SHOULD keep its private key in Android Keystore, hardware-backed where available. Tokens are
  stored only as received/issued; the phone MAY store a hash of each token instead of the token.

## 3. Transport modes

The phone server runs in exactly one mode at a time, on the same port (default 8765):

| Mode | URL | Auth | When |
|---|---|---|---|
| **secure** (default) | `wss://<phone>:8765/v1/ws` | Pinned TLS + token in `hello` | Normal use |
| **dev** | `ws://<phone>:8765/v1/ws` | None | Explicit, off-by-default developer setting |

- TLS 1.3 SHOULD be used; TLS 1.2 with ECDHE and an AEAD cipher MUST be accepted (Android 8–9 lack 1.3).
- A client connecting in secure mode MUST verify that the server's pin equals the stored pin and MUST NOT fall back
  to any other check or to plaintext. A pin mismatch is a hard failure: the client warns that something may be
  impersonating the phone, and suggests re-pairing only if the phone app was reset or reinstalled.
- Clients MUST use dev mode only when told to by an option named for it. Never because of a `ws://` address, an
  mDNS `dev=1`, or a failed secure connection.
- The phone MUST show that it is in dev mode for as long as it is, including while the app is in the background.
- mDNS (`_phonetopc._tcp`) TXT records gain `tls=1` in secure mode; `dev=1` stays for dev mode.

## 4. Pairing

Pairing creates trust between one phone and one PC. It runs **once**. Afterwards the PC holds
`{phone id, phone pin, token}` and the phone holds `{PC id, PC name, PC pin, token}`.

### 4.1 Roles and channel

- The PC runs `phonetopc pair`, which opens a short-lived **pairing listener**: a TLS WebSocket server, path
  `/v1/pair`, using the PC's long-term certificate. It closes after one successful pairing, on Ctrl-C, or after
  5 minutes (close code 1001 for an idle phone connection).
- Default port **8766**, so users can open it once in a firewall; implementations SHOULD allow choosing another
  port and SHOULD print a firewall hint if a host firewall is detected. If 8766 is busy, a random port MAY be used.
- The phone is the **client** of the pairing listener. Messages are JSON-RPC 2.0 like `PROTOCOL.md` §2, but with
  the methods below, and protocol name `phonetopc-pair/1`.
- The pairing listener accepts one pairing at a time; a second phone connecting meanwhile is closed with 1013.
  A failed attempt frees the slot before it is answered, so the phone can start over on a new connection; a
  connection that starts over while another phone holds the slot is closed with 1013 too.
  So that nobody can hold that slot, the listener SHOULD close a connection with 1001 that hasn't taken its next
  step within 20 seconds of connecting or of its last step. Only while the users compare codes (after
  `pair_sas_nonce` was answered) does it wait as long as the users get (5 minutes). A step is a request that
  advances the pairing or reaches a user who says no; malformed, out-of-order and wrong-secret requests, and a
  `pair_sas_begin` refused with `-32021` because the PC doesn't offer Way B, are not steps. Phones send each
  request right after the previous answer.
  It SHOULD rate-limit failed attempts to 5 per minute per address; once over the limit, connections are closed
  with 1013, and after an error response that exceeds it, with 1008.
- After `-32020`, `-32021` or `-32023` the listener stays open for another attempt; all three count as failures.
  Way B has a stricter limit across all addresses as well (§4.3).
- **One connection per pairing:** all steps of a pairing, from `pair_qr` or `pair_sas_begin` to `pair_finish`,
  happen on one connection. When that connection closes, the listener discards the pairing's state; a phone that
  reconnects starts over. An abandoned code comparison counts as failed (§4.3).
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
- On match, the PC asks its user to confirm (`Pair with "Melker's S25 Ultra"? [y/N]`), because a QR on screen
  might be photographed by someone else. Refusal: `-32021`.
- **One prompt per secret:** once a `pair_qr` with the right secret reached the PC user's prompt and did not end
  in a pairing (refused, or the phone disconnected or timed out), the PC MUST replace the secret with a fresh one
  and show a new QR code; the old secret then gets `-32020`. Otherwise someone who photographed the QR code could
  retry, under the name of the user's phone, until the user gives in. Phones SHOULD tell the user to scan again
  after `-32020`. The PC SHOULD tell its user when a second phone connects while a pairing is pending.
- **Prompts** (here and in §4.3) need an explicit yes; an empty answer is no. A PC reading answers from a
  terminal MUST ignore input typed before the question was shown, so a key pressed while waiting can't answer it.
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
- **Limit on runs:** every run in which the PC reveals `Npc` gives a man in the middle a fresh 1-in-a-million
  chance, and per-address limits don't stop one (an IPv6 /64 is a lot of addresses). A run **fails** if the PC
  answered `pair_sas_nonce` and the run did not end in `pair_finish`: a mismatch, a refusal, a timeout or a
  disconnect all count. After 3 failed runs, from any addresses, the listener MUST NOT reveal a new `Npc`, and
  SHOULD close and tell its user that someone may be trying to pair in the phone's place. The user can start
  again; the QR code (Way A) doesn't have this weakness.
- If both confirm, the phone stores `pc_pin_seen` as the PC's pin and continues with §4.4.
- The PC's confirmation MUST come from a person comparing the codes. A PC that pairs without asking (e.g.
  `phonetopc pair --yes`) MUST refuse `pair_sas_begin` with `-32021` and SHOULD NOT advertise
  `_phonetopc-pair._tcp`.

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
- Re-pairing a PC that the phone already knows (same PC id and pin) replaces its old token, and the phone closes
  sessions that used the old token with 1008. A PC that uses a known PC id with a **different** pin would take
  that PC's place, including the PC id approvals are signed for: the phone MUST NOT send `pair_finish` to it
  and MUST tell the user, who can remove the old PC first.
- On the PC, a pairing whose `phone.id` is already paired with a **different** pin would replace that phone's
  entry, and with it the key the PC trusts (including for approvals). The PC MUST warn its user and get an
  explicit yes for that (an empty answer is no); a PC that pairs without asking MUST refuse it with `-32021`.
  If the stored entry changed between the question and the save, the PC MUST NOT save (`-32603`).

### 4.5 Pairing errors

These codes occur only on the pairing connection. `PROTOCOL.md` §7 lists them with every other code.

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
  even with a valid token. Loopback MAY be exempt: `adb forward` clients all come from 127.0.0.1, so one bad
  client would lock out the rest, and tokens are too long to guess anyway. Conformance runners need the limit off
  or at least 20 per minute.
- The TLS handshake and the WebSocket upgrade together MUST finish within the `hello` timeout (10 s by default),
  however slowly the bytes arrive. A server SHOULD limit how many unauthenticated connections one address can
  hold open (Android: 8); loopback is exempt, because `adb forward` clients all come from 127.0.0.1, but a
  total cap still applies.
- A failed `hello` after a successful one also closes the connection with 1008.
- The `hello` result gains `pc: {"id", "name"}`: which pairing the server recognised.
- Revoking a PC on the phone closes its open sessions immediately with code 1008.
- In dev mode, `auth` is ignored. The `hello` timeout and the limits on connections still apply.

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
      "approval_key": "sha256/…",
      "approval_attestation": "strongbox"
    }
  ]
}
```

- `id`, `name`, `pin` and `token` are required. `port` defaults to 8765, `addresses` to `[]`. `paired_at`,
  `approval_key` and `approval_attestation` (`tee`, `strongbox` or `none`; see `APPROVAL.md` §3), and
  `decrypt_key` and `decrypt_attestation` (`DECRYPT.md` §3) are optional. Readers MUST keep unknown fields when
  rewriting.
- **Writing:** take an exclusive `flock` on the file `.lock` in the same directory, re-read `devices.json`, change
  it, write a temp file in the same directory with mode `0600`, rename it over `devices.json`, release the lock.
  On platforms without `flock`, skip the lock.
- **Finding a paired phone:** browse mDNS for `_phonetopc._tcp` with TXT `id` equal to the device id; if that
  fails, try `addresses` in order. Every attempt verifies the pin; a pin mismatch at one address does not stop the
  others, and is reported only if no address works. Once connected, move the working address to the front of
  `addresses` (never storing loopback addresses, e.g. from `adb forward`), and update `port` only from mDNS.
- If `hello.device.id` differs from the stored id, clients SHOULD warn; the pin already proved the key.

## 7. Error codes

Secure sessions add `-32010` (`unauthorized`): a secure mode `hello` without a valid token (§5). Pairing adds the
codes of §4.5. `PROTOCOL.md` §7 is the complete list.

## 8. Who uses the store

- `phonetopcd` reads `devices.json`, keeps the connection to the phone, and offers it to other programs on the PC
  as plain HTTP ([`LOCAL-API.md`](LOCAL-API.md)). Programs normally use that and never touch the store.
- A program that speaks `PROTOCOL.md` itself (for example through the Go client library, `pcd/client`) MAY read
  the store and connect directly, following §6.
- A phone with many PCs, or a PC with many phones, works; a PC with several phones picks one with `--device`.
