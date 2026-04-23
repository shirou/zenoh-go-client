"""Declare a liveliness token on --key, hold it until stdin says DROP/KILL.

Line protocol:
  READY     — emitted after declare_token succeeds
  DROP<EOL> — graceful undeclare, exit 0 after emitting DONE
  KILL<EOL> — abrupt os._exit without undeclaring (simulates SIGKILL);
              zenohd sees a TCP FIN/RST and revokes the token. Used by
              tests that assert the Delete Sample path driven by
              connection loss rather than by the declarer's cleanup.
"""

import argparse
import os
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
            if not line:
                break
            cmd = line.strip()
            if cmd == "DROP":
                break
            if cmd == "KILL":
                # Skip zenoh cleanup: the OS closes the socket on process
                # exit, mirroring a real SIGKILL. Exit code 137 follows the
                # shell convention (128 + SIGKILL=9) so the parent sees a
                # familiar "got killed" signal even though no signal was
                # actually delivered.
                os._exit(137)
        tok.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
