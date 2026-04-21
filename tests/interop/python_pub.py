"""Publish N messages on --key and exit.

Waits for a ``GO`` line on stdin after printing ``READY`` so the caller can
ensure the peer subscriber is declared before emission.
"""

import argparse
import sys
import time

from python_common import DONE, GO, READY, emit, open_session


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    p.add_argument("--payload-prefix", default="msg-")
    args = p.parse_args()

    with open_session() as session:
        pub = session.declare_publisher(args.key)
        emit(READY)
        if sys.stdin.readline().strip() != GO:
            return 1
        for i in range(args.count):
            pub.put(f"{args.payload_prefix}{i}")
        # Give the router a moment to flush before we close.
        time.sleep(0.2)
        pub.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
