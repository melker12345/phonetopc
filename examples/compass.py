"""Print the phone's compass heading. Usage: python compass.py [host[:port] | ws://...]"""

import asyncio
import sys

import phonetopc

POINTS = ["N", "NE", "E", "SE", "S", "SW", "W", "NW"]


async def main() -> None:
    # None: $PHONETOPC_ADDR, else the phone paired with `phonetopc pair`.
    # A ws:// URL (e.g. ws://127.0.0.1:8765 for phonetopc-mock) means dev mode.
    addr = sys.argv[1] if len(sys.argv) > 1 else None
    async with phonetopc.connect(addr, client_name="compass") as phone:
        print(f"connected to {phone.device.name}; lay the phone flat")
        # Degrees clockwise from magnetic north that the top of the phone points to.
        # min_delta skips readings that moved less than half a degree.
        async with phone.subscribe("compass", rate_hz=5, min_delta=0.5) as events:
            async for ev in events:
                heading = ev.value
                point = POINTS[round(heading / 45) % 8]
                print(f"#{ev.seq:<6} {heading:5.1f}° {point}")


asyncio.run(main())
