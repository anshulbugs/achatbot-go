#!/bin/bash
# Bring up the ENTIRE voice agent on a 1x GH200 (96 GB) box.
#
#   bash deploy/scripts/up-voice-gh200.sh
#
# This is the GH200 counterpart to up-voice-4gpu.sh. Read that one first — it
# is the reference, and everything here is a deviation from it with a reason.
#
# WHAT IS DIFFERENT, AND WHY
#
#   ONE GPU, NOT FOUR. The 5090 box gave each service a card of its own. Here
#   all four share 96 GB of HBM3e, so the LLM can no longer take 85% of the
#   device: --mem-fraction-static is lowered to leave room for ASR and TTS.
#   That number is a STARTING POINT, not a measurement. Watch nvidia-smi under
#   load and move it.
#
#   ONE LLM REPLICA, NO LOAD BALANCER. Two replicas existed because the LLM was
#   the binding constraint and there were two spare cards. With one card there
#   is nothing to balance, so nginx is skipped and the agent points straight at
#   :8001. The prefix-routing config in deploy/llm/ is therefore inert here;
#   its 27% gain came from steering campaigns to a consistent replica, which
#   needs more than one replica to mean anything.
#
#   ARM64. The kokoro and parakeet images on this branch build from
#   nvidia/cuda rather than pytorch/pytorch, which has no arm64 manifest at
#   all. Expect the first build to be slow: some of NeMo's dependency tree has
#   no aarch64 wheels and compiles from source.
#
# WHAT IS NOT KNOWN YET
#
#   CAPACITY. 61 concurrent legs was measured on 4x RTX 5090 and does NOT carry
#   over — different card count, different architecture, different memory
#   layout. Leave max_gpu_calls low until deploy/loadtest has been run here, and
#   set it from that. Guessing high looks fine at p50 and falls apart at p95,
#   which a caller experiences as "it broke".
#
# Env overrides: GPU (default 0), HF_CACHE, LLM_MEM_FRACTION, SKIP_DEPS=1,
# SKIP_TUNNEL=1, SKIP_SERVER=1, SKIP_SENTIMENT=1, SKIP_SIDECAR=1, REBUILD=1.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

GPU="${GPU:-0}"
export HF_CACHE="${HF_CACHE:-$HOME/hf-cache}"
# 0.75 leaves roughly 24 GB for parakeet (~2.5 GB), kokoro (~0.3 GB), their CUDA
# contexts and headroom for KV growth. Deliberately conservative: an LLM that
# cannot allocate fails loudly at startup, while an ASR that cannot allocate
# fails on a live call.
LLM_MEM_FRACTION="${LLM_MEM_FRACTION:-0.75}"
mkdir -p "$HF_CACHE"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

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
if [ "${SKIP_DEPS:-0}" != "1" ]; then
  bash deploy/scripts/deps-install.sh || die "dependencies missing (see above)"
fi
command -v docker >/dev/null || die "docker not installed"
docker info 2>/dev/null | grep -qi nvidia || die "nvidia container runtime not available"

arch="$(uname -m)"
[ "$arch" = "aarch64" ] || echo "    NOTE: this script is for Grace (aarch64); running on $arch"
have=$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)
[ "$have" -ge 1 ] || die "no GPU visible"
echo "    $arch, $have GPU(s): $(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader | head -1)"
echo "    everything shares GPU $GPU; LLM takes $LLM_MEM_FRACTION of it"

log "Building service images (arm64 — first build is slow)"
if [ "${REBUILD:-0}" = "1" ] || ! docker image inspect kokoro-gpu:local >/dev/null 2>&1; then
  docker build -t kokoro-gpu:local deploy/tts
else echo "    kokoro-gpu:local present"; fi
if [ "${REBUILD:-0}" = "1" ] || ! docker image inspect parakeet-gpu:local >/dev/null 2>&1; then
  docker build -t parakeet-gpu:local deploy/asr
else echo "    parakeet-gpu:local present"; fi

log "LLM (GPU $GPU) -> :8001"
NAME=sglang PORT=8001 LLM_GPU="$GPU" MEM_FRACTION="$LLM_MEM_FRACTION" \
  bash deploy/scripts/sglang-start.sh >/dev/null

log "ASR Parakeet (GPU $GPU) -> :8890"
ASR_GPU="$GPU" bash deploy/scripts/parakeet-start.sh >/dev/null
log "TTS Kokoro (GPU $GPU) -> :8880"
TTS_GPU="$GPU" bash deploy/scripts/kokoro-start.sh >/dev/null

log "Waiting for GPU services"
wait_ready sglang     http://127.0.0.1:8001/v1/models 1800
wait_ready parakeet   http://127.0.0.1:8890/health     900
wait_ready kokoro-tts http://127.0.0.1:8880/health     900

log "Config"
if [ ! -f config.yaml ]; then
  cp deploy/config.yaml.example config.yaml
  # Straight at the single replica: there is no load balancer on this layout.
  sed -i 's#^  base_url: .*#  base_url: "http://127.0.0.1:8001/v1"#' config.yaml
  sed -i 's#^  model: "RedHatAI.*#  model: "google/gemma-4-E4B-it"#' config.yaml
  # max_gpu_calls starts LOW on purpose. 61 belonged to the 5090 box. Raise it
  # from a loadtest run on THIS hardware, not from the old number.
  sed -i 's#^  max_gpu_calls: .*#  max_gpu_calls: 20#' config.yaml
  sed -i 's#^  pool_size: 64#  pool_size: 80#g' config.yaml
  sed -i 's#^  sentiment_base_url: .*#  sentiment_base_url: "http://127.0.0.1:11435/v1"#' config.yaml
  sed -i 's#^  sentiment_model: .*#  sentiment_model: "sentiment-cpu"#' config.yaml
  echo "    wrote config.yaml (single LLM, max_gpu_calls 20 pending a loadtest)"
else
  echo "    config.yaml exists — left untouched"
fi

if [ "${SKIP_SENTIMENT:-0}" != "1" ]; then
  log "Sentiment classifier (CPU) -> :11435"
  # Grace has 72 ARM cores, so the CPU classifier has more headroom here than
  # it did on the 5090 box, not less. Still off the caller's critical path.
  if bash deploy/scripts/sentiment-start.sh >/dev/null 2>&1; then
    echo "    sentiment-cpu ready on :11435"
  else
    echo "    SKIPPED - ollama not available (check it has an arm64 build)"
  fi
fi

if [ "${SKIP_SIDECAR:-0}" != "1" ]; then
  log "Daily room sidecar (browser calls)"
  if bash deploy/scripts/sidecar-install.sh >/dev/null 2>&1; then
    echo "    sidecar venv ready"
  else
    echo "    SKIPPED - see deploy/scripts/sidecar-install.sh"
  fi
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
printf '  %-14s %s\n' LLM "http://127.0.0.1:8001/v1  (single replica)"
printf '  %-14s %s\n' ASR "http://127.0.0.1:8890/health"
printf '  %-14s %s\n' TTS "http://127.0.0.1:8880/health"
printf '  %-14s %s\n' UI  "http://127.0.0.1:4321"
[ -f tunnel-url.txt ] && printf '  %-14s %s\n' public "$(cat tunnel-url.txt)"
echo
echo "BEFORE ANY REAL CAMPAIGN, in this order:"
echo "  1. python3 deploy/scripts/verify-speech-markup.py   # speech markup + pauses"
echo "  2. python3 deploy/loadtest/ttsbench.py 30 90        # then asrbench.py"
echo "  3. set server.max_gpu_calls from what step 2 shows, and restart"
echo
echo "Teardown:   docker rm -f sglang parakeet kokoro-tts"

if [ -f rexa-secrets.env ]; then
  log "Platform-contract instance -> :4399"
  bash deploy/scripts/contract-start.sh
else
  echo
  echo "Platform-contract instance (:4399, HMAC endpoints, live rooms, WebRTC):"
  echo "  1. cp deploy/rexa-secrets.env.example rexa-secrets.env  # then fill it in"
  echo "  2. bash deploy/scripts/contract-start.sh"
fi
