#!/usr/bin/env bash
# Find where first-turn TTFT crosses from acceptable to not, so the
# server.first_turn_* thresholds are measured rather than guessed.
#
# Runs BOTH workloads at every concurrency, and the good case is not optional.
# The bad case alone says where to trip; only the good case says the threshold
# will not fire on healthy campaign traffic — a gate that refuses work the stack
# can serve is worse than no gate at all, because it looks like a capacity
# problem and sends you optimising the wrong thing.
#
#   PORT=8001 ./calibrate.sh
#
# One replica by default, to compare against the numbers in
# docs/NEXT-BACKPRESSURE.md, which were measured the same way.
set -u
PORT="${PORT:-8001}"
TURNS="${TURNS:-8}"
THINK="${THINK:-12.4}"
TOKENS="${TOKENS:-3000}"
STEPS="${STEPS:-6 12 20 30 45 60}"

cd "$(dirname "$0")"
echo "calibrating on port $PORT: $TOKENS-token prompts, $TURNS turns, ${THINK}s think"
echo

for n in $STEPS; do
  for mode in shared distinct; do
    # Cold cache before each run. A run that inherited the previous one's
    # prefixes measures the wrong thing, and turn 1 is precisely the turn whose
    # cost depends on what is already resident.
    curl -s -m 10 -X POST "http://127.0.0.1:$PORT/flush_cache" >/dev/null 2>&1
    sleep 3
    python3 turnbench.py "$PORT" "$n" "$TURNS" "$THINK" "$TOKENS" "$mode"
  done
  echo
done
