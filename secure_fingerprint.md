# Making fingerprint approval as secure as possible

Notes from 2026-10-15: how fingerprint approval works today, where it is weak, and what to build to make it as
strong as it can be. Specification: [`spec/APPROVAL.md`](spec/APPROVAL.md).

## How it works today

Approval is not a "did the fingerprint return OK" flag. The phone proves the answer with a signature:

1. When the phone pairs, it creates an ECDSA P-256 **approval key in Android Keystore** (secure hardware: the TEE,
   or StrongBox where available). The private key never leaves that hardware
   (`android/app/.../data/ApprovalKey.kt`).
2. The key is locked to the fingerprint:
   - `setUserAuthenticationRequired(true)`
   - `AUTH_BIOMETRIC_STRONG` only, so no PIN or pattern fallback
   - one fingerprint per signature: validity `0`/`-1`, used through a `CryptoObject`
   - `setInvalidatedByBiometricEnrollment(true)`: adding or removing a fingerprint destroys the key
3. The PC learns the public key in `hello`, over TLS pinned to the phone, and stores it in `devices.json`. If it
   changes later, the PC refuses approvals until the user accepts the new key (`phonetopc approval-key --accept`).
4. For an approval, the PC sends a fresh 32-byte random nonce and the question. After the fingerprint, the phone
   signs:
   ```
   phonetopc-approval/1
   <phone id>
   <pc id>
   <nonce>
   <base64url(SHA-256(reason))>
   ```
5. `phonetopcd` verifies the signature against the stored key before it answers `{"approved": true}`.

The answer can't be forged by the network, a fake phone or a stolen token, and an old one can't be replayed. On the
phone this is the same mechanism as unlocking it with a fingerprint.

## Where it is weak

- **The last hop on the PC.** The daemon verifies the signature, then tells the program `{"approved": true}` over
  localhost, unsigned. Malware running as the same user can stop the daemon and answer in its place, or patch the
  program. No design fully fixes this: a program running as you can always lie to another one running as you.
- **The PC trusts the app's word about the key.** Nothing proves to the PC that the approval key really is in secure
  hardware and locked to the fingerprint. A modified app or an emulator could use a software key.
- **Approval fatigue.** Malware can trigger a prompt at a moment when you expect one, and you approve it (the
  "MFA push bombing" attack).
- **Approval is a verdict, not a secret.** It can gate an action, but it can't protect data: a key stored on the PC
  is readable without the phone.
- **Open audit findings.** A strong fingerprint check doesn't help if the attacker can get around it.
  [`AUDIT.md`](AUDIT.md) (2026-10-09) lists 3 high and 5 medium findings:
  - H1 (`pair --yes`) is fixed (`a43e7ba`).
  - H2 (grinding the 6-digit code) has a limit now (`ErrSASLimit`); check it against the finding.
  - The rest haven't been re-checked. `AUDIT.md` still says "nothing fixed".

## What to build

### 1. Re-audit and fix

Mark each `AUDIT.md` finding fixed or open, and fix what's open, starting with H3 (pre-auth memory exhaustion on
the phone). Add CI that runs the tests with `-race` on every push. This is the cheapest and biggest win.

### 2. Return the signature from `POST /v1/approvals`

Add `nonce`, `reason`, `signature` and the key's `pin` to the answer, so a program can verify the phone itself
instead of trusting the daemon's `approved: true`. Small change; document it in `spec/LOCAL-API.md`.

### 3. Key attestation and StrongBox

- **Attestation:** at pairing, the phone sends the approval key's Android attestation certificate chain. The PC
  checks it once:
  - the chain ends at Google's attestation root;
  - the key is hardware-backed (TEE or StrongBox);
  - it needs a strong biometric for every use;
  - it belongs to this app's package and signing certificate;
  - the device's boot state is verified.

  This turns "the app says so" into "the hardware proves it". It's the biggest single upgrade.
- **StrongBox:** use it where available (the S25 has it), report which one holds the key, and let the user require
  StrongBox. APPROVAL.md only says SHOULD today.

### 4. Number matching and rate limits

- The PC shows a number, and the user picks the same one on the phone before the fingerprint, as Microsoft and Google
  do against push bombing.
- A cool-down after denials.
- Show which program is asking.

### 5. Sign more context

Add the requesting program's name and an expiry time to the signed message (`phonetopc-approval/2`). Then a
signature can't be presented as another program's or used later.

### 6. Run the daemon as its own system user

Run `phonetopcd` as a dedicated system user, with its socket in a root-owned `/run/phonetopc/` and its keys in its
own directory. Malware running as you then can't stop and impersonate it, or rewrite its stored keys, the same idea
that protects `sshd` and `polkit`. It can still patch your own program; only the next step protects against that.

### 7. Key unwrapping: protect data, not just actions

Use this to encrypt files that can only be read after a fingerprint on the phone:

- **Encrypt:** generate a random file key, encrypt the file with it, and wrap the file key to a second Keystore
  key on the phone. That key also needs a strong biometric for every use (Keystore supports ECDH key agreement from
  Android 12, or use an AES key). The PC stores only the wrapped key.
- **Decrypt:** a new phone method, `unwrap`, takes the wrapped key, asks for the fingerprint, unwraps it in the
  secure hardware and returns the file key over the pinned connection.
- **Recovery:** also wrap the file key with a long passphrase kept offline. Losing or wiping the phone, or adding a
  fingerprint (which destroys the key), then costs convenience, not data.
- **Format:** plug into [age](https://age-encryption.org) as a plugin (`age-plugin-phonetopc`), the way
  `age-plugin-yubikey` works, rather than inventing a file format.

Compared with a long passphrase:

| | Phone + fingerprint | Long passphrase |
|---|---|---|
| Stolen disk or backup | Safe: the key isn't on the PC | Safe, if the passphrase is strong |
| Keylogger, shoulder-surfing, phishing | Safe: nothing to type | Captured once, usable forever |
| Weak human choices | Nothing to choose | Only as good as the passphrase |
| Forced unlock (police, coercion) | Weaker: a finger can be pressed onto the sensor, and in many countries you can be compelled to give a fingerprint but not a password | Stronger |
| Lost or broken phone, changed fingerprints | Data lost without a recovery key | Nothing to lose |
| PC compromised while decrypting | Key leaks | Key leaks |

The strongest setup combines them: phone + fingerprint (optionally plus a short PIN) for daily use, and a
passphrase kept offline for recovery.

### 8. Engineering practice

| Practice | What it covers |
|---|---|
| **Threat model** | One page: what is protected, against whom, and what is out of scope (a rooted phone, malware running as you). |
| **Fuzz tests** | Go fuzzing on everything that parses network data: JSON-RPC, pairing messages, panel layouts. |
| **Signed, reproducible releases** | The APK signing key kept offline; checksums and signatures for the PC binaries. |
| **Dependency scanning** | `govulncheck` and Gradle dependency checks in CI. |
| **Security policy** | How people report vulnerabilities. |
| **An outside review** | Before calling it secure, have someone else try to break the pairing and approval design. |

## Order

1. **Re-audit and fix, plus CI.** Step 1.
2. **Return the signature from `/v1/approvals`.** Small.
3. **Key attestation and StrongBox.** Medium-sized; the biggest step up.
4. **Number matching and rate limits.**
5. **Sign more context.**
6. **Daemon as its own system user.**
7. **Key unwrapping and the age plugin.**
8. **Fuzzing, signed releases and an outside review**, before announcing it as secure.
