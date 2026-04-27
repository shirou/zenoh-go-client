#!/usr/bin/env bash
#
# smoke_test.sh — exercise every example binary against a running zenohd.
#
# Pairs covered:
#   z_pub                 ↔ z_sub
#   z_put → z_storage     → z_get
#   z_ping                ↔ z_pong
#   z_pub_thr             ↔ z_sub_thr
#   z_liveliness          → z_get_liveliness / z_sub_liveliness
#
# Requires: a zenohd reachable at $ENDPOINT (default tcp/127.0.0.1:7447).
# The repo's `make interop-up` will start one in docker compose.

set -u -o pipefail

ENDPOINT="${ENDPOINT:-tcp/127.0.0.1:7447}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR="$(mktemp -d -t zenoh-smoke.XXXXXX)"
LOG_DIR="$(mktemp -d -t zenoh-smoke-logs.XXXXXX)"
trap 'rm -rf "$BIN_DIR"' EXIT

PASS=0
FAIL=0
FAILED_NAMES=()

green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }
info()  { printf '\033[36m== %s\033[0m\n' "$*"; }

build() {
  info "Building example binaries → $BIN_DIR"
  for d in "$ROOT"/examples/z_*; do
    [ -d "$d" ] || continue
    name="$(basename "$d")"
    if ! (cd "$ROOT" && go build -o "$BIN_DIR/$name" "./examples/$name"); then
      red "build failed: $name"
      exit 1
    fi
  done
}

# wait_for_line <log> <pattern> <timeout-seconds>
wait_for_line() {
  local log="$1" pat="$2" timeout="$3"
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if grep -q -- "$pat" "$log" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# stop_pid <pid> — best-effort SIGINT then SIGKILL
stop_pid() {
  local pid="$1"
  if [ -z "$pid" ]; then return; fi
  if ! kill -0 "$pid" 2>/dev/null; then return; fi
  kill -INT "$pid" 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if ! kill -0 "$pid" 2>/dev/null; then return; fi
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# record_result <name> <ok|fail> [reason]
record_result() {
  local name="$1" status="$2" reason="${3:-}"
  if [ "$status" = "ok" ]; then
    green "PASS: $name"
    PASS=$((PASS + 1))
  else
    red "FAIL: $name — $reason"
    FAIL=$((FAIL + 1))
    FAILED_NAMES+=("$name")
  fi
}

# ---- tests ---------------------------------------------------------------

test_pub_sub() {
  local name="z_pub ↔ z_sub"
  info "$name"
  local key="demo/example/smoke-pubsub-$$"
  local sub_log="$LOG_DIR/sub.log"
  : > "$sub_log"

  "$BIN_DIR/z_sub" --endpoint "$ENDPOINT" --key "$key" >"$sub_log" 2>&1 &
  local sub_pid=$!
  # Give the subscriber a beat to declare.
  sleep 0.5

  "$BIN_DIR/z_pub" --endpoint "$ENDPOINT" --key "$key" --value smoke \
    >"$LOG_DIR/pub.log" 2>&1 &
  local pub_pid=$!

  if wait_for_line "$sub_log" '\[Subscriber\]' 10; then
    record_result "$name" ok
  else
    record_result "$name" fail "subscriber received no samples"
  fi

  stop_pid "$pub_pid"
  stop_pid "$sub_pid"
}

test_put_storage_get() {
  local name="z_put → z_storage → z_get"
  info "$name"
  local key="demo/example/smoke-storage-$$"
  local store_log="$LOG_DIR/storage.log"
  local get_log="$LOG_DIR/get.log"
  : > "$store_log" "$get_log"

  "$BIN_DIR/z_storage" --endpoint "$ENDPOINT" --key "demo/example/**" >"$store_log" 2>&1 &
  local store_pid=$!
  sleep 0.5

  "$BIN_DIR/z_put" --endpoint "$ENDPOINT" --key "$key" --value smoke-put >"$LOG_DIR/put.log" 2>&1
  if [ $? -ne 0 ]; then
    record_result "$name" fail "z_put exited non-zero"
    stop_pid "$store_pid"
    return
  fi

  if ! wait_for_line "$store_log" "$key" 10; then
    record_result "$name" fail "storage did not record the put"
    stop_pid "$store_pid"
    return
  fi

  if "$BIN_DIR/z_get" --endpoint "$ENDPOINT" --key "$key" --timeout 5s \
       >"$get_log" 2>&1; then
    if grep -q '\[Get\] ok .*smoke-put' "$get_log"; then
      record_result "$name" ok
    else
      record_result "$name" fail "z_get missing expected reply (see $get_log)"
    fi
  else
    record_result "$name" fail "z_get exited non-zero"
  fi

  stop_pid "$store_pid"
}

test_ping_pong() {
  local name="z_ping ↔ z_pong"
  info "$name"
  local pong_log="$LOG_DIR/pong.log"
  local ping_log="$LOG_DIR/ping.log"
  : > "$pong_log" "$ping_log"

  "$BIN_DIR/z_pong" --endpoint "$ENDPOINT" >"$pong_log" 2>&1 &
  local pong_pid=$!
  sleep 0.5

  if "$BIN_DIR/z_ping" --endpoint "$ENDPOINT" --size 32 --samples 5 \
       --warmup 200ms >"$ping_log" 2>&1; then
    if grep -qE 'rtt=[0-9]+us' "$ping_log"; then
      record_result "$name" ok
    else
      record_result "$name" fail "no rtt= lines in ping output"
    fi
  else
    record_result "$name" fail "z_ping exited non-zero"
  fi

  stop_pid "$pong_pid"
}

test_pub_sub_thr() {
  local name="z_pub_thr ↔ z_sub_thr"
  info "$name"
  local pub_log="$LOG_DIR/pub_thr.log"
  local sub_log="$LOG_DIR/sub_thr.log"
  : > "$pub_log" "$sub_log"

  "$BIN_DIR/z_pub_thr" --endpoint "$ENDPOINT" --size 64 >"$pub_log" 2>&1 &
  local pub_pid=$!

  # Small numbers so the test finishes quickly: 1 round of 1000 messages.
  "$BIN_DIR/z_sub_thr" --endpoint "$ENDPOINT" --samples 1 --number 1000 \
    >"$sub_log" 2>&1 &
  local sub_pid=$!

  if wait_for_line "$sub_log" 'msgs/s' 30; then
    record_result "$name" ok
  else
    record_result "$name" fail "no msgs/s line from z_sub_thr"
  fi

  stop_pid "$sub_pid"
  stop_pid "$pub_pid"
}

test_liveliness() {
  local name="z_liveliness → z_get_liveliness / z_sub_liveliness"
  info "$name"
  local key="group1/smoke-$$"
  local sub_log="$LOG_DIR/sub_liveliness.log"
  local tok_log="$LOG_DIR/liveliness.log"
  local get_log="$LOG_DIR/get_liveliness.log"
  : > "$sub_log" "$tok_log" "$get_log"

  "$BIN_DIR/z_sub_liveliness" --endpoint "$ENDPOINT" --key "group1/**" >"$sub_log" 2>&1 &
  local sub_pid=$!
  sleep 0.5

  "$BIN_DIR/z_liveliness" --endpoint "$ENDPOINT" --key "$key" >"$tok_log" 2>&1 &
  local tok_pid=$!

  if ! wait_for_line "$sub_log" "New alive token .*$key" 10; then
    record_result "$name (sub-alive)" fail "sub_liveliness did not see new token"
    stop_pid "$tok_pid"; stop_pid "$sub_pid"
    return
  fi

  if "$BIN_DIR/z_get_liveliness" --endpoint "$ENDPOINT" --key "group1/**" \
       --timeout 3s >"$get_log" 2>&1; then
    if grep -q "Alive token .*$key" "$get_log"; then
      record_result "$name (get)" ok
    else
      record_result "$name (get)" fail "no matching alive token in z_get_liveliness output"
    fi
  else
    record_result "$name (get)" fail "z_get_liveliness exited non-zero"
  fi

  stop_pid "$tok_pid"
  if wait_for_line "$sub_log" "Dropped token .*$key" 10; then
    record_result "$name (sub-dropped)" ok
  else
    record_result "$name (sub-dropped)" fail "sub_liveliness did not see token drop"
  fi

  stop_pid "$sub_pid"
}

# ---- main ----------------------------------------------------------------

build

info "Endpoint: $ENDPOINT"
info "Logs:     $LOG_DIR"

test_pub_sub
test_put_storage_get
test_ping_pong
test_pub_sub_thr
test_liveliness

echo
info "Summary: $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
  red "Failed: ${FAILED_NAMES[*]}"
  echo "Logs preserved in: $LOG_DIR"
  exit 1
fi
green "All smoke tests passed"
echo "Logs preserved in: $LOG_DIR"
