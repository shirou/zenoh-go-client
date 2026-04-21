"""Subscribe on --key, emit JSON per sample, exit when --count received."""

import argparse
import sys
import threading

from python_common import DONE, READY, emit, open_session, sample_to_json


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--key", required=True)
    p.add_argument("--count", type=int, default=1)
    args = p.parse_args()

    received = 0
    done = threading.Event()
    lock = threading.Lock()

    def on_sample(sample):
        nonlocal received
        with lock:
            emit(sample_to_json(sample))
            received += 1
            if received >= args.count:
                done.set()

    with open_session() as session:
        sub = session.declare_subscriber(args.key, on_sample)
        emit(READY)
        done.wait()
        sub.undeclare()
        emit(DONE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
