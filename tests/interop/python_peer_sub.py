"""Subscribe to --key in peer mode, emit each sample as JSON, then DONE."""

import argparse
import json
import os
import sys
import threading
import time

import zenoh

from python_common import DONE, READY, emit, sample_to_json


def open_peer_session() -> zenoh.Session:
    # Peer-mode tests use ZENOH_PEER_ENDPOINT specifically so the override
    # does not collide with ZENOH_ENDPOINTS (used by every router-based
    # python_*.py script in the same container).
    endpoint = os.environ.get("ZENOH_PEER_ENDPOINT", "tcp/host.docker.internal:7461")
    cfg = zenoh.Config()
    cfg.insert_json5("mode", '"peer"')
    cfg.insert_json5("connect/endpoints", json.dumps([endpoint]))
    cfg.insert_json5("scouting/multicast/enabled", "false")
    return zenoh.open(cfg)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    args = p.parse_args()

    with open_peer_session() as session:
        received = 0
        done = threading.Event()

        def on_sample(sample) -> None:
            nonlocal received
            emit(sample_to_json(sample))
            received += 1
            if received >= args.count:
                done.set()

        sub = session.declare_subscriber(args.key, on_sample)
        emit(READY)

        # Block on the event the callback flips, with a 30s safety timeout
        # so a missed sample doesn't hang the harness forever.
        done.wait(timeout=30.0)
        sub.undeclare()
        # Give the U_SUBSCRIBER a moment to flush before tearing the
        # session down — mirrors python_pub.py's pre-close sleep.
        time.sleep(0.2)
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
