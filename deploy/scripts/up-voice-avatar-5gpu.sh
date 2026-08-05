#!/bin/bash
# Bring up the voice agent PLUS the photoreal talking-head avatar on a
# 5x RTX 5090 box. Layers the avatar on top of up-voice-4gpu.sh rather than
# duplicating it, so the voice path stays defined in exactly one place.
#
#   bash deploy/scripts/up-voice-avatar-5gpu.sh
#
#   GPU 0  SGLang replica 1   :8001  ─┐
#   GPU 1  SGLang replica 2   :8002  ─┴─ nginx least-conn :8011
#   GPU 2  Parakeet ASR       :8890
#   GPU 3  Kokoro TTS         :8880
#   GPU 4  SoulX-FlashHead    :8899   <- the avatar, this script's addition
#
# The avatar needs NO change to the Go pipeline: the browser forwards the bot
# audio it already receives, and this service republishes synchronized audio +
# video over WebRTC via Daily. That is why it can be bolted on as a fifth card.
#
# Know what you are buying: ~4 concurrent streams per GPU and ~1.4s added
# latency. This is a demo / premium path, not the mass phone-call path — the
# voice-only layout carries 61 concurrent legs on four cards, this carries four.
# Do not size a phone campaign against it.
#
# Prerequisites this script CANNOT do for you (both need your credentials):
#   ~/SoulX-FlashHead/daily.env          Daily API key, chmod 600
#   ~/SoulX-FlashHead/avatar_current.jpg the face to animate
# Source photo quality dominates output quality — sharp, front-facing, evenly
# lit, clean background. A still grabbed from video gives soft, mediocre output.
#
# Env overrides: GPUS ("0 1 2 3"), AVATAR_GPU (default 4), SOULX_DIR, plus
# everything up-voice-4gpu.sh accepts.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

AVATAR_GPU="${AVATAR_GPU:-4}"
SOULX_DIR="${SOULX_DIR:-$HOME/SoulX-FlashHead}"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

have=$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)
[ "$have" -ge 5 ] || die "need 5 GPUs, found $have (use up-voice-4gpu.sh instead)"

# Fail on missing avatar prerequisites BEFORE spending ten minutes bringing the
# voice stack up, rather than after.
log "Avatar preflight"
[ -d "$SOULX_DIR" ] || die "$SOULX_DIR not found — follow deploy/avatar/README.md §2.1 to fetch weights"
[ -f "$SOULX_DIR/daily.env" ] || die "$SOULX_DIR/daily.env missing (see deploy/avatar/daily.env.example)"
[ -f "$SOULX_DIR/avatar_current.jpg" ] || die "$SOULX_DIR/avatar_current.jpg missing — the face to animate"
[ -d "$SOULX_DIR/models/SoulX-FlashHead-1_3B" ] || die "avatar weights missing under $SOULX_DIR/models"
echo "    weights, Daily creds and avatar image present"

log "Voice stack (4 GPUs)"
bash deploy/scripts/up-voice-4gpu.sh

log "Avatar SoulX-FlashHead (GPU $AVATAR_GPU) -> :8899"
cp deploy/avatar/avatar_daily.py deploy/avatar/idle_gen.py "$SOULX_DIR/"

if ! docker inspect soulx >/dev/null 2>&1; then
  # Stock pytorch image already has Python 3.11 + CUDA 12.8, so no build needed.
  # 8899 must be PUBLISHED — it serves the control WebSocket the browser uses.
  docker run -d --name soulx --restart unless-stopped -p 8899:8899 \
    --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="$AVATAR_GPU" \
    -v "$SOULX_DIR":/workspace/SoulX-FlashHead \
    -w /workspace/SoulX-FlashHead \
    pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime sleep infinity >/dev/null
  echo "    container created — installing dependencies (several minutes)"
  docker exec soulx bash -c '
    set -e
    cd /workspace/SoulX-FlashHead
    pip install --no-cache-dir torch==2.7.1 torchvision==0.22.1 --index-url https://download.pytorch.org/whl/cu128
    # SoulX pins nvidia-nccl-cu12==2.27.3, which conflicts with torch 2.7.1 (wants
    # 2.26.2). It is only used for multi-GPU, so drop the pin for single-GPU.
    grep -v "nvidia-nccl-cu12" requirements.txt > requirements_fixed.txt
    pip install --no-cache-dir -r requirements_fixed.txt
    pip install --no-cache-dir "https://github.com/Dao-AILab/flash-attention/releases/download/v2.8.0.post2/flash_attn-2.8.0.post2+cu12torch2.7cxx11abiFALSE-cp311-cp311-linux_x86_64.whl"
    pip install --no-cache-dir daily-python aiohttp requests websockets
    # The runtime image lacks these: opencv/mediapipe need GUI libs, Triton JIT needs cc.
    apt-get update -qq && apt-get install -y -qq libgl1 libxcb1 libxrender1 libsm6 libxext6 libglib2.0-0 build-essential ffmpeg
    python -c "import torch,xformers,flash_attn,mediapipe,cv2,daily; print(\"deps ok\", torch.__version__)"
  '
else
  echo "    soulx container exists — reusing"
  docker start soulx >/dev/null 2>&1 || true
fi

# An old process still holding :8899 makes the restart fail with "address already
# in use", so clear it first.
docker exec soulx bash -c 'pkill -9 python 2>/dev/null; sleep 3' || true
docker exec -d soulx bash -c 'cd /workspace/SoulX-FlashHead && CC=cc python avatar_daily.py > avatar_daily.log 2>&1'

# ~60s of model load + avatar prep + torch.compile warmup. A call started inside
# that window gets connection-refused on :8899, so wait for the real sentinel.
printf '    waiting for avatar '
for i in $(seq 1 100); do
  if grep -q "serving on" "$SOULX_DIR/avatar_daily.log" 2>/dev/null; then printf ' ready\n'; break; fi
  printf '.'; sleep 3
  [ "$i" = 100 ] && { printf '\n'; tail -30 "$SOULX_DIR/avatar_daily.log"; die "avatar did not start (log above)"; }
done

log "Up"
printf '  %-14s %s\n' LLM    "http://127.0.0.1:8011/v1  (replicas :8001 :8002)"
printf '  %-14s %s\n' ASR    "http://127.0.0.1:8890/health"
printf '  %-14s %s\n' TTS    "http://127.0.0.1:8880/health"
printf '  %-14s %s\n' avatar "ws://127.0.0.1:8899   (GPU $AVATAR_GPU)"
printf '  %-14s %s\n' UI     "http://127.0.0.1:4321"
[ -f tunnel-url.txt ] && printf '  %-14s %s\n' public "$(cat tunnel-url.txt)"
echo
echo "In the UI: Advanced -> Avatar service -> ws://<box>:8899, then enable video."
echo "Serving the UI over HTTPS? The avatar URL must be wss:// — browsers block"
echo "mixed content. Tunnel it: ./cloudflared tunnel --url http://127.0.0.1:8899"
echo
echo "Teardown: docker rm -f sglang sglang2 parakeet kokoro-tts llm-lb soulx"
