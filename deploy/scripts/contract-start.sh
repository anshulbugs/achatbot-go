#!/bin/bash
# Bring up the PLATFORM-CONTRACT instance (:4399) after a restart.
#
#   bash deploy/scripts/contract-start.sh
#
# Idempotent: stops the previous instance and its tunnel, starts a new one,
# waits for readiness, and prints both addresses. Assumes the GPU services are
# already up (deploy/scripts/up-voice-4gpu.sh does those).
#
# TWO ADDRESSES, ON PURPOSE — they are not interchangeable:
#
#   http://<box-ip>:4399     what the PLATFORM dispatches to. Plain HTTP, so
#                            integration testing only: the dispatch body carries
#                            the tenant's Telnyx api_key and HMAC authenticates
#                            a request without encrypting it.
#   https://<tunnel>         what the BROWSER and TELNYX use. Both need TLS —
#                            getUserMedia refuses a microphone on a non-secure
#                            origin, and the Telnyx media stream URL is derived
#                            from this, so plain HTTP would make it ws:// where
#                            Telnyx expects wss://.
#
# The tunnel is what TELNYX_PUBLIC_URL is set to for exactly that reason. The
# platform is unaffected: both addresses reach the same process.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

PORT="${CONTRACT_PORT:-4399}"
SECRETS="${SECRETS:-./rexa-secrets.env}"
TELNYX_ENV="${TELNYX_ENV:-./telnyx.env}"
BIN="${BIN:-./rexa-server}"
CF="${CLOUDFLARED:-./cloudflared}"
# Fall back to a system install, then to the one deps-install.sh puts in $HOME.
[ -x "$CF" ] || CF="$(command -v cloudflared 2>/dev/null || echo "$HOME/cloudflared")"
POOL="${POOL:-4}"
# The Daily sidecar: a Python process per browser call that joins the room
# and pipes its audio to /room/media. Without it /connection_webrtc refuses
# rather than handing back a room nobody is in. sidecar-install.sh makes it.
SIDECAR_PYTHON="${SIDECAR_PYTHON:-$HOME/sidecar-venv/bin/python}"
SIDECAR_SCRIPT="${SIDECAR_SCRIPT:-$PWD/deploy/sidecar/room_agent.py}"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

[ -x "$BIN" ]      || die "$BIN missing — build it: go build -o rexa-server ./examples/websocket"
[ -f "$SECRETS" ]  || die "$SECRETS missing — it holds the two HMAC secrets shared with the platform"
[ -f "config.yaml" ] || die "config.yaml missing — cp deploy/config.yaml.example config.yaml"

# Keep the log across restarts.
#
# It used to be truncated on every start, which is fine until someone asks "what
# happened on that call an hour ago" and the answer is gone because the server
# was redeployed since. Appended instead, with a marker so one run can be told
# from the next, and rotated when it gets large.
if [ -f "rexa-${PORT}.log" ] && [ "$(stat -c%s "rexa-${PORT}.log" 2>/dev/null || echo 0)" -gt 209715200 ]; then
  mv "rexa-${PORT}.log" "rexa-${PORT}.log.1"
fi
printf '
===== agent starting %s =====
' "$(date -Is)" >> "rexa-${PORT}.log"

log "Stopping the previous instance"
# Match the executable name exactly. `pkill -f rexa-server` would also match
# this script's own command line and the ssh command that launched it.
pkill -x "$(basename "$BIN")" 2>/dev/null || true
# Kill only OUR tunnel. A bare `pkill -f "cloudflared tunnel"` takes down every
# other tunnel on the box, which on a shared machine is someone else's outage.
pkill -f "cloudflared tunnel --url http://127.0.0.1:${PORT}" 2>/dev/null || true
sleep 2

log "Public tunnel for the browser and Telnyx"
[ -x "$CF" ] || command -v cloudflared >/dev/null || die "cloudflared not found (set CLOUDFLARED=)"
rm -f "cf-${PORT}.log"
nohup setsid "$CF" tunnel --url "http://127.0.0.1:${PORT}" --no-autoupdate \
  > "cf-${PORT}.log" 2>&1 < /dev/null &
disown
PUBLIC=""
for _ in $(seq 1 40); do
  PUBLIC=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "cf-${PORT}.log" | head -1 || true)
  [ -n "$PUBLIC" ] && break
  sleep 1
done
[ -n "$PUBLIC" ] || die "tunnel did not come up — see cf-${PORT}.log"
echo "$PUBLIC" > "tunnel-url-${PORT}.txt"
echo "    $PUBLIC"

log "Starting the contract instance on :${PORT}"
set -a
# shellcheck disable=SC1090
. "$SECRETS"
# Optional: with Telnyx credentials the DEMO page on this instance can also
# place phone calls. The contract path never uses them — the platform sends its
# own per-tenant credentials with each dispatch.
[ -f "$TELNYX_ENV" ] && . "$TELNYX_ENV"
set +a

ACHATBOT_SERVER_ADDR=":${PORT}" \
ACHATBOT_VAD_POOL_SIZE="$POOL" \
ACHATBOT_ASR_POOL_SIZE="$POOL" \
ACHATBOT_TTS_POOL_SIZE="$POOL" \
ACHATBOT_SERVER_MAX_GPU_CALLS="${MAX_GPU_CALLS:-61}" \
ACHATBOT_SERVER_MAX_TOTAL_CALLS="${MAX_TOTAL_CALLS:-200}" \
TELNYX_PUBLIC_URL="$PUBLIC" \
SIDECAR_PYTHON="$SIDECAR_PYTHON" \
SIDECAR_SCRIPT="$SIDECAR_SCRIPT" \
  nohup setsid "$BIN" -config config.yaml >> "rexa-${PORT}.log" 2>&1 < /dev/null &
disown

printf '    waiting for /health '
for _ in $(seq 1 60); do
  if curl -sf -m 3 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    printf ' ready\n'; break
  fi
  printf '.'; sleep 3
done
curl -sf -m 3 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1 \
  || { tail -30 "rexa-${PORT}.log"; die "server did not become ready (last 30 lines above)"; }

IP=$(curl -s -m 5 https://api.ipify.org 2>/dev/null || echo "<box-ip>")
log "Up"
printf '  %-22s %s\n' "platform dispatches to" "http://${IP}:${PORT}"
printf '  %-22s %s\n' "browser test page"      "$PUBLIC"
printf '  %-22s %s\n' "dashboard"              "${PUBLIC}/dashboard"
printf '  %-22s %s\n' "telnyx webhooks"        "$PUBLIC"
echo
echo "The tunnel hostname changes on every restart. The platform's base URL does"
echo "not — it dispatches to the IP, which is why the two are kept separate."
echo
echo "If the port is not reachable from outside:  sudo ufw allow ${PORT}/tcp"
