# phonetopc approval — fingerprint confirmation (draft 4)

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md) and [`SECURITY.md`](SECURITY.md).
The key words MUST, SHOULD and MAY are used as in RFC 2119.

Draft numbers are shared by the whole spec (see `PROTOCOL.md`). This document was added in draft 3 and is
unchanged in draft 4.

A PC program asks the phone for an **approval**. The user confirms with a strong biometric (fingerprint) on the
phone, and the phone returns a **signature** from a hardware key that can only sign right after that biometric
check. The PC verifies the signature itself, so a "yes" can't be forged by anything that lacks that key: not the
network, not a stolen token, not a bug elsewhere in the phone app.

## 1. Availability

- Approval exists only in **secure mode**. In dev mode the method doesn't exist (`-32601`).
- The phone lists capability `approval` in `hello.capabilities` when it can do approvals: the device has a strong
  (Class 3) biometric, at least one is enrolled, and the approval key exists or can be created.
- The `hello` result then includes `approval`:

```jsonc
"approval": {"key": "<base64url DER SubjectPublicKeyInfo>", "pin": "sha256/…"}
```

`pin` is computed over `key` exactly as in `SECURITY.md` §2.

## 2. The approval key (phone)

- A separate ECDSA P-256 key in Android Keystore — never the TLS key — that requires user authentication **for
  every use** with `BIOMETRIC_STRONG` only (no device credential/PIN fallback), and is invalidated when biometric
  enrollment changes (`setInvalidatedByBiometricEnrollment(true)`). StrongBox SHOULD be used where available.
- Signing happens through `BiometricPrompt` with a `CryptoObject` wrapping the `Signature`, so the key is unusable
  without a fresh fingerprint.
- If the key was invalidated (enrollment changed), the phone deletes it and creates a new one. The new key has a
  different `pin`; see §3 for what the PC does.

## 3. Trusting the approval key (PC)

- The `hello` result arrives over TLS that is pinned to the phone (`SECURITY.md` §3), so the PC can trust the
  `approval.pin` it sees there. The first time, the PC stores it as `approval_key` in `devices.json`.
- If a later `hello` shows a **different** `approval.pin` than the stored one, the PC MUST NOT accept approvals
  signed by the new key until the user accepts it explicitly on the PC (e.g. `phonetopc approval-key --accept`).
  Typical cause: fingerprints were added or removed on the phone. Requests made meanwhile fail with a clear
  message instead of being sent.
- Re-pairing clears the stored `approval_key`.

## 4. `request_approval`

```jsonc
// PC → phone
{"jsonrpc":"2.0","id":7,"method":"request_approval","params":{
  "reason":"Run deploy.sh on arch-desktop?",
  "nonce":"<32 random bytes, base64url>",
  "timeout_ms":30000}}
// phone → PC, after the user acted (or the timeout)
{"jsonrpc":"2.0","id":7,"result":{"approved":true,"signature":"<base64url DER ECDSA signature>"}}
{"jsonrpc":"2.0","id":7,"result":{"approved":false,"why":"denied"}}
```

| Param | Rules |
|---|---|
| `reason` | Required. 1–200 Unicode code points; must not start or end with whitespace; must not contain control characters (category Cc) or bidirectional controls (U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069), so it can't be made to look like something else. Shown to the user verbatim |
| `nonce` | Required. Exactly 32 bytes after base64url decoding. Generated fresh by the PC for every request |
| `timeout_ms` | Optional, default 30000, allowed 5000–120000 |

Invalid params: `-32602`. The phone never trims or normalizes `reason`: it rejects or accepts it exactly as
received. Clients SHOULD validate before sending.
Without capability `approval`: result `{"approved": false, "why": "unavailable"}`. A client that sees the
capability without `hello.approval` (or the reverse) MUST treat approval as unavailable and send nothing.

- If the client's own wait runs out, that's a client-side error (e.g. a timeout exception), not a `why`.
- The response is sent only once there's an outcome, which can take up to `timeout_ms` plus a little; clients MUST
  NOT time out the request earlier than `timeout_ms` + 5 s. Events and other responses continue meanwhile.
- **One approval at a time per phone.** While one is pending, others get `"why": "busy"`.
- If the session closes while an approval is pending, the phone cancels the prompt.

### 4.1 Outcomes

| `approved` | `why` | Meaning |
|---|---|---|
| `true` | — | Biometric succeeded and the phone signed; `signature` is present |
| `false` | `denied` | The user pressed Deny |
| `false` | `cancelled` | The user dismissed the prompt or notification |
| `false` | `timeout` | `timeout_ms` passed without an answer |
| `false` | `busy` | Another approval was pending |
| `false` | `lockout` | Too many failed fingerprint attempts; Android locked biometrics for now |
| `false` | `unavailable` | No strong biometric enrolled, or no approval key |

Clients MUST treat `approved: true` as approved **only after** verifying the signature (§5). A `true` with a
missing or invalid signature is a failure and SHOULD be reported as such, never as approval.

### 4.2 What the user sees (phone)

- A high-priority notification on its own channel, *Approval requests*: title "<PC name> asks for approval",
  text = `reason`, with actions *Approve* and *Deny*. *Approve* or tapping the notification opens the biometric
  prompt; *Deny* answers `denied` without a prompt.
- The prompt shows the PC name as title, `reason` as subtitle, and a negative button *Deny*.
- If the app is in the foreground, the prompt MAY open directly.
- When the request ends for any reason, the notification and prompt go away.

## 5. Signed message and verification

The phone signs these bytes (UTF-8), lines joined by a single `\n`, no trailing newline:

```
phonetopc-approval/1
<phone id>
<pc id>
<nonce, base64url as sent>
<base64url(SHA-256(reason as UTF-8 bytes))>
```

`reason` here is exactly the string sent in the request, with no trimming or normalization on either side.

- `phone id` is `hello.device.id`, `pc id` is `hello.pc.id` of this session.
- Algorithm: ECDSA P-256 with SHA-256 (`SHA256withECDSA`); `signature` is the DER encoding, base64url.

The PC approves only if **all** of these hold:

1. The response belongs to a request it sent in this session, with the nonce it generated, and arrived before
   its own timeout.
2. `approval.pin` of this session is the pin of `approval.key` (the PC computes it), and equals the stored,
   accepted `approval_key` (§3).
3. The signature verifies with `approval.key` over the bytes above, built from **its own** copy of the nonce,
   reason, phone id and pc id — never from values echoed by the phone.

[`conformance/vectors/approval.json`](conformance/vectors/approval.json) holds a worked example (test key,
message bytes, signature) that every verifier MUST accept, plus tampered variants it MUST reject.

## 6. Error codes

No new codes: problems with the request are `-32602`; user-level outcomes are results (§4.1), not errors.
`PROTOCOL.md` §7 lists every code.
