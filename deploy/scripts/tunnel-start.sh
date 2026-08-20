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
# deps-install.sh drops it in $HOME when the system has none.
command -v "$CF" >/dev/null 2>&1 || { [ -x "$HOME/cloudflared" ] && CF="$HOME/cloudflared"; }

pkill -f "cloudflared tunnel" 2>/dev/null || true
sleep 1
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
