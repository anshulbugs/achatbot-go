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
# Context window. MUST exceed system prompt + retained history + max_tokens.
#
# 4096 is far too small for this deployment: the configured system prompt alone
# is ~3.1k tokens and chat_history_size 12 retains 26 messages on top. Exceed it
# and SGLang answers HTTP 400 for EVERY request while /v1/models still returns
# 200 -- the LLM GPU sits at 0% and calls stall at a flat ~7s, looking like a
# hang rather than a rejection. Raise it whenever the prompt grows.
CONTEXT_LENGTH="${CONTEXT_LENGTH:-8192}"
# Share of the device SGLang reserves up front. 0.85 is right when the LLM
# owns a card, which it did on the 4x 5090 box. On a single-GPU layout ASR
# and TTS sit on the same device and need what is left, so
# up-voice-gh200.sh lowers this.
MEM_FRACTION="${MEM_FRACTION:-0.85}"

# gemma-4 is a vision-language model, and on aarch64 its VISION tower is what
# crashes the scheduler: sgl_kernel there has no flash_ops, so the first real
# request dies in gemma4_vision.py with "Can not import FA3 in sgl_kernel".
# The HTTP frontend survives, so the symptom is every request hanging until it
# times out rather than an obvious crash.
#
# The fix is to point the tower's attention at a kernel that exists everywhere.
# sdpa is torch's own scaled_dot_product_attention: no compiled extension, so it
# is present on any build. The tower still loads, and since we never send an
# image it never actually runs a batch — this only has to stop the import.
#
# --language-only would be tidier and is what a voice agent really wants, but
# SGLang REJECTS it for this model: it routes through encoder disaggregation,
# which supports only Qwen2VL, Qwen3VL, Qwen3.5, InternS2, Qwen2Audio,
# Qwen2.5Omni, Kimi and MiMoV2 — not Gemma4ForConditionalGeneration. Left as an
# opt-in for whenever that list grows.
EXTRA_ARGS=(--mm-attention-backend "${MM_ATTENTION_BACKEND:-sdpa}")
[ "${LANGUAGE_ONLY:-0}" = "1" ] && EXTRA_ARGS+=(--language-only)
HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
NAME="${NAME:-sglang}"
PORT="${PORT:-8001}"
mkdir -p "$HF_CACHE"

# --runtime=nvidia is not universal: this box may expose GPUs through CDI
# instead, and passing the wrong one fails the container outright.
source "$(dirname "$0")/gpu-flags.sh"
gpu_docker_flags "$LLM_GPU" || exit 1

docker rm -f "$NAME" 2>/dev/null || true
docker run -d --name "$NAME" --restart unless-stopped \
  "${GPU_FLAGS[@]}" \
  -e HF_TOKEN="${HF_TOKEN:-}" \
  -v "$HF_CACHE":/root/.cache/huggingface \
  -p 127.0.0.1:"$PORT":8000 --shm-size=8g \
  lmsysorg/sglang:latest \
  python3 -m sglang.launch_server \
    --model-path "$LLM_MODEL" \
    --host 0.0.0.0 --port 8000 \
    --context-length "$CONTEXT_LENGTH" \
    --mem-fraction-static "$MEM_FRACTION" \
    --cuda-graph-max-bs 256 \
    --schedule-policy lpm \
    --enable-metrics \
    --tool-call-parser gemma4 \
    "${EXTRA_ARGS[@]}"

echo "$NAME started (GPU $LLM_GPU, host port $PORT, model $LLM_MODEL)."
echo "First run downloads the model into $HF_CACHE — watch: docker logs -f $NAME"
echo
# --tool-call-parser is REQUIRED for call transfer to work at all.
#
# Without it SGLang returns the model's tool call as raw text in `content` and
# leaves `tool_calls` null. The agent reads tool_calls, so the transfer never
# fires -- and the raw markup ("<|tool_call>call:call_transfer{...}") flows on
# to TTS and is SPOKEN ALOUD to the caller. Verified against gemma-4-E4B-it:
# the model gets the decision right and the plumbing silently drops it.
#
# The parser must match the model family -- `auto` detects from the chat
# template, `gemma4` is explicit and is what this deployment runs. Change the
# model and this flag has to change with it.
#
# --enable-metrics exposes /metrics (prefix-cache hit rate, queue depth, KV pool
# usage). The agent polls it for the dashboard; without the flag the endpoint
# 404s while /v1/models keeps answering 200, so the panel simply reads "not
# polling" and nothing else breaks. Nothing gates traffic on these numbers --
# they are there to say WHY latency moved: a falling hit rate means prompts have
# stopped sharing prefixes, a growing queue means there are simply too many.
echo "WARNING: do NOT lower --context-length below (system prompt + history + max_tokens)."
echo "  SGLang rejects every oversized request with HTTP 400, the LLM GPU sits at 0%, and"
echo "  calls stall at a flat ~7s. /v1/models still returns 200, so it looks healthy."
echo
echo "Scale out (one replica per GPU, then load-balance — see deploy/llm/README.md):"
echo "  NAME=sglang2 PORT=8002 LLM_GPU=1 $0"
echo "  cp deploy/llm/nginx-llm-lb.conf ~/lb/default.conf   # then point config.yaml at the LB"
