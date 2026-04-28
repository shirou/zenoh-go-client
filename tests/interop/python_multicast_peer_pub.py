"""Run a peer-mode Python session that publishes N samples on --key.

Joins the multicast group named in ZENOH_MULTICAST_GROUP (default
``udp/224.0.0.224:7446``) as a peer, with no router involved. Mirrors
python_pub.py's READY → wait GO → publish → DONE protocol so the Go
test runner can reuse the same line scaffolding.
"""

import argparse
import json
import os
import sys
import time

import zenoh

from python_common import DONE, GO, READY, emit


def open_multicast_peer_session() -> zenoh.Session:
    group = os.environ.get("ZENOH_MULTICAST_GROUP", "udp/224.0.0.224:7446")
    cfg = zenoh.Config()
    cfg.insert_json5("mode", '"peer"')
    cfg.insert_json5("connect/endpoints", json.dumps([group]))
    # Multicast scouting is what discovers the group; explicit endpoint
    # plus scouting=true is the rust default for peer-multicast.
    cfg.insert_json5("scouting/multicast/enabled", "true")
    cfg.insert_json5("scouting/multicast/address", json.dumps(group.removeprefix("udp/")))
    return zenoh.open(cfg)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    p.add_argument("--payload-prefix", default="mpeer-")
    args = p.parse_args()

    with open_multicast_peer_session() as session:
        pub = session.declare_publisher(args.key)
        emit(READY)
        if sys.stdin.readline().strip() != GO:
            return 1
        for i in range(args.count):
            pub.put(f"{args.payload_prefix}{i}")
        # Brief settle so the last put traverses the multicast group
        # before the session tears down.
        time.sleep(0.3)
        pub.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
