#!/bin/bash
# ASR server: Parakeet GPU container (built from deploy/asr). Host port 8890.
#
# ASR_WORKERS matters as much here as it does for TTS. asr_server.py declares the endpoint
# `async def` but calls model.transcribe() synchronously inside it, which blocks the event
# loop — one worker therefore handles exactly one request at a time: measured 14 req/s flat,
# p50 63ms -> 1810ms at 30 concurrent callers. Four workers took it to ~26–36 req/s.
#
# Four is the measured optimum on one card: EIGHT workers was *worse* (20 req/s) because the
# copies contend. Re-measure if you change GPU or model. See deploy/loadtest/README.md.
#
# Env overrides: ASR_GPU (default 2), ASR_WORKERS (default 4), HF_CACHE, NAME, PORT.
set -euo pipefail
ASR_GPU="${ASR_GPU:-2}"
ASR_WORKERS="${ASR_WORKERS:-4}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
NAME="${NAME:-parakeet}"
PORT="${PORT:-8890}"
mkdir -p "$HF_CACHE"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$ASR_GPU" \
  --shm-size=8g \
  -e HF_TOKEN="${HF_TOKEN:-}" \
  -e ASR_MODEL="${ASR_MODEL:-nvidia/parakeet-tdt-0.6b-v2}" \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -p 127.0.0.1:"$PORT":8890 \
  -w /app \
  parakeet-gpu:local \
  uvicorn asr_server:app --host 0.0.0.0 --port 8890 --workers "$ASR_WORKERS"

echo "$NAME started (GPU $ASR_GPU, host port $PORT, $ASR_WORKERS workers)."
echo "First run downloads the model (~2.5 GB) into $HF_CACHE — watch: docker logs -f $NAME"
echo "Health: curl -s http://127.0.0.1:$PORT/health"
echo "Each worker loads its own copy of the model into VRAM — size workers to fit the card."
echo "Benchmark: python3 deploy/loadtest/asrbench.py 30 90"
