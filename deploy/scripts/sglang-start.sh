#!/bin/bash
# LLM server: SGLang, OpenAI-compatible endpoint. Host port 8001 (replica 1).
#
# The flags below are the tuned set — see deploy/llm/README.md for the measurements
# behind them (+34–51% throughput vs the stock config on the same GPU).
#
# Env overrides: LLM_GPU (default 0), LLM_MODEL, HF_CACHE, NAME, PORT.
set -euo pipefail
LLM_GPU="${LLM_GPU:-0}"
LLM_MODEL="${LLM_MODEL:-Qwen/Qwen2.5-3B-Instruct}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
NAME="${NAME:-sglang}"
PORT="${PORT:-8001}"
mkdir -p "$HF_CACHE"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$LLM_GPU" \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -p 127.0.0.1:"$PORT":8000 --shm-size=8g \
  lmsysorg/sglang:latest \
  python3 -m sglang.launch_server \
    --model-path "$LLM_MODEL" \
    --host 0.0.0.0 --port 8000 \
    --context-length 4096 \
    --mem-fraction-static 0.85 \
    --cuda-graph-max-bs 256 \
    --schedule-policy lpm

echo "$NAME started (GPU $LLM_GPU, host port $PORT, model $LLM_MODEL)."
echo "First run downloads the model into $HF_CACHE — watch: docker logs -f $NAME"
echo
echo "WARNING: do NOT lower --context-length below (system prompt + history + max_tokens)."
echo "  SGLang rejects every oversized request with HTTP 400, the LLM GPU sits at 0%, and"
echo "  calls stall at a flat ~7s. /v1/models still returns 200, so it looks healthy."
echo
echo "Scale out (one replica per GPU, then load-balance — see deploy/llm/README.md):"
echo "  NAME=sglang2 PORT=8002 LLM_GPU=1 $0"
echo "  cp deploy/llm/nginx-llm-lb.conf ~/lb/default.conf   # then point config.yaml at the LB"
