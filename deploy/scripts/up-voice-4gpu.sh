#!/bin/bash
# Bring up the ENTIRE voice agent on a 4x RTX 5090 box, from nothing to a
# ringing phone number. Idempotent: safe to re-run, it replaces containers it
# owns and leaves anything else on the box alone.
#
#   bash deploy/scripts/up-voice-4gpu.sh
#
# GPU layout (measured, see deploy/llm/README.md and deploy/loadtest/README.md):
#
#   GPU 0  SGLang replica 1   :8001  ─┐
#   GPU 1  SGLang replica 2   :8002  ─┴─ nginx least-conn :8011
#   GPU 2  Parakeet ASR       :8890
#   GPU 3  Kokoro TTS         :8880
#
# Two LLM replicas rather than one big model: the LLM is the binding constraint
# at scale, and two replicas of gemma-4-E4B measured 29.0 req/s at conc=60 —
# matching a 7B on one card while being materially better on the failure modes
# that actually happen on calls (inventing capabilities, latching onto
# mis-transcribed names).
#
# Capacity: this layout was load-tested to 61 concurrent agent legs with p50
# 1065ms / p95 2405ms and zero errors. Past that p95 roughly doubles while p50
# barely moves — the median stays fine while the tail falls apart, which a
# caller experiences as "it broke". Treat 61 as the ceiling, not a target.
#
# Env overrides: GPUS ("0 1 2 3"), HF_CACHE, SKIP_TUNNEL=1, SKIP_SERVER=1,
# REBUILD=1 (force image + binary rebuild).
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

GPUS="${GPUS:-0 1 2 3}"
read -r LLM_GPU_A LLM_GPU_B ASR_GPU_N TTS_GPU_N <<<"$GPUS"
export HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
mkdir -p "$HF_CACHE"

log()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die()  { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

# Wait for an explicit readiness signal, never a fixed sleep. A timeout here
# means something is genuinely wrong, so it prints the logs rather than
# continuing into a confusing downstream failure.
wait_ready() {
  local name="$1" url="$2" secs="${3:-600}" i=0
  printf '    waiting for %s ' "$name"
  while [ "$i" -lt "$secs" ]; do
    if curl -sf -m 3 "$url" >/dev/null 2>&1; then printf ' ready\n'; return 0; fi
    printf '.'; sleep 3; i=$((i + 3))
  done
  printf '\n'
  docker logs --tail 40 "$name" 2>&1 || true
  die "$name did not become ready within ${secs}s (last 40 log lines above)"
}

log "Preflight"
command -v docker >/dev/null || die "docker not installed"
docker info 2>/dev/null | grep -qi nvidia || die "nvidia container runtime not available"
have=$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)
[ "$have" -ge 4 ] || die "need 4 GPUs, found $have"
echo "    docker + nvidia runtime OK, $have GPUs visible"
echo "    layout: LLM $LLM_GPU_A,$LLM_GPU_B | ASR $ASR_GPU_N | TTS $TTS_GPU_N"

log "Building service images"
# --gpus does not work on this class of box; the start scripts already use
# --runtime=nvidia. Build context must be a readable dir, never /tmp (snap docker).
if [ "${REBUILD:-0}" = "1" ] || ! docker image inspect kokoro-gpu:local >/dev/null 2>&1; then
  docker build -t kokoro-gpu:local deploy/tts
else echo "    kokoro-gpu:local present"; fi
if [ "${REBUILD:-0}" = "1" ] || ! docker image inspect parakeet-gpu:local >/dev/null 2>&1; then
  docker build -t parakeet-gpu:local deploy/asr
else echo "    parakeet-gpu:local present"; fi

log "LLM replica 1 (GPU $LLM_GPU_A) -> :8001"
NAME=sglang  PORT=8001 LLM_GPU="$LLM_GPU_A" bash deploy/scripts/sglang-start.sh >/dev/null
log "LLM replica 2 (GPU $LLM_GPU_B) -> :8002"
NAME=sglang2 PORT=8002 LLM_GPU="$LLM_GPU_B" bash deploy/scripts/sglang-start.sh >/dev/null

log "ASR Parakeet (GPU $ASR_GPU_N) -> :8890"
ASR_GPU="$ASR_GPU_N" bash deploy/scripts/parakeet-start.sh >/dev/null
log "TTS Kokoro (GPU $TTS_GPU_N) -> :8880"
TTS_GPU="$TTS_GPU_N" bash deploy/scripts/kokoro-start.sh >/dev/null

log "LLM load balancer -> :8011"
mkdir -p ~/lb && cp deploy/llm/nginx-llm-lb.conf ~/lb/default.conf
docker rm -f llm-lb >/dev/null 2>&1 || true
docker run -d --name llm-lb --restart unless-stopped --network host \
  -v ~/lb:/etc/nginx/conf.d:ro nginx:alpine >/dev/null
echo "    llm-lb started"

log "Waiting for GPU services"
# First run downloads weights (LLM several GB, ASR ~2.5GB) — slow, not hung.
wait_ready sglang   http://127.0.0.1:8001/v1/models 1800
wait_ready sglang2  http://127.0.0.1:8002/v1/models 1800
wait_ready parakeet http://127.0.0.1:8890/health     900
wait_ready kokoro-tts http://127.0.0.1:8880/health   900

log "Config"
if [ ! -f config.yaml ]; then
  cp deploy/config.yaml.example config.yaml
  # Point the agent at the load balancer, not one replica, and size the pools
  # to the tested ceiling. pool_size below max concurrent calls starves sessions.
  sed -i 's#^  base_url: .*#  base_url: "http://127.0.0.1:8011/v1"#' config.yaml
  sed -i 's#^  model: "RedHatAI.*#  model: "google/gemma-4-E4B-it"#' config.yaml
  sed -i 's#^  pool_size: 64#  pool_size: 240#g' config.yaml
  echo "    wrote config.yaml (LB endpoint, pool_size 240)"
else
  echo "    config.yaml exists — left untouched"
fi

if [ "${SKIP_TUNNEL:-0}" != "1" ]; then
  log "Public tunnel"
  bash deploy/scripts/tunnel-start.sh
fi

if [ "${SKIP_SERVER:-0}" != "1" ]; then
  log "Go voice server -> :4321"
  bash deploy/scripts/server-start.sh
  wait_ready "voice server" http://127.0.0.1:4321/api/options 300
fi

log "Up"
printf '  %-14s %s\n' LLM "http://127.0.0.1:8011/v1  (replicas :8001 :8002)"
printf '  %-14s %s\n' ASR "http://127.0.0.1:8890/health"
printf '  %-14s %s\n' TTS "http://127.0.0.1:8880/health"
printf '  %-14s %s\n' UI  "http://127.0.0.1:4321"
[ -f tunnel-url.txt ] && printf '  %-14s %s\n' public "$(cat tunnel-url.txt)"
echo
echo "Load test:  python3 deploy/loadtest/ttsbench.py 30 90   (and asrbench.py)"
echo "Teardown:   docker rm -f sglang sglang2 parakeet kokoro-tts llm-lb"
