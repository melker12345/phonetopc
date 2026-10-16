# phonetopc decryption — data that needs a fingerprint to read

This document is normative and extends [`PROTOCOL.md`](PROTOCOL.md), [`SECURITY.md`](SECURITY.md) and
[`APPROVAL.md`](APPROVAL.md). The key words MUST, SHOULD and MAY are used as in RFC 2119.

The phone holds a **decryption key** in secure hardware that works only right after a strong biometric check. Anyone
can encrypt to its public key, without the phone. Decrypting needs the phone and a fingerprint each time: the PC sends
the ciphertext, the user confirms, and the phone returns the plaintext. Typically the ciphertext is a small key that
encrypts a file (§5), so the file can't be read with anything stored on the PC.

## 1. Availability

- Decryption exists only in **secure mode**. In dev mode the method doesn't exist (`-32601`).
- The phone lists capability `decrypt` in `hello.capabilities` when it can decrypt: the device has a strong
  (Class 3) biometric, at least one is enrolled, and the decryption key exists or can be created.
- The `hello` result then includes `decrypt`:

```jsonc
"decrypt": {"key": "<base64url DER SubjectPublicKeyInfo>", "pin": "sha256/…",
            "attestation": ["<base64url DER certificate>", …]}
```

`pin` is computed over `key` exactly as in `SECURITY.md` §2, and `attestation` is as in `APPROVAL.md` §1.

## 2. The decryption key (phone)

- An RSA key in Android Keystore, separate from the TLS and approval keys: 2048 bits in StrongBox where available,
  otherwise 3072 bits. Purpose decrypt only, padding OAEP with digest SHA-256 (MGF1 with SHA-1, the Keystore
  default).
- It requires user authentication **for every use** with `BIOMETRIC_STRONG` only, is invalidated when biometric
  enrollment changes, and is attested with the same challenge, exactly like the approval key (`APPROVAL.md` §2).
  Decryption happens through `BiometricPrompt` with a `CryptoObject` wrapping the `Cipher`.
- If the key was invalidated, the phone deletes it and creates a new one with a different `pin`. **Everything
  encrypted to the old key can no longer be decrypted by the phone**; §6 is how users keep access.

## 3. Encrypting to the phone

RSAES-OAEP (RFC 8017) with hash SHA-256, MGF1 with SHA-1 and an empty label, to `hello.decrypt.key`. The plaintext is
at most 190 bytes for a 2048-bit key, and SHOULD be a random key that encrypts the actual data.

A PC learns `key` from a `hello`, over TLS pinned to the phone (`SECURITY.md` §3). It doesn't need the phone again
to encrypt.

Data encrypted to the key is only as safe as the key, so before a PC encrypts to it or derives a recipient from it
(§5), it MUST verify `decrypt.attestation` as in `APPROVAL.md` §3.1, with purpose decrypt (1) in place of sign, or
have its user accept an unattested key explicitly (e.g. `age-plugin-phonetopc --generate --unattested`). It MAY
store the outcome as `decrypt_key` and `decrypt_attestation` in `devices.json`, so that each key is checked once.
Decrypting doesn't need the attestation.

## 4. `decrypt`

```jsonc
// PC → phone
{"jsonrpc":"2.0","id":9,"method":"decrypt","params":{
  "reason":"Decrypt secrets.age",
  "key_pin":"sha256/…",
  "ciphertext":"<base64url>",
  "timeout_ms":30000}}
// phone → PC, after the user acted (or the timeout)
{"jsonrpc":"2.0","id":9,"result":{"decrypted":true,"plaintext":"<base64url>"}}
{"jsonrpc":"2.0","id":9,"result":{"decrypted":false,"why":"denied"}}
```

| Param | Rules |
|---|---|
| `reason` | Required. Same rules as `request_approval`'s `reason` (`APPROVAL.md` §4). Shown to the user verbatim |
| `key_pin` | Required. The pin of the key the ciphertext was made for |
| `ciphertext` | Required. base64url; after decoding, exactly as long as the key's modulus (256 or 384 bytes) |
| `timeout_ms` | Optional, default 30000, allowed 5000–120000 |

Invalid params: `-32602`. Without capability `decrypt`, or when `key_pin` isn't the pin of the phone's current key:
result `{"decrypted": false, "why": "unavailable"}`, without asking the user.

`why` is one of the outcomes of `APPROVAL.md` §4.1 (`denied`, `cancelled`, `timeout`, `busy`, `lockout`,
`unavailable`), or `failed`: the user confirmed, but the ciphertext doesn't decrypt with this key (damaged, or made
for another key).

Timing, waiting, queueing and cancellation are as in `APPROVAL.md` §4. Approvals and decryptions count together: a PC
with either pending gets `busy` for both, and they wait in the same queue.

What the user sees is as in `APPROVAL.md` §4.2, with the notification titled "<PC name> asks to decrypt".

The PC can't tell a correct plaintext from a wrong one by itself. Formats built on this MUST authenticate what they
decrypt with the plaintext, as age does with its header MAC (§5).

## 5. age

Files are encrypted with [age](https://age-encryption.org) through the plugin `age-plugin-phonetopc`.

- **Recipient:** `age1phonetopc1…`, the Bech32 encoding of the DER SubjectPublicKeyInfo of `hello.decrypt.key`.
- **Identity:** `AGE-PLUGIN-PHONETOPC-1…`, the Bech32 encoding of one byte *n*, the *n* bytes of the phone's device
  id (UTF-8), then the same SubjectPublicKeyInfo. It holds no secret; it says which phone to ask and for which key.
  It also works as a recipient.
- **Stanza:** `-> phonetopc <tag>`, where `tag` is the unpadded standard base64 of the first 4 bytes of SHA-256 of the
  SubjectPublicKeyInfo, and the body is the age file key encrypted as in §3.

To decrypt, the plugin connects to the phone named in the identity as a paired PC and sends `decrypt` for each stanza
whose tag matches. A plaintext that isn't 16 bytes is an error.

## 6. Recovery

Losing or resetting the phone, or adding or removing a fingerprint, destroys the key. Encrypt important data to a
second recipient as well, for example an age key kept offline, so that losing the phone key costs convenience, not
data.
