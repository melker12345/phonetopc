"""Run a command only after you approve it with your fingerprint on the phone.

Usage::

    python approve.py "Run deploy?" -- ./deploy.sh --prod
    python approve.py "Wipe the test database?" && make reset-db
    python approve.py --timeout 60 --device "Pixel 8" "Push to main?" -- git push

The phone shows the question in a notification; *Approve* opens the
fingerprint prompt. The command runs only if the phone answered "approved"
*and* its signature checked out: the answer is signed by a key on the phone
that can only be used right after a fingerprint, over a fresh random nonce and
this exact question, and verified here (spec/APPROVAL.md).

Exit status: the command's own status when it ran; 0 when approved without a
command; 1 when not approved (denied, cancelled, timed out, ...); 2 on errors.

Needs a paired phone (``phonetopc pair``) and ``pip install 'phonetopc[approval]'``.
"""

from __future__ import annotations

import argparse
import subprocess
import sys

import phonetopc
import phonetopc.sync


def approved(
    reason: str, *, addr: str | None = None, device: str | None = None, timeout: float = 30.0
) -> bool:
    """Ask the paired phone; ``True`` only for a verified fingerprint approval."""
    with phonetopc.sync.connect(addr, device=device, client_name="approve.py") as phone:
        result = phone.request_approval(reason, timeout=timeout)
    if not result:
        print(f"approve.py: not approved ({result.why})", file=sys.stderr)
    return bool(result)


def main(argv: list[str]) -> int:
    command: list[str] = []
    if "--" in argv:
        i = argv.index("--")
        argv, command = argv[:i], argv[i + 1 :]
    parser = argparse.ArgumentParser(
        prog="approve.py",
        description="Run a command only after a fingerprint approval on the phone.",
        epilog='example: approve.py "Run deploy?" -- ./deploy.sh',
    )
    parser.add_argument("reason", help="the question shown on the phone (up to 200 characters)")
    parser.add_argument("--addr", help="phone host[:port] (default: the paired phone)")
    parser.add_argument("--device", metavar="NAME_OR_ID", help="which paired phone")
    parser.add_argument(
        "--timeout", type=float, default=30.0, help="seconds to wait for you (5-120, default 30)"
    )
    args = parser.parse_args(argv)

    try:
        ok = approved(args.reason, addr=args.addr, device=args.device, timeout=args.timeout)
    except KeyboardInterrupt:
        return 130
    except (phonetopc.Error, OSError, ValueError) as exc:
        # Includes ApprovalKeyChanged (fingerprints changed on the phone: run
        # `phonetopc approval-key --accept`), ApprovalVerificationError (a "yes"
        # without a valid signature) and a missing cryptography package.
        print(f"approve.py: {exc}", file=sys.stderr)
        return 2
    if not ok:
        return 1
    if not command:
        return 0
    try:
        return subprocess.run(command).returncode
    except OSError as exc:
        print(f"approve.py: can't run {command[0]}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
