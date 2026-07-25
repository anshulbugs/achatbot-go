#!/bin/bash
# Build (if needed) and start the Go voice-agent server. Run from the repo root
# after the GPU services (SGLang, Kokoro, Parakeet) and the tunnel are up.
#
# Loads telnyx.env (optional — telephony is disabled without it) and picks up
# the public URL from tunnel-url.txt (written by tunnel-start.sh) so Telnyx
# webhooks resolve.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

# Telephony env (TELNYX_API_KEY / TELNYX_APP_ID / TELNYX_FROM_NUMBER)
if [ -f telnyx.env ]; then
  set -a; . ./telnyx.env; set +a
fi
# Public URL for Telnyx webhooks (from the tunnel)
if [ -f tunnel-url.txt ]; then
  export TELNYX_PUBLIC_URL="$(cat tunnel-url.txt)"
fi

# Build the server binary (CGO: links sherpa-onnx for VAD; needs a C toolchain).
if [ ! -x ./server-bin ] || [ "${REBUILD:-0}" = "1" ]; then
  echo "building server-bin ..."
  go build -o server-bin ./examples/websocket/
fi

# Raise the open-file limit so many concurrent WebSocket calls fit.
ulimit -n 65535 2>/dev/null || true

pkill -f "server-bin -config" 2>/dev/null || true
sleep 1
nohup setsid ./server-bin -config config.yaml >> server-run.log 2>&1 < /dev/null &
disown
echo "server started pid $! public=${TELNYX_PUBLIC_URL:-<none>} log=server-run.log"
