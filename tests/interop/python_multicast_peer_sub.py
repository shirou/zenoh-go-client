"""Subscribe to --key in peer-multicast mode, emit each sample as JSON, then DONE."""

import argparse
import json
import os
import sys
import threading
import time

import zenoh

from python_common import DONE, READY, emit, sample_to_json


def open_multicast_peer_session() -> zenoh.Session:
    group = os.environ.get("ZENOH_MULTICAST_GROUP", "udp/224.0.0.224:7446")
    cfg = zenoh.Config()
    cfg.insert_json5("mode", '"peer"')
    cfg.insert_json5("connect/endpoints", json.dumps([group]))
    cfg.insert_json5("scouting/multicast/enabled", "true")
    cfg.insert_json5("scouting/multicast/address", json.dumps(group.removeprefix("udp/")))
    return zenoh.open(cfg)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    args = p.parse_args()

    with open_multicast_peer_session() as session:
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

        # 30s safety timeout — a missed sample shouldn't hang the harness.
        done.wait(timeout=30.0)
        sub.undeclare()
        time.sleep(0.2)
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
