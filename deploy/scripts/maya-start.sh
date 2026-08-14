#!/bin/bash
# Maya1 expressive TTS service. Host port 8881, so it can run ALONGSIDE kokoro
# on 8880 rather than replacing it -- switching campaigns between the two is
# then a config change, and rolling back is instant.
#
# ONE WORKER, deliberately. Unlike parakeet and kokoro, concurrency here comes
# from vLLM's continuous batching inside a single process, not from uvicorn
# workers. A second worker would load a second 3B model into the same card and
# halve the KV cache for no throughput gain.
#
# Measured on one RTX 5090 (see docs/RESOURCES.md): 61 concurrent calls at
# batch 16, 214 at batch 64, 104ms to first audio at 61 concurrent.
#
# Env overrides: MAYA_GPU (default 3), PORT (8881), MAYA_VOICE (brisk_warm),
# MAYA_MAX_MODEL_LEN (2048), HF_CACHE, NAME.
set -euo pipefail
MAYA_GPU="${MAYA_GPU:-3}"
PORT="${PORT:-8881}"
NAME="${NAME:-maya-tts}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
IMAGE="${IMAGE:-vllm/vllm-openai:latest}"
SRC="${SRC:-$PWD/deploy/tts}"

mkdir -p "$HF_CACHE"

# Docker cannot read /tmp on this box (snap confinement), so the mount source
# must live under $HOME. Stage the service there rather than mounting the repo.
STAGE="$HOME/.maya-tts"
mkdir -p "$STAGE"
cp "$SRC/maya_server.py" "$STAGE/maya_server.py"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$MAYA_GPU" \
  --shm-size=8g \
  -e HF_HOME=/root/.cache/huggingface \
  -e MAYA_VOICE="${MAYA_VOICE:-brisk_warm}" \
  -e MAYA_MAX_MODEL_LEN="${MAYA_MAX_MODEL_LEN:-2048}" \
  -e MAYA_GPU_FRACTION="${MAYA_GPU_FRACTION:-0.80}" \
  -e PORT=8881 \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -v "$STAGE":/app \
  -w /app \
  -p 127.0.0.1:"$PORT":8881 \
  --entrypoint bash \
  "$IMAGE" -lc 'pip install -q snac >/dev/null 2>&1; python3 /app/maya_server.py'

echo "$NAME starting (GPU $MAYA_GPU, host port $PORT)."
echo "First run downloads Maya1 (~6 GB) and SNAC into $HF_CACHE."
echo "Startup also warms batch shapes 1-32, which takes a minute but avoids a"
echo "2.2s stall on the first live call at each new concurrency."
echo "Watch:  docker logs -f $NAME"
echo "Health: curl -s http://127.0.0.1:$PORT/health"
echo
echo "To use it, point the agent at it in config.yaml:"
echo "  tts:"
echo "    model: maya_http"
echo "    http_url: http://127.0.0.1:$PORT"
echo "    http_voice: brisk_warm   # brisk_warm | low_calm"
echo "Kokoro keeps running on 8880, so switching back is a config change."
