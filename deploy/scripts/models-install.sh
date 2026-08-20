#!/bin/bash
# Model files the AGENT loads directly, as opposed to the ones the GPU services
# pull from Hugging Face themselves.
#
#   bash deploy/scripts/models-install.sh
#
# Run by the up-voice-* scripts; safe to run on its own and idempotent.
#
# WHY THIS EXISTS. There is exactly one of these today — silero_vad.onnx — and
# nothing fetched it. On a box that had been running a while it was simply
# there, put in place by hand at some point and never written down, so the gap
# only appeared on a REBUILD: the agent starts, fails to create any VAD
# provider, and exits with
#
#   failed to create VAD provider for model "silero" (model file downloaded?)
#
# printed once per pool slot, which on a pool of 80 buries the one line that
# matters. That is a bad way to find out about a 630 KB download.
#
# The path is not configurable: pkg/consts derives MODELS_DIR from the source
# tree, so the file has to land in <repo>/models/.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

MODELS_DIR="${MODELS_DIR:-$PWD/models}"
BASE="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

mkdir -p "$MODELS_DIR"

# name:minimum-plausible-size. The size check catches the classic failure where
# a redirect or an error page is saved as the model and the agent then dies
# somewhere much less obvious.
for spec in "silero_vad.onnx:100000" "ten-vad.onnx:100000"; do
  name="${spec%%:*}"; min="${spec##*:}"
  dest="$MODELS_DIR/$name"
  if [ -f "$dest" ] && [ "$(stat -c%s "$dest")" -ge "$min" ]; then
    printf '    \033[1;32mOK\033[0m   %s (%s bytes)\n' "$name" "$(stat -c%s "$dest")"
    continue
  fi
  log "Fetching $name"
  curl -fsSL -o "$dest.tmp" "$BASE/$name" || die "could not download $name from $BASE"
  got="$(stat -c%s "$dest.tmp")"
  [ "$got" -ge "$min" ] || { rm -f "$dest.tmp"; die "$name came back only $got bytes — not a model"; }
  mv "$dest.tmp" "$dest"
  printf '    \033[1;32mOK\033[0m   %s (%s bytes)\n' "$name" "$got"
done

echo
echo "    VAD models in $MODELS_DIR"
