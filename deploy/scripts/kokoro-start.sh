#!/bin/bash
# TTS server: Kokoro GPU container (built from deploy/tts). Host port 8880.
# Env overrides: TTS_GPU (default 1).
set -euo pipefail
TTS_GPU="${TTS_GPU:-1}"

docker rm -f kokoro-tts 2>/dev/null || true
docker run -d --name kokoro-tts --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$TTS_GPU" \
  -p 127.0.0.1:8880:8880 \
  kokoro-gpu:local

echo "kokoro-tts started (GPU $TTS_GPU, host port 8880)."
echo "Health: curl -s http://127.0.0.1:8880/health"
