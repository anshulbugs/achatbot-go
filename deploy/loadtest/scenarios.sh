#!/usr/bin/env bash
# The two shapes real traffic actually takes, at the concurrency we claim.
#
# calibrate.sh finds where the gate should trip. This answers a different
# question: will production traffic ever get anywhere near it?
#
#   contact      60 calls, ONE campaign, per-contact block last.
#                A single campaign dialling its list.
#   campaigns12  60 calls spread over 12 campaigns, 5 each, round-robin.
#                A real dispatch queue with several campaigns live at once.
#
# Both are bracketed by shared (identical prompts, the unreachable best case)
# and distinct (unique bytes at the front, the worst case), so the two realistic
# numbers can be read against the two extremes rather than in isolation.
set -u
PORT="${PORT:-8001}"
CALLS="${CALLS:-60}"
TURNS="${TURNS:-8}"
THINK="${THINK:-12.4}"
TOKENS="${TOKENS:-3000}"

cd "$(dirname "$0")"
echo "scenarios on port $PORT: $CALLS calls, $TOKENS-token prompts, $TURNS turns"
echo

for mode in shared contact campaigns12 distinct; do
  # Cold cache each run: turn 1 is precisely the turn whose cost depends on
  # what is already resident, so inheriting the previous run's prefixes would
  # measure the wrong thing.
  curl -s -m 10 -X POST "http://127.0.0.1:$PORT/flush_cache" >/dev/null 2>&1
  sleep 3
  python3 turnbench.py "$PORT" "$CALLS" "$TURNS" "$THINK" "$TOKENS" "$mode"
done
