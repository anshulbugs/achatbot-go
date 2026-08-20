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

# cfg <key> <fallback> — read a server setting out of config.yaml.
#
# These two are passed as environment variables, and the environment BEATS the
# config file, so a hardcoded default here silently overrides whatever
# config.yaml says. max_gpu_calls used to default to 61, which was measured on
# 4x RTX 5090; carried onto different hardware it over-admits calls, and the
# symptom is a p95 that falls apart while p50 still looks fine. config.yaml is
# the documented place to set capacity, so config.yaml wins unless the caller
# passes MAX_GPU_CALLS= explicitly.
cfg() {
  local v
  v=$(grep -E "^  $1:" config.yaml 2>/dev/null | head -1 | cut -d: -f2- | sed 's/^ *//;s/ *#.*//;s/ *$//')
  [ -n "$v" ] && printf '%s' "$v" || printf '%s' "$2"
}
# The Daily sidecar: a Python process per browser call that joins the room
# and pipes its audio to /room/media. Without it /connection_webrtc refuses
# rather than handing back a room nobody is in. sidecar-install.sh makes it.
SIDECAR_PYTHON="${SIDECAR_PYTHON:-$HOME/sidecar-venv/bin/python}"
SIDECAR_SCRIPT="${SIDECAR_SCRIPT:-$PWD/deploy/sidecar/room_agent.py}"
# The room pre-joiner: holds a listen-in room's session open so Daily registers
# its SIP endpoint during the call rather than during the handover. Inert unless
# server.live_room_prewarm is on. Same venv as the sidecar — same Daily SDK.
PREWARM_SCRIPT="${PREWARM_SCRIPT:-$PWD/deploy/sidecar/room_prewarm.py}"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

# BUILD HERE, always.
#
# This script used to only CHECK that the binary existed, and take a stale one
# without comment. That is a silent failure with no symptom: `git pull` and
# `go build -o /tmp/somewhere` both succeed, the script prints "Up", /health
# answers — and the process is running code from hours earlier. Three separate
# fixes were "deployed" that way and none of them were, which was only caught
# because a log line that should have been there was not.
#
# The build is the deploy. SKIP_BUILD=1 for the rare case where the binary was
# deliberately built elsewhere.
if [ "${SKIP_BUILD:-0}" = "1" ]; then
	[ -x "$BIN" ] || die "$BIN missing and SKIP_BUILD=1 — build it: go build -o rexa-server ./examples/websocket"
	log "Skipping build (SKIP_BUILD=1) — running $BIN as it stands"
else
	log "Building $BIN from $(git rev-parse --short HEAD 2>/dev/null || echo 'working tree')"
	# Build beside it, then rename. Writing straight to $BIN fails with "text
	# file busy" while the old one is running, and building AFTER the stop
	# would mean a failed build leaves nothing serving. Rename replaces the
	# directory entry; the running process keeps its own inode until it exits.
	CGO_ENABLED=1 go build -o "$BIN.new" ./examples/websocket \
		|| die "build failed — not restarting, the running agent is untouched"
	mv -f "$BIN.new" "$BIN" || die "could not put the new binary in place at $BIN"
fi
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

# Refuse to restart into a live call.
#
# Restarting kills every call in flight instantly: the media session ends, the
# sidecar loses its socket, and whoever is on the phone hears silence with no
# explanation. That happened to a real test call during a redeploy, and the
# symptom looked exactly like an audio bug — which cost more time than the
# deploy saved.
#
# FORCE=1 to override when the calls in flight are your own.
if [ "${FORCE:-0}" != "1" ]; then
  live=$(curl -sf -m 3 "http://127.0.0.1:${PORT}/health" 2>/dev/null     | python3 -c 'import json,sys; print(json.load(sys.stdin)["calls"]["total"])' 2>/dev/null || echo 0)
  if [ "${live:-0}" -gt 0 ]; then
    die "$live call(s) in flight — restarting would cut them off. Wait, or FORCE=1 to override."
  fi
fi

log "Stopping the previous instance"
# Match the executable name exactly. `pkill -f rexa-server` would also match
# this script's own command line and the ssh command that launched it.
pkill -x "$(basename "$BIN")" 2>/dev/null || true
# Kill only OUR tunnel. A bare `pkill -f "cloudflared tunnel"` takes down every
# other tunnel on the box, which on a shared machine is someone else's outage.
pkill -f "cloudflared tunnel --url http://127.0.0.1:${PORT}" 2>/dev/null || true
# Room sidecars are children of the server, but only the server's own stopSidecar
# ever ends them — a restart or a hard kill orphans them, and an orphan sits in a
# Daily room forever holding a session for a call that ended hours ago. One was
# found doing exactly that. Sweep them here, where "the old instance is gone" is
# already the invariant being established.
pkill -f "deploy/sidecar/room_agent.py" 2>/dev/null || true
sleep 2

log "Public tunnel for the browser and Telnyx"
[ -x "$CF" ] || command -v cloudflared >/dev/null || die "cloudflared not found (set CLOUDFLARED=)"
rm -f "cf-${PORT}.log"
# --config /dev/null is NOT cosmetic. cloudflared reads /etc/cloudflared/config.yml
# by default, and the Lambda GH200 image ships one for its own JupyterLab tunnel:
# it names a credentials file and an ingress list ending in `- service:
# http_status:404`. A quick tunnel started without --config inherits ALL of that,
# so our trycloudflare hostname matches no ingress rule and every request to it
# returns 404 — while cloudflared logs a healthy registered connection and the
# service answers fine on localhost. Isolating the config is what makes the
# public URL actually reach us.
#
# --protocol http2 goes with it. Dropping the host config also dropped the
# `protocol: http2` it was setting, and cloudflared then defaults to QUIC, which
# cannot dial out from this box: every attempt fails with `CRYPTO_ERROR 0x178
# (remote): tls: no application protocol` and the tunnel registers ZERO
# connections while still printing a URL. UDP egress is evidently filtered here.
nohup setsid "$CF" --config /dev/null --protocol http2 tunnel --url "http://127.0.0.1:${PORT}" --no-autoupdate \
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

# Preflight: say out loud which optional features this start will and will not
# have.
#
# Every one of these degrades SILENTLY. A missing DAILY_API_KEY does not fail
# anything — browser calls are refused, listen-in never appears, browser
# recordings are never made, and barging drops back to a six-second lag, all
# without an error. A missing sidecar does the same to /connection_webrtc. The
# whole class of "we deployed it and the feature was quietly off" is what this
# block exists to stop, and it has already cost us once with
# voicemail_detection.
log "Preflight"
[ -n "$REXA_OUTBOUND_HMAC_SECRET" ] && [ -n "$REXA_INBOUND_HMAC_SECRET" ] \
  || die "$SECRETS has no HMAC secrets — the contract endpoints will not register at all"
if [ -n "$DAILY_API_KEY" ]; then
  printf '    daily            ON  (listen-in, browser calls, browser recording, instant barge)\n'
else
  printf '    daily            OFF (no listen-in, no browser calls, no browser recording) — set DAILY_API_KEY\n'
fi
if [ -x "$SIDECAR_PYTHON" ] && [ -f "$SIDECAR_SCRIPT" ]; then
  printf '    sidecar          ON  (%s)\n' "$SIDECAR_SCRIPT"
else
  printf '    sidecar          OFF — browser calls will be refused. Run deploy/scripts/sidecar-install.sh\n'
fi
for key in voicemail_detection voicemail_message dial_timeout_secs; do
  val=$(grep -E "^  ${key}:" config.yaml | head -1 | cut -d: -f2- | sed 's/^ *//;s/ *#.*//')
  case "$val" in
    ""|'""'|disabled)
      printf '    %-16s NOT SET in config.yaml — see deploy/config.yaml.example\n' "$key" ;;
    *)
      printf '    %-16s %s\n' "$key" "$(printf '%.60s' "$val")" ;;
  esac
done

ACHATBOT_SERVER_ADDR=":${PORT}" \
ACHATBOT_VAD_POOL_SIZE="$POOL" \
ACHATBOT_ASR_POOL_SIZE="$POOL" \
ACHATBOT_TTS_POOL_SIZE="$POOL" \
ACHATBOT_SERVER_MAX_GPU_CALLS="${MAX_GPU_CALLS:-$(cfg max_gpu_calls 61)}" \
ACHATBOT_SERVER_MAX_TOTAL_CALLS="${MAX_TOTAL_CALLS:-$(cfg max_total_calls 200)}" \
TELNYX_PUBLIC_URL="$PUBLIC" \
SIDECAR_PYTHON="$SIDECAR_PYTHON" \
SIDECAR_SCRIPT="$SIDECAR_SCRIPT" \
PREWARM_SCRIPT="$PREWARM_SCRIPT" \
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
