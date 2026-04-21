"""Liveliness get: print the key expr of every live token matching --key."""

import argparse
import sys

from python_common import DONE, READY, emit, open_session, reply_to_json


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    args = p.parse_args()

    with open_session() as session:
        emit(READY)
        for reply in session.liveliness().get(args.key):
            if reply.ok is None:
                continue
            emit(reply_to_json(reply))
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
