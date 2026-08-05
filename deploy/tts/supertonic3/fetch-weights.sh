#!/bin/bash
# Fetch the four Supertonic-3 ONNX graphs (~380MB) into onnx/ and verify them.
#
# The graphs are NOT in this repository. achatbot-go is a public fork, and
# GitHub bills git-lfs storage to the fork parent, so it refuses LFS uploads
# from forks outright -- "can not upload new objects to public fork". Nothing in
# the repo config can work around that.
#
# Everything else needed to run the model IS committed: the ten voice styles
# (small enough for plain git), config.json, the licence, and SHA256SUMS. Only
# the binaries are external, and SHA256SUMS pins exactly which build they must
# be, so a substituted or truncated download fails loudly here rather than
# quietly synthesizing something else.
#
# SUPERTONIC_MIRROR should point at our own copy. The upstream default is a
# fallback only: Supertone archived the project on 2026-07-23 and nothing
# obliges them to keep hosting it.
#
#   SUPERTONIC_MIRROR=<your-org>/supertonic3-weights ./fetch-weights.sh
set -euo pipefail

cd "$(dirname "$0")"
REPO="${SUPERTONIC_MIRROR:-Supertone/supertonic-3}"
BASE="https://huggingface.co/${REPO}/resolve/main"
FILES="text_encoder.onnx duration_predictor.onnx vector_estimator.onnx vocoder.onnx"

mkdir -p onnx
for f in $FILES; do
    if [ -f "onnx/$f" ]; then
        echo "have onnx/$f"
        continue
    fi
    echo "fetching $f from $REPO ..."
    curl -fL --progress-bar \
        ${HF_TOKEN:+-H "Authorization: Bearer $HF_TOKEN"} \
        -o "onnx/$f.part" "$BASE/onnx/$f"
    mv "onnx/$f.part" "onnx/$f"
done

echo "verifying against SHA256SUMS ..."
if sha256sum -c SHA256SUMS --quiet; then
    echo "OK - all files match the mirrored build."
else
    echo "CHECKSUM MISMATCH. Do not deploy these files." >&2
    exit 1
fi
