#!/bin/bash
# Public ingress via a Cloudflare quick tunnel (ephemeral URL, changes on
# restart). Writes the assigned URL to tunnel-url.txt so server-start.sh can
# hand it to Telnyx as the webhook base. Requires the `cloudflared` binary on
# PATH or in the current directory.
#
# For a STABLE URL (recommended for sharing / production), use a NAMED tunnel:
#   cloudflared tunnel login
#   cloudflared tunnel create voice-agent
#   cloudflared tunnel route dns voice-agent demo.yourdomain.com
#   cloudflared tunnel run --url http://127.0.0.1:4321 voice-agent
set -euo pipefail
PORT="${SERVER_PORT:-4321}"
CF="${CLOUDFLARED:-cloudflared}"

pkill -f "cloudflared tunnel" 2>/dev/null || true
sleep 1
nohup setsid "$CF" tunnel --url "http://127.0.0.1:${PORT}" --no-autoupdate \
  > cloudflared.log 2>&1 < /dev/null &
disown

url=""
for _ in $(seq 1 30); do
  url=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' cloudflared.log | head -1 || true)
  [ -n "$url" ] && break
  sleep 1
done
echo "$url" > tunnel-url.txt
echo "cloudflared tunnel: ${url:-<not ready — check cloudflared.log>}"
