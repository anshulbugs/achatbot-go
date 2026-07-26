#!/bin/bash
# TTS server: Kokoro GPU container (built from deploy/tts). Host port 8880.
#
# TTS_WORKERS is the most important setting here. The app is a sync FastAPI endpoint, so
# ONE worker serialises every request: measured 8 req/s flat regardless of client
# concurrency, with p50 climbing 61ms -> 5252ms at 50 concurrent callers. Eight workers
# took it to 25.7 req/s. See deploy/loadtest/README.md.
#
# Env overrides: TTS_GPU (default 1), TTS_WORKERS (default 8), NAME, PORT.
set -euo pipefail
TTS_GPU="${TTS_GPU:-1}"
TTS_WORKERS="${TTS_WORKERS:-8}"
NAME="${NAME:-kokoro-tts}"
PORT="${PORT:-8880}"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$TTS_GPU" \
  --shm-size=8g \
  -p 127.0.0.1:"$PORT":8880 \
  -w /app \
  kokoro-gpu:local \
  uvicorn tts_server:app --host 0.0.0.0 --port 8880 --workers "$TTS_WORKERS"

echo "$NAME started (GPU $TTS_GPU, host port $PORT, $TTS_WORKERS workers)."
echo "Health: curl -s http://127.0.0.1:$PORT/health"
echo "Each worker loads its own copy of the model into VRAM — size workers to fit the card."
echo "Benchmark: python3 deploy/loadtest/ttsbench.py 50 150"
