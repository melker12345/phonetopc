# phonetopc threat model

What phonetopc protects, from whom, and what it doesn't. The mechanisms are specified in
[`SECURITY.md`](SECURITY.md), [`APPROVAL.md`](APPROVAL.md), [`DECRYPT.md`](DECRYPT.md) and
[`LOCAL-API.md`](LOCAL-API.md).

## What is protected

| Asset | Where it lives |
|---|---|
| **Pairing**: which PCs a phone serves and which phone a PC trusts | Pins and ids, on both sides |
| **Tokens**: one per paired PC, the PC's credential towards the phone | Phone: a hash. PC: `devices.json`, mode 0600 |
| **Approvals**: "the user said yes with a fingerprint to this reason" | Signed by a Keystore key that needs a fresh strong biometric |
| **Decryption keys**: data that needs the phone and a fingerprint to read | RSA key in Keystore, same biometric rule; never leaves the phone |
| **Sensor data and phone state**: motion, location, NFC tags, battery, panel input | Sent only to paired PCs, over pinned TLS |
| **The phone's screen**: notifications and panels the PC shows | Only from PCs the user allowed, as display text that can't hide or reorder characters |

## Attackers

| Attacker | What stops them | What they can still do |
|---|---|---|
| **Network attacker** (sees, changes and injects traffic, spoofs mDNS) | TLS pinned to each side's key; no fallback to anything else; token only after the pin checks out | Block or drop traffic. Read everything if the user turns on dev mode, which the phone shows while it's on |
| **LAN attacker during pairing** | QR: a secret only the screen shows, and the PC user confirms; a new secret after every prompt. 6-digit code: committed comparison, 1 in a million per run, at most 3 failed runs per listener | Hold the pairing slot for 20 s at a time; end a code comparison early, so the user retries or uses the QR code |
| **Stolen token** | It works only together with the pairing; the user revokes the PC on the phone, which also closes its sessions | Until revoked, from the LAN: read sensors and location, post notifications and panels if allowed, ask for approvals and decryptions. Each of those still needs the user's fingerprint and the number that PC shows its user, but asking keeps the single approval slot `busy` for every PC, and three refusals put the PC in a 5-minute cool-down |
| **Other apps on the phone** | Android's app sandbox: Keystore keys belong to the app; the server needs a token from them too; components that start the server or answer prompts aren't exported | Apps with notification access read approval reasons and the PC's notifications |
| **Malware running as the user on the PC** | Nothing, mostly. Approvals and decryptions still need a fingerprint for each request, after the user picks the number the asking program shows; the phone names that program; three refusals within 10 minutes pause the PC's requests for 5 minutes, against prompt bombing | Read `devices.json` and the PC's key, use the phone as that PC, use the local API without a token (as any program of the user may), word a request's reason to mislead, and read whatever gets decrypted |
| **Other users on the PC** | Files 0600, directories 0700, socket 0600; TCP requests from other users need the token in `$XDG_RUNTIME_DIR` | Nothing beyond what the OS lets them do to your account |
| **Web pages in the user's browser** | Requests with `Origin` or a cross-site `Sec-Fetch-Site` need the token; a `Host` other than loopback is refused (DNS rebinding) | Nothing, unless given the token |
| **Root on the PC** | Nothing | Everything the user and the daemon can do |
| **Root on the phone, or a modified app** | Keys in secure hardware can't be copied out | Fake approvals: the PC trusts the approval key the app first reports and can't yet check that it is a hardware key that needs a fingerprint |
| **Thief with the phone** | The screen lock; Keystore keys need a fingerprint and no PIN fallback; tokens stored as hashes; backups off | With the phone unlocked: see paired PCs, change their permissions, pair a new PC |
| **Thief with the PC's disk** | Full-disk encryption, if used. age files need the phone and a fingerprint; the identity file holds no secret | Without disk encryption: tokens and the PC key are in plain files, so the thief acts as that PC until the user revokes it |
| **Coercion** | Nothing | Force a fingerprint. Approvals and decryptions prove a finger was on the sensor, not that the user wanted it |

## Out of scope

- A compromised PC user account or root on either device; the protections above that still hold there are a bonus.
- Denial of service by anyone who can drop or delay traffic.
- Losing access: adding or removing a fingerprint, or resetting the phone, destroys the decryption key. Encrypt
  to an offline recovery key as well ([`DECRYPT.md`](DECRYPT.md) §6).
- Physical attacks on the phone's secure hardware.
- Supply-chain attacks beyond what CI checks: pinned GitHub Actions, Gradle dependency checksums and `govulncheck`.
