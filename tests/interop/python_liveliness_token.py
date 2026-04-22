"""Declare a liveliness token on --key, hold it until stdin says DROP.

Line protocol:
  READY     — emitted after declare_token succeeds
  DROP<EOL> — input that triggers undeclare
  DONE      — emitted after undeclare (exit)
"""

import argparse
import sys

from python_common import DONE, READY, emit, open_session


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    args = p.parse_args()

    with open_session() as session:
        tok = session.liveliness().declare_token(args.key)
        emit(READY)
        # Block on readline() rather than `for line in sys.stdin`: the
        # latter can be block-buffered on a pipe even with PYTHONUNBUFFERED.
        while True:
            line = sys.stdin.readline()
            if not line or line.strip() == "DROP":
                break
        tok.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
