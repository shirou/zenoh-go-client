"""Run a peer-mode Python session that publishes N samples on --key.

Connects directly to a Go peer via tcp/<addr> taken from ZENOH_ENDPOINTS.
No router is involved; mode=peer on both ends. Mirrors python_pub.py's
output protocol (READY → wait GO → publish → DONE) so the Go test
runner can reuse the same line scaffolding.
"""

import argparse
import json
import os
import sys
import time

import zenoh

from python_common import DONE, GO, READY, emit


def open_peer_session() -> zenoh.Session:
    endpoint = os.environ.get("ZENOH_ENDPOINTS", "tcp/127.0.0.1:7447")
    cfg = zenoh.Config()
    cfg.insert_json5("mode", '"peer"')
    cfg.insert_json5("connect/endpoints", json.dumps([endpoint]))
    cfg.insert_json5("scouting/multicast/enabled", "false")
    return zenoh.open(cfg)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    p.add_argument("--payload-prefix", default="peer-msg-")
    args = p.parse_args()

    with open_peer_session() as session:
        pub = session.declare_publisher(args.key)
        emit(READY)
        if sys.stdin.readline().strip() != GO:
            return 1
        for i in range(args.count):
            pub.put(f"{args.payload_prefix}{i}")
        time.sleep(0.2)
        pub.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
