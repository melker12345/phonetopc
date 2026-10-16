# phonetopc approval — fingerprint confirmation

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md) and [`SECURITY.md`](SECURITY.md).
The key words MUST, SHOULD and MAY are used as in RFC 2119.

A PC program asks the phone for an **approval**. The user confirms with a strong biometric (fingerprint) on the
phone, and the phone returns a **signature** from a hardware key that can only sign right after that biometric
check. The PC verifies the signature itself, so a "yes" can't be forged by anything that lacks that key: not the
network, not a stolen token. The phone's secure hardware proves to the PC that the key is there and needs a
fingerprint for every use (§3.1), so a modified app or an emulator can't pass off a software key.

## 1. Availability

- Approval exists only in **secure mode**. In dev mode the method doesn't exist (`-32601`).
- The phone lists capability `approval` in `hello.capabilities` when it can do approvals: the device has a strong
  (Class 3) biometric, at least one is enrolled, and the approval key exists or can be created.
- The `hello` result then includes `approval`:

```jsonc
"approval": {"key": "<base64url DER SubjectPublicKeyInfo>", "pin": "sha256/…",
             "attestation": ["<base64url DER certificate>", …]}
```

`pin` is computed over `key` exactly as in `SECURITY.md` §2. `attestation` is the key's Android Keystore
attestation certificate chain, leaf first (§3.1); a phone that has none omits it.

## 2. The approval key (phone)

- A separate ECDSA P-256 key in Android Keystore — never the TLS key — that requires user authentication **for
  every use** with `BIOMETRIC_STRONG` only (no device credential/PIN fallback), and is invalidated when biometric
  enrollment changes (`setInvalidatedByBiometricEnrollment(true)`). StrongBox SHOULD be used where available.
- Signing happens through `BiometricPrompt` with a `CryptoObject` wrapping the `Signature`, so the key is unusable
  without a fresh fingerprint.
- The key is generated with an attestation challenge (`setAttestationChallenge`): the SHA-256 of the phone's TLS
  SubjectPublicKeyInfo, which is the digest in its pin (`SECURITY.md` §2). That ties the attestation to the phone
  the PC paired with.
- If the key was invalidated (enrollment changed), the phone deletes it and creates a new one. The new key has a
  different `pin`; see §3 for what the PC does.

## 3. Trusting the approval key (PC)

- The `hello` result arrives over TLS that is pinned to the phone (`SECURITY.md` §3), so the PC can trust the
  `approval.pin` it sees there. The first time, if the key's attestation verifies (§3.1), the PC stores the pin as
  `approval_key` in `devices.json`, and the outcome (`tee` or `strongbox`) as `approval_attestation`.
- If a later `hello` shows a **different** `approval.pin` than the stored one, the PC MUST NOT accept approvals
  signed by the new key until the user accepts it explicitly on the PC (e.g. `phonetopc approval-key --accept`),
  and its attestation verifies. Typical cause: fingerprints were added or removed on the phone. Requests made
  meanwhile fail with a clear message instead of being sent.
- A key whose attestation is missing or fails is refused in the same way, with a message that names the failed
  check. The user MAY accept it anyway, but only explicitly (e.g. `phonetopc approval-key --accept --unattested`);
  `approval_attestation` is then `none`.
- A PC MAY let its user require StrongBox, and then refuses keys attested in the TEE.
- Re-pairing clears the stored `approval_key` and `approval_attestation`.

### 3.1 Key attestation

The PC checks `approval.attestation` once, when it first trusts a key, and stores the outcome. It accepts the
chain only if all of these hold:

1. **Chain.** Each certificate is issued (by name) and signed by the next. The last one is one of Google's
   hardware attestation roots, published at <https://android.googleapis.com/attestation/root>, or is issued and
   signed by one; the PC compares public keys, since a root is re-issued with the same key. Only the leaf has the
   key description extension (OID `1.3.6.1.4.1.11129.2.1.17`).
2. **Validity.** The certificates above the leaf are valid at the key's `creationDateTime` (now, if absent).
   A chain whose certificate just below the root has a serial number in its subject is factory-provisioned; its
   certificates stay trusted after they expire. The leaf's validity is not checked.
3. **Key.** The leaf's public key is `approval.key`.
4. **Challenge.** `attestationChallenge` is the SHA-256 of the SubjectPublicKeyInfo of the phone's TLS key that
   this connection is pinned to (§2).
5. **Secure hardware.** `attestationSecurityLevel` and `keyMintSecurityLevel` are both `TrustedEnvironment` (1)
   or `StrongBox` (2); the lower one is the key's level.
6. **Fingerprint for every use.** In `hardwareEnforced`, `userAuthType` has the fingerprint bit (2) and not the
   password bit (1). Neither list has `noAuthRequired` or `authTimeout`.
7. **Purpose.** `hardwareEnforced.purpose` includes sign (2).
8. **Verified boot.** `hardwareEnforced.rootOfTrust` has `verifiedBootState` `Verified` (0) and `deviceLocked`
   true.
9. **App.** `attestationApplicationId` names exactly one package, `io.github.melker12345.phonetopc`. A PC MAY
   also require one of the SHA-256 digests of the app's signing certificates; builds signed by others differ.

The key description's ASN.1 is at <https://source.android.com/docs/security/features/keystore/attestation>.
A PC MAY also check Google's revocation list, <https://android.googleapis.com/attestation/status>, and reject a
chain with a listed certificate; since that contacts the internet, it SHOULD do so only when its user asks.

[`conformance/vectors/attestation.json`](conformance/vectors/attestation.json) holds a chain from a test root
that a verifier given that root MUST accept, and variants it MUST reject.
## 4. `request_approval`

```jsonc
// PC → phone
{"jsonrpc":"2.0","id":7,"method":"request_approval","params":{
  "reason":"Run deploy.sh on arch-desktop?",
  "program":"deploy.sh",
  "nonce":"<32 random bytes, base64url>",
  "match":42,
  "expiry":1760000040000,
  "timeout_ms":30000}}
// phone → PC, after the user acted (or the timeout)
{"jsonrpc":"2.0","id":7,"result":{"approved":true,"signature":"<base64url DER ECDSA signature>"}}
{"jsonrpc":"2.0","id":7,"result":{"approved":false,"why":"denied"}}
```

| Param | Rules |
|---|---|
| `reason` | Required. 1–200 Unicode code points; must not start or end with whitespace; must not contain control characters (category Cc) or hidden characters (`PANELS.md` §8), so it can't be made to look like something else. Shown to the user verbatim |
| `program` | Required. The program on the PC that asks, typically its executable's file name, or `a program` when the PC can't tell. 1–60 code points, otherwise the rules of `reason`. Shown to the user verbatim |
| `nonce` | Required. Exactly 32 bytes after base64url decoding. Generated fresh by the PC for every request |
| `match` | Required. An integer 10–99, chosen uniformly at random by the PC for every request and shown to its user on the PC (§4.2) |
| `expiry` | Required. An integer: when the signature stops being valid, in Unix milliseconds. The PC sets it to its clock + `timeout_ms` + 10 s. The phone refuses one that is more than 5 minutes behind its own clock, or more than `timeout_ms` + 60 s + 5 minutes ahead |
| `timeout_ms` | Optional, default 30000, allowed 5000–120000 |

Invalid params: `-32602`. Integers are spelled as JSON integers (`42`, not `42.0` or `"42"`). The phone never
trims or normalizes `reason` or `program`: it rejects or accepts them exactly as received. Clients SHOULD
validate before sending.
Without capability `approval`: result `{"approved": false, "why": "unavailable"}`. A client that sees the
capability without `hello.approval` (or the reverse) MUST treat approval as unavailable and send nothing.

The 5 minutes of `expiry` allow for the phone's and the PC's clocks to differ. Both normally set themselves
from the network and differ by far less; 5 minutes is the usual tolerance of Kerberos, and keeps a signature
valid for at most about 8 minutes after it was made, even at the longest timeout. A phone or PC whose clock is
further off has to fix it before approvals work, and the phone's error message says so.

- If the client's own wait runs out, that's a client-side error (e.g. a timeout exception), not a `why`.
- The response is sent only once there's an outcome, which can take up to `timeout_ms` plus a little; clients MUST
  NOT time out the request earlier than `timeout_ms` + 5 s. Events and other responses continue meanwhile.
- **One pending approval per PC** (by paired PC id, across its sessions, counting `decrypt`, `DECRYPT.md` §4).
  Another request from the same PC gets `"why": "busy"` at once.
- The phone shows one request at a time. Those of other PCs wait their turn, first come first served, at most
  8 waiting; beyond that `busy`. `timeout_ms` runs while a request waits: one that times out waiting is
  answered `timeout` and never shown.
- If the session closes while an approval is pending, the phone cancels the prompt, or drops the request if it
  is still waiting.
- A PC in cool-down (§4.3) gets `"why": "cooldown"` at once, before the check for `busy`.

### 4.1 Outcomes

| `approved` | `why` | Meaning |
|---|---|---|
| `true` | — | Biometric succeeded and the phone signed; `signature` is present |
| `false` | `denied` | The user pressed Deny |
| `false` | `cancelled` | The user dismissed the prompt or notification |
| `false` | `mismatch` | The user picked a number other than `match` |
| `false` | `timeout` | `timeout_ms` passed without an answer |
| `false` | `busy` | This PC already had a request pending, or 8 other PCs' requests were waiting |
| `false` | `cooldown` | This PC's requests were refused too often; nothing was shown (§4.3) |
| `false` | `lockout` | Too many failed fingerprint attempts; Android locked biometrics for now |
| `false` | `unavailable` | No strong biometric enrolled, or no approval key |

Clients MUST treat `approved: true` as approved **only after** verifying the signature (§5). A `true` with a
missing or invalid signature is a failure and SHOULD be reported as such, never as approval.

### 4.2 What the user sees

Number matching makes the user check that the request on the phone is the one they started on the PC, so a
prompt that malware sends at a moment they expect one isn't approved by habit.

- **On the PC**, before or while sending the request, the program that asks shows `match` to its user ("pick 42
  on your phone"). A PC MUST NOT send a request whose number its user can't see.
- **On the phone**, a high-priority notification on its own channel, *Approval requests*: title "<program> on
  <PC name> asks for approval", text = `reason`, with actions *Approve* and *Deny*. *Approve* or tapping the
  notification opens the number choice; *Deny* answers `denied` without asking anything.
- The number choice shows `reason` and three numbers: `match` and two others, all different, drawn uniformly
  from 10–99, in random order, and a button *Deny*. The notification never shows them. Picking `match` opens
  the biometric prompt; picking another number answers `mismatch` without a prompt.
- The prompt shows "<program> on <PC name>" as title, `reason` as subtitle, and a negative button *Deny*.
- If the app is in the foreground, the number choice MAY open directly.
- When the request ends for any reason, the notification, number choice and prompt go away.

### 4.3 Cool-down

Per paired PC, the phone counts the requests its user refused: outcomes `denied`, `cancelled` and `mismatch`, of
approvals and decryptions together. At the third within 10 minutes, the PC is in cool-down for 5 minutes: every
`request_approval` and `decrypt` from it with valid params is answered `cooldown` at once, without showing
anything, and isn't counted. When a cool-down starts, the refusals before it are forgotten. Approvals and other
outcomes neither count nor reset the count. The phone MAY forget the count and the cool-down when the app
restarts.

## 5. Signed message and verification

The phone signs these bytes (UTF-8), lines joined by a single `\n`, no trailing newline:

```
phonetopc-approval/2
<phone id>
<pc id>
<nonce, base64url as sent>
<base64url(SHA-256(reason as UTF-8 bytes))>
<base64url(SHA-256(program as UTF-8 bytes))>
<match, decimal>
<expiry, decimal>
```

`reason` and `program` here are exactly the strings sent in the request, with no trimming or normalization on
either side; `match` and `expiry` are written in decimal without sign or leading zeros.

- `phone id` and `pc id` are the ids stored at pairing: the PC uses the phone id it stored and its own id, and
  the phone uses its own id and the PC id it stored. In this session `hello.device.id` and `hello.pc.id` MUST
  equal them; if either differs, the PC MUST NOT send `request_approval` in this session.
- Algorithm: ECDSA P-256 with SHA-256 (`SHA256withECDSA`); `signature` is the DER encoding, base64url.

The PC approves only if **all** of these hold:

1. The response belongs to a request it sent in this session, with the nonce it generated, and arrived before
   its own timeout.
2. `approval.pin` of this session is the pin of `approval.key` (the PC computes it), and equals the stored,
   accepted `approval_key`, whose attestation verified or which the user accepted without (§3).
3. The signature verifies with `approval.key` over the bytes above, built from **its own** copy of the nonce,
   reason, program, match, expiry, phone id and pc id — never from values echoed by the phone. A signature
   over any other message, such as one labelled `phonetopc-approval/1`, is invalid.

Anyone who checks a signature later, with the values it was made for, MUST also check that `expiry` hasn't
passed on their own clock.

[`conformance/vectors/approval.json`](conformance/vectors/approval.json) holds a worked example (test key,
message bytes, signature) that every verifier MUST accept, plus tampered variants it MUST reject.

## 6. Error codes

No new codes: problems with the request are `-32602`; user-level outcomes are results (§4.1), not errors.
`PROTOCOL.md` §7 lists every code.
