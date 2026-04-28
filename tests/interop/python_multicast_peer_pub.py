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
    # Rust forbids multicast endpoints in connect/endpoints (see
    # zenoh/src/net/runtime/orchestrator.rs:752 "Forbidden multicast
    # endpoint in connect list"). The multicast peer transport is
    # opened via listen/endpoints instead, which dispatches to
    # add_listener_multicast in the transport manager.
    group = os.environ.get("ZENOH_MULTICAST_GROUP", "udp/224.0.0.224:7446")
    cfg = zenoh.Config()
    cfg.insert_json5("mode", '"peer"')
    cfg.insert_json5("listen/endpoints", json.dumps([group]))
    cfg.insert_json5("scouting/multicast/enabled", "false")
    # Match the Go peer's wire: we always attach the QoS extension to
    # JOINs and FRAMEs (16 lane SNs). Rust drops a JOIN with ext_qos
    # when transport/multicast/qos/enabled is false (the default in
    # DEFAULT_CONFIG.json5), so explicitly opt in here. See
    # io/zenoh-transport/src/multicast/rx.rs:137 for the gate.
    cfg.insert_json5("transport/multicast/qos/enabled", "true")
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
