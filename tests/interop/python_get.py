"""Issue one Get on --key and print each reply as JSON; finish with DONE."""

import argparse
import sys

from python_common import DONE, READY, emit, open_session, reply_to_json


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    args = p.parse_args()

    with open_session() as session:
        emit(READY)
        for reply in session.get(args.key):
            emit(reply_to_json(reply))
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
