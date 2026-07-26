#!/bin/bash
# Agent-to-agent phone load test: place N concurrent calls to one of our own
# numbers, so the answering agent talks to the calling agent.
#   usage: ./callramp.sh <N> [dest_number] [stagger_ms]
set -u
cd /tmp/achatbot-test
set -a; . ./telnyx.env; set +a
N="${1:-5}"; DEST="${2:-+16095519667}"; STAGGER="${3:-250}"
LOG="ramp_$(date +%H%M%S)_n${N}.log"
echo "=== ramp: $N concurrent calls -> $DEST (stagger ${STAGGER}ms) ===" | tee "$LOG"
before=$(grep -c "telnyx response latency" server-run.log 2>/dev/null || echo 0)
ok=0; fail=0
for i in $(seq 1 "$N"); do
  code=$(curl -s -o /tmp/ramp_resp.$$ -w "%{http_code}" -X POST http://127.0.0.1:4321/api/call \
    -H "Content-Type: application/json" -d "{\"to\":\"$DEST\"}")
  if [ "$code" = "200" ]; then ok=$((ok+1)); else fail=$((fail+1)); echo "  call $i FAILED http=$code $(head -c 160 /tmp/ramp_resp.$$)" | tee -a "$LOG"; fi
  sleep "$(awk "BEGIN{print $STAGGER/1000}")"
done
rm -f /tmp/ramp_resp.$$
echo "placed: ok=$ok fail=$fail" | tee -a "$LOG"
for t in 20 40 60 90; do
  sleep 20
  live=$(grep -c "telnyx media stream connected" server-run.log)
  ended=$(grep -c "telnyx media stream ended" server-run.log)
  errs=$(grep -ci "error\|panic" server-run.log | tail -1)
  gpu=$(nvidia-smi --query-gpu=index,utilization.gpu,memory.used --format=csv,noheader | tr "\n" " | ")
  lat=$(grep "telnyx response latency" server-run.log | tail -60 | grep -oE "~[0-9]+ms" | tr -d "~ms" | sort -n | awk "{a[NR]=\$1} END{if(NR)printf \"p50=%dms p95=%dms n=%d\", a[int(NR*0.5)+0], a[int(NR*0.95)], NR}")
  echo "[t+${t}s] streams_connected=$live ended=$ended | $lat | GPU: $gpu" | tee -a "$LOG"
done
echo "=== done. log: $LOG ===" | tee -a "$LOG"
