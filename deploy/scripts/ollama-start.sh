#!/bin/bash
# OPTIONAL alternative LLM backend: Ollama (instead of SGLang). Only needed if
# you set llm.provider/base_url to point at Ollama. SGLang is the default and
# is faster under concurrency. Kept here because these env vars were required to
# get Ollama stable on Blackwell.
set -euo pipefail
LLM_GPU="${LLM_GPU:-0}"
export OLLAMA_MODELS="${OLLAMA_MODELS:-$HOME/ollama-models}"
export CUDA_VISIBLE_DEVICES="$LLM_GPU"
export OLLAMA_KEEP_ALIVE=-1          # keep model resident (avoid cold-start reloads)
export OLLAMA_MAX_LOADED_MODELS=3    # stop model thrashing that caused ~5.7s/turn
export OLLAMA_NUM_PARALLEL=2
export OLLAMA_CONTEXT_LENGTH=4096
export OLLAMA_LLM_LIBRARY=cuda_v13   # avoid the Vulkan path (hangs on Blackwell)
export OLLAMA_VULKAN=0

pkill -f "ollama serve" 2>/dev/null || true
sleep 1
nohup setsid ollama serve >> ollama.log 2>&1 < /dev/null &
disown
echo "ollama started pid $! (GPU $LLM_GPU)"
