#!/bin/bash
# TTS server: Supertonic-3 GPU container (built from deploy/tts/supertonic).
# Host port 8881, so it can run alongside Kokoro on 8880 during a comparison.
#
# INSTANCES is the concurrency knob, NOT uvicorn workers -- the opposite of
# kokoro-start.sh, so do not copy that pattern here.
#
# Measured over HTTP on this box at 8 concurrent callers:
#   1 uvicorn worker,  1 instance    p50 433ms   20.3 req/s    5.1GB
#   2 uvicorn workers, 1 instance    p50 205ms   28.2 req/s    6.6GB
#   8 uvicorn workers, 1 instance    p50 192ms   31.6 req/s   16.5GB
#   1 uvicorn worker,  4 instances   p50 402ms   22.3 req/s    8.7GB  <- default
#
# Do not read the small differences as a ranking; the service plateaus around
# 25-32 req/s whatever the layout, because the ceiling is Python-side (GIL,
# response serialisation), not the GPU. The instance pool is the default because
# it reaches that plateau on roughly half the VRAM of 8 workers and keeps one
# CUDA context.
#
# Sanity check on the numbers: an in-process benchmark with no HTTP reached 56
# req/s on the same GPU. That figure does NOT survive the web layer and must not
# be quoted as service throughput.
#
# The pool must stay bounded: ONNX Runtime's CUDA provider allocates a cuBLAS
# handle per thread and enough of them fails with "CUBLAS failure 3". Seen at
# 61 threads, safe at 8.
#
# Each instance is ~1.7GB VRAM. 61 concurrent agents need roughly 25
# audio-sec/sec; at ~4.9s of audio per request, 22 req/s is ~108 audio-sec/sec,
# so 4 instances is about 4x headroom.
#
# Env overrides: TTS_GPU (default 7), INSTANCES (default 4), STEPS (default 8),
# RATE (default 24000), NAME, PORT.
set -euo pipefail
TTS_GPU="${TTS_GPU:-7}"
INSTANCES="${INSTANCES:-4}"
STEPS="${STEPS:-8}"
RATE="${RATE:-24000}"
NAME="${NAME:-supertonic-tts}"
PORT="${PORT:-8881}"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$TTS_GPU" \
  --shm-size=8g \
  -e SUPERTONIC_STEPS="$STEPS" \
  -e SUPERTONIC_RATE="$RATE" \
  -e SUPERTONIC_INSTANCES="$INSTANCES" \
  -e SUPERTONIC_REQUIRE_CUDA=1 \
  -p 127.0.0.1:"$PORT":8881 \
  -w /app \
  supertonic-gpu:local \
  uvicorn server:app --host 0.0.0.0 --port 8881 --workers 1

echo "$NAME started (GPU $TTS_GPU, host port $PORT, $INSTANCES instances, steps $STEPS, rate $RATE)."
echo "Health: curl -s http://127.0.0.1:$PORT/health"
echo "Verify providers are CUDA, not CPU -- the container refuses to start on CPU by design."
echo "Benchmark: python3 deploy/loadtest/ttsbench.py 50 150   (point it at $PORT)"
