#!/usr/bin/env bash
# What would prefix-aware routing be worth, measured before changing nginx.
#
# Same total load and same campaign count as a run through the load balancer:
# 60 calls, 12 campaigns. The only difference is that here each replica gets a
# DISJOINT set of campaigns, which is the routing `hash $llm_route` produces.
# Anything better than the balanced run is what connection-counting was costing.
#
# Kept in a file rather than inlined over ssh for a specific reason: a wait loop
# written as `bash -c 'while pgrep -f turnbench...'` matches ITS OWN command
# line and hangs forever. Same trap as pkill -f killing its own shell.
set -u
PORT_A="${PORT_A:-8001}"
PORT_B="${PORT_B:-8002}"
CALLS="${CALLS:-30}"      # per replica; 60 total
CAMPS="${CAMPS:-6}"       # per replica; 12 total
TURNS="${TURNS:-8}"
THINK="${THINK:-12.4}"
TOKENS="${TOKENS:-3000}"

cd "$(dirname "$0")"
curl -s -m 10 -X POST "http://127.0.0.1:$PORT_A/flush_cache" >/dev/null 2>&1
curl -s -m 10 -X POST "http://127.0.0.1:$PORT_B/flush_cache" >/dev/null 2>&1
sleep 3

python3 turnbench.py "$PORT_A" "$CALLS" "$TURNS" "$THINK" "$TOKENS" "uniqcamp$CAMPS" 0 > /tmp/aff_a.log 2>&1 &
pid_a=$!
python3 turnbench.py "$PORT_B" "$CALLS" "$TURNS" "$THINK" "$TOKENS" "uniqcamp$CAMPS" "$CAMPS" > /tmp/aff_b.log 2>&1 &
pid_b=$!
wait $pid_a $pid_b

echo "[replica $PORT_A, campaigns 0-$((CAMPS-1))]"
cat /tmp/aff_a.log
echo "[replica $PORT_B, campaigns $CAMPS-$((CAMPS*2-1))]"
cat /tmp/aff_b.log
