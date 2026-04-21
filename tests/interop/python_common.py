"""Shared helpers for interop scripts.

Each script uses:
  - ``open_session()`` to honour the ZENOH_ENDPOINTS environment variable
  - ``emit(line)`` to print single-line status messages on stdout, each
    followed by an explicit flush so the Go test runner can read them
    synchronously.

Output protocol (line-oriented, stdout; mirrored in interop_test.go):
  ``READY``                 — session opened, entity declared
  ``{"...json..."}``        — one record per received sample/reply
  ``DONE``                  — final record emitted, script about to exit
"""

import base64
import json
import os
import sys

import zenoh

# Sentinels on the stdout line protocol shared with the Go test runner.
# Keep in sync with readyMarker / doneMarker / goMarker in interop_test.go.
READY = "READY"
DONE = "DONE"
GO = "GO"


def emit(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def open_session() -> zenoh.Session:
    endpoint = os.environ.get("ZENOH_ENDPOINTS", "tcp/zenohd:7447")
    cfg = zenoh.Config()
    cfg.insert_json5("connect/endpoints", json.dumps([endpoint]))
    cfg.insert_json5("mode", '"client"')
    cfg.insert_json5("scouting/multicast/enabled", "false")
    return zenoh.open(cfg)


def encode_payload(payload: bytes) -> str:
    return base64.b64encode(bytes(payload)).decode("ascii")


def sample_to_json(sample) -> str:
    return json.dumps(
        {
            "key": str(sample.key_expr),
            "payload": encode_payload(sample.payload.to_bytes()),
            "kind": str(sample.kind),
        }
    )


def reply_to_json(reply) -> str:
    if reply.ok is not None:
        s = reply.ok
        return json.dumps(
            {
                "kind": "ok",
                "key": str(s.key_expr),
                "payload": encode_payload(s.payload.to_bytes()),
            }
        )
    err = reply.err
    return json.dumps(
        {
            "kind": "err",
            "payload": encode_payload(err.payload.to_bytes()),
        }
    )
