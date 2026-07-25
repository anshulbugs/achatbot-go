#!/bin/bash
# LLM server: SGLang, OpenAI-compatible endpoint on host port 8001.
# Env overrides: LLM_GPU (default 0), LLM_MODEL, HF_CACHE dir.
set -euo pipefail
LLM_GPU="${LLM_GPU:-0}"
LLM_MODEL="${LLM_MODEL:-Qwen/Qwen2.5-3B-Instruct}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
mkdir -p "$HF_CACHE"

docker rm -f sglang 2>/dev/null || true
docker run -d --name sglang \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$LLM_GPU" \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -p 127.0.0.1:8001:8000 --shm-size=8g \
  lmsysorg/sglang:latest \
  python3 -m sglang.launch_server \
    --model-path "$LLM_MODEL" \
    --host 0.0.0.0 --port 8000 \
    --attention-backend triton \
    --context-length 4096 --mem-fraction-static 0.6

echo "sglang started (GPU $LLM_GPU, host port 8001, model $LLM_MODEL)."
echo "First run downloads the model into $HF_CACHE — watch: docker logs -f sglang"
# For 2 LLM GPUs: add --tp 2 and set NVIDIA_VISIBLE_DEVICES=0,1 (tensor parallel),
# or run a second container on another GPU + a load balancer (e.g. nginx) on 8001.
