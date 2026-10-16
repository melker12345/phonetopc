# Reporting a vulnerability

Please report vulnerabilities privately, through GitHub: on the repository's **Security** tab, choose
**Report a vulnerability**. Don't open a public issue or pull request for them.

Include what an attacker can do, what they need (network position, a stolen token, an app on the phone, …), and
steps or code that show it.

## In scope

- Pairing, TLS pinning and token checks between the PC and the phone (`spec/SECURITY.md`)
- Fingerprint approvals and decryption, including the age plugin (`spec/APPROVAL.md`, `spec/DECRYPT.md`)
- The phone app's server and the PC's `phonetopcd`, `phonetopc` and `age-plugin-phonetopc`
- The local HTTP API's access checks (`spec/LOCAL-API.md` §1)
- Anything where the code does less than the specs promise

What the project does not try to protect against is in [`spec/THREAT-MODEL.md`](../spec/THREAT-MODEL.md), for
example malware running as you on the PC or a rooted phone. Reports that show those limits matter more than
expected are still welcome.

## What to expect

- An answer, aimed for within a week, saying whether it's confirmed.
- A fix in the specs and in every implementation they affect, with a regression test, as soon as it's understood.
- Credit in the advisory, unless you'd rather not.
- Please give us 90 days, or until a fix is released, before publishing.

Only the latest commit on the default branch is supported.
