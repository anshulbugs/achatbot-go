#!/bin/bash
# LLM server: SGLang, OpenAI-compatible endpoint. Host port 8001 (replica 1).
#
# The flags below are the tuned set — see deploy/llm/README.md for the measurements
# behind them (+34–51% throughput vs the stock config on the same GPU).
#
# Default model is gemma-4-E4B-it (Apache-2.0): 2.3B effective / 5.1B raw via Per-Layer
# Embeddings. Chosen on measurement, not size. Against Qwen2.5-7B-FP8 on identical warm
# benchmarks it matched throughput (29.0 vs 29.8 req/s at conc=60, two replicas) while
# being clearly better on the failures that actually occurred on calls:
#
#   - asked to look up an account balance, the 7B answered "Sure thing! I'll need your
#     account number" -- inventing a capability it does not have. E4B declines and
#     redirects. That one matters on a customer line.
#   - given a bare mis-transcribed name ("Riley" heard as "Valley"), the 7B started
#     addressing the caller by it in 5 of 6 samples. E4B: 0 of 6.
#
# Do not assume smaller is worse here. Qwen2.5-3B failed this instruction set outright,
# and E4B is smaller still, but a generation newer -- it holds the rules the 3B could not.
#
# Models rejected after benchmarking, with reasons, so they are not retried:
#   Qwen3.6-35B-A3B-FP8  5.4 req/s at conc=60 on TWO gpus. Its hybrid attention needs a
#                        5.36GB SSM state cache per gpu which starves the KV cache
#                        (max_running_requests capped at 32), and TP=2 has no P2P between
#                        consumer RTX cards so every all-reduce crosses host memory.
#   Qwen3-8B-FP8         15% less throughput than the 7B and no quality gain; 2.6x the KV
#                        per token (36 layers x 8 kv heads vs 28 x 4).
#
# Env overrides: LLM_GPU (default 0), LLM_MODEL, HF_CACHE, NAME, PORT.
set -euo pipefail
LLM_GPU="${LLM_GPU:-0}"
LLM_MODEL="${LLM_MODEL:-google/gemma-4-E4B-it}"
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
NAME="${NAME:-sglang}"
PORT="${PORT:-8001}"
mkdir -p "$HF_CACHE"

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$LLM_GPU" \
  -e HF_TOKEN="${HF_TOKEN:-}" \
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
