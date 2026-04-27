"""Subscribe to --key in peer mode, emit each sample as JSON, then DONE."""

import argparse
import json
import os
import sys

import zenoh

from python_common import DONE, READY, emit, sample_to_json


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
    args = p.parse_args()

    with open_peer_session() as session:
        received = 0

        def on_sample(sample) -> None:
            nonlocal received
            emit(sample_to_json(sample))
            received += 1

        sub = session.declare_subscriber(args.key, on_sample)
        emit(READY)

        # Spin until we have the requested count or stdin closes.
        while received < args.count:
            line = sys.stdin.readline()
            if not line:
                break
        sub.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
