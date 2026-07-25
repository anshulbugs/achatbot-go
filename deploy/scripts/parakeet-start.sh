#!/bin/bash
# ASR server: Parakeet GPU container (built from deploy/asr). Host port 8890.
# Env overrides: ASR_GPU (default 2), HF_CACHE (persists the model download).
set -euo pipefail
ASR_GPU="${ASR_GPU:-2}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
mkdir -p "$HF_CACHE"

docker rm -f parakeet 2>/dev/null || true
docker run -d --name parakeet --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$ASR_GPU" \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -p 127.0.0.1:8890:8890 \
  parakeet-gpu:local

echo "parakeet started (GPU $ASR_GPU, host port 8890)."
echo "First run downloads the model (~2.5 GB) into $HF_CACHE — watch: docker logs -f parakeet"
echo "Health: curl -s http://127.0.0.1:8890/health"
