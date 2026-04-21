"""Declare a queryable on --key. Each query receives one reply with --reply-payload."""

import argparse
import sys

from python_common import DONE, READY, emit, open_session


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--reply-key", default=None, help="key to reply on (defaults to --key)")
    p.add_argument("--reply-payload", default="ok")
    args = p.parse_args()
    reply_key = args.reply_key or args.key

    def on_query(query):
        query.reply(reply_key, args.reply_payload)

    with open_session() as session:
        qbl = session.declare_queryable(args.key, on_query)
        emit(READY)
        # Block until the test runner signals end-of-input on stdin.
        sys.stdin.readline()
        qbl.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
