#!/bin/bash
# Watch the agent during live traffic. One line per sample, anomalies flagged.
#
#   bash deploy/scripts/watch-calls.sh                  # every 10s, forever
#   INTERVAL=5 DURATION=3600 bash deploy/scripts/watch-calls.sh | tee run.log
#
# WHAT TO WATCH FOR, in the order it usually goes wrong:
#
#   gor climbing while live stays flat   goroutine leak. The earliest signal
#                                        there is: every call starts several and
#                                        every one must end with it. Rises
#                                        before memory does.
#   rss climbing and never returning     a leak the Go heap cannot explain —
#                                        the ONNX runtimes inside VAD/ASR, or
#                                        pool growth that never shrinks.
#   fds climbing                         socket leak. Presents as "too many open
#                                        files" long before memory matters.
#   reaped > 0                           hangup webhooks are being missed. Each
#                                        one is capacity that would have leaked
#                                        away permanently without the reaper.
#   lpf > 0                              live events never reached the caller's
#                                        Redis. Silent by design, so this
#                                        counter is the only place it shows.
#   trips climbing                       first-turn backpressure firing. Means
#                                        dispatched prompts are not sharing
#                                        prefixes.
#   sidecars != webrtc calls             a room agent outlived its call.
#
# The flags are advisory: they compare against the FIRST sample, so start this
# before the traffic does.
set -uo pipefail

HOST="${HOST:-127.0.0.1:4399}"
INTERVAL="${INTERVAL:-10}"
DURATION="${DURATION:-0}"        # 0 = until interrupted
GROWTH_PCT="${GROWTH_PCT:-50}"   # flag when a baseline grows by this much

command -v curl >/dev/null || { echo "curl required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 required" >&2; exit 1; }

printf '%-8s %5s %5s %5s %5s  %6s %6s %5s %6s  %5s %5s %5s %5s %5s\n' \
  time live gpu res vm gor heap rss fds ftp95 trips reap lpf rej

start=$(date +%s)
base_gor=0 base_rss=0 base_fds=0

while :; do
  snap=$(curl -sf -m 5 "http://${HOST}/health" 2>/dev/null) || {
    printf '%-8s  UNREACHABLE\n' "$(date +%H:%M:%S)"
    sleep "$INTERVAL"; continue
  }

  # One python pass rather than a jq dependency: python3 is already required by
  # the load tests and this box has no jq.
  read -r live gpu res vm gor heap rss fds ftp95 trips reap lpf rej accepting <<<"$(
    printf '%s' "$snap" | python3 -c '
import json,sys
d=json.load(sys.stdin)
c,r,t,f=d["calls"],d.get("runtime",{}),d["totals"],d.get("first_turn",{})
print(c["total"], c["on_gpu"], c["reserved"], c["voicemail"],
      r.get("goroutines",0), r.get("heap_mb",0), r.get("rss_mb",0), r.get("open_fds",0),
      f.get("p95_ms",0), f.get("trips",0), t.get("reaped",0),
      t.get("live_publish_failures",0), t.get("rejected",0),
      "yes" if d.get("accepting") else "NO")
')"

  [ "$base_gor" = 0 ] && { base_gor=$gor; base_rss=$rss; base_fds=$fds; }

  flags=""
  [ "$accepting" = "NO" ] && flags="$flags not-accepting"
  [ "$trips" -gt 0 ] && flags="$flags first-turn-trips=$trips"
  [ "$reap" -gt 0 ] && flags="$flags reaped=$reap"
  [ "$lpf" -gt 0 ] && flags="$flags redis-fail=$lpf"
  # Growth flags only once there is a baseline worth comparing against.
  grew() { [ "$2" -gt 0 ] && [ $(( ($1 - $2) * 100 / $2 )) -ge "$GROWTH_PCT" ]; }
  grew "$gor" "$base_gor" && [ "$live" -eq 0 ] && flags="$flags GOROUTINE-LEAK?"
  grew "$rss" "$base_rss" && [ "$live" -eq 0 ] && flags="$flags RSS-GROWTH?"
  grew "$fds" "$base_fds" && [ "$live" -eq 0 ] && flags="$flags FD-LEAK?"

  # Sidecar processes should match the number of browser calls in flight.
  side=$(pgrep -fc "room_agent.py" 2>/dev/null || echo 0)

  printf '%-8s %5s %5s %5s %5s  %6s %5sM %4sM %6s  %5s %5s %5s %5s %5s  side=%s%s\n' \
    "$(date +%H:%M:%S)" "$live" "$gpu" "$res" "$vm" \
    "$gor" "$heap" "$rss" "$fds" "$ftp95" "$trips" "$reap" "$lpf" "$rej" \
    "$side" "$flags"

  [ "$DURATION" != "0" ] && [ $(( $(date +%s) - start )) -ge "$DURATION" ] && break
  sleep "$INTERVAL"
done

echo
echo "Baselines at start: goroutines=$base_gor rss=${base_rss}MB fds=$base_fds"
echo "Leak flags compare against those and only fire when live calls are 0 —"
echo "growth WITH calls in flight is normal; growth after they drain is not."
