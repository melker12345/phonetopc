"""Hello world: print the phone's gyroscope. Usage: python print_gyro.py [host[:port]]"""

import asyncio
import sys

import phonetopc


async def main() -> None:
    addr = sys.argv[1] if len(sys.argv) > 1 else None  # None: $PHONETOPC_ADDR or 127.0.0.1:8765
    async with phonetopc.connect(addr, client_name="print_gyro") as phone:
        print(f"connected to {phone.device.name}")
        async with phone.subscribe("gyro", rate_hz=50) as events:
            async for ev in events:
                x, y, z = ev.value
                print(f"#{ev.seq:<6} x={x:+.3f} y={y:+.3f} z={z:+.3f} {ev.unit}")


asyncio.run(main())
