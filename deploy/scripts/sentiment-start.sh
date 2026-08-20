#!/bin/bash
# Mid-call sentiment classifier — a small LLM on CPU, deliberately off the GPUs.
#
#   bash deploy/scripts/sentiment-start.sh
#
# WHY CPU. The conversation model's first-turn latency is what decides how many
# calls the fleet carries (deploy/loadtest/README.md), so anything sharing that
# GPU is spending capacity. This classifier runs once per caller turn, off the
# reply path, where latency is nearly free — so it belongs on the hardware we
# have most of. Measured at the rate 60 concurrent calls produce (4.84 req/s):
#
#   p50 489ms, p95 554ms, 8 CPU cores of 255, zero GPU memory.
#
# WHY llama3.2:3b AND NOT SOMETHING SMALLER. Measured on twelve probes:
#
#   llama3.2:3b            11/12   the one miss still raised an alert
#   qwen2.5:0.5b-instruct   9/12   missed "Can I speak to a real person"
#   llama3.2:1b             5/12   worse than the 0.5B
#   qwen3:0.6b              4/12   and it is a reasoning model, see below
#
# The 0.5B's failure is the case the whole feature exists for. Size does not
# scale smoothly here — 1B scored worse than 0.5B — so pick on the probes, not
# on parameter count.
#
# NEVER USE A REASONING MODEL. qwen3 emits <think> before answering, so a small
# max_tokens truncates the reply to an empty string: the classifier returns
# "none" for everything and fails silently, which is the one failure this
# feature cannot afford. Given room to think it costs ~220 tokens a turn and
# still gets it wrong.
set -euo pipefail

PORT="${SENTIMENT_PORT:-11435}"
BASE_MODEL="${SENTIMENT_BASE_MODEL:-llama3.2:3b}"
MODEL="${SENTIMENT_MODEL:-sentiment-cpu}"
OLLAMA_BIN="${OLLAMA_BIN:-$HOME/ollama/bin/ollama}"
MODELS_DIR="${OLLAMA_MODELS:-$HOME/ollama-models}"
# Concurrent request slots. THE important setting: with the default of one, the
# calls serialise and p50 goes from 489ms to 12 SECONDS at the same throughput,
# while using FEWER cores. Latency here was never a thread-count problem.
PARALLEL="${SENTIMENT_PARALLEL:-8}"
# Threads per request. Bounds contention with the call path; 32 measured 8 cores
# in steady state at the 60-call rate.
THREADS="${SENTIMENT_THREADS:-32}"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

command -v curl >/dev/null || die "curl not installed"
mkdir -p "$MODELS_DIR"

# Install ollama into $HOME if it is not already here.
#
# Into $HOME rather than /usr/local: this has to work on a fresh cluster where
# we may not have root, and the official install script wants it. The tarball is
# self-contained, so unpacking it is the whole installation.
if [ ! -x "$OLLAMA_BIN" ]; then
  if command -v ollama >/dev/null 2>&1; then
    OLLAMA_BIN="$(command -v ollama)"
  else
    log "Installing ollama into $HOME/ollama"
    mkdir -p "$HOME/ollama"
    # ollama.com/download/ollama-linux-<arch>.tgz is GONE — it 404s for amd64
    # and arm64 alike; upstream moved to zstd tarballs on the GitHub release.
    # Resolved from the API so this does not rot again the next time they
    # rename something, and so the arch is whatever the box actually is.
    case "$(uname -m)" in
      x86_64)        OARCH="amd64" ;;
      aarch64|arm64) OARCH="arm64" ;;
      *) die "unsupported CPU architecture $(uname -m) for ollama" ;;
    esac
    command -v zstd >/dev/null 2>&1 || die "zstd is needed to unpack ollama (apt-get install zstd)"
    ASSET="ollama-linux-${OARCH}.tar.zst"
    URL="$(curl -fsSL https://api.github.com/repos/ollama/ollama/releases/latest | grep -o "https://[^\"]*${ASSET}" | head -1)"
    [ -n "$URL" ] || die "could not find $ASSET in the latest ollama release"
    log "Downloading $ASSET (about 1.5GB)"
    curl -fsSL "$URL" | tar --zstd -x -C "$HOME/ollama" || die "could not download ollama"
    OLLAMA_BIN="$HOME/ollama/bin/ollama"
  fi
fi
[ -x "$OLLAMA_BIN" ] || die "ollama not found at $OLLAMA_BIN (set OLLAMA_BIN=)"

if curl -sf -m 3 "http://127.0.0.1:${PORT}/api/tags" >/dev/null 2>&1; then
  log "sentiment server already up on :${PORT}"
else
  log "Starting CPU-only ollama on :${PORT}"
  # CUDA_VISIBLE_DEVICES="" is what keeps this off the GPUs. Without it ollama
  # takes a card and the whole point of running it here is lost.
  CUDA_VISIBLE_DEVICES="" \
  OLLAMA_HOST="127.0.0.1:${PORT}" \
  OLLAMA_MODELS="$MODELS_DIR" \
  OLLAMA_NUM_PARALLEL="$PARALLEL" \
  OLLAMA_MAX_LOADED_MODELS=1 \
  OLLAMA_KEEP_ALIVE=24h \
    nohup setsid "$OLLAMA_BIN" serve > "$HOME/ollama-cpu.log" 2>&1 < /dev/null &
  disown
  for _ in $(seq 1 30); do
    curl -sf -m 2 "http://127.0.0.1:${PORT}/api/tags" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -sf -m 3 "http://127.0.0.1:${PORT}/api/tags" >/dev/null 2>&1 \
    || die "ollama did not come up — see $HOME/ollama-cpu.log"
fi

export OLLAMA_HOST="127.0.0.1:${PORT}" OLLAMA_MODELS="$MODELS_DIR"

if ! "$OLLAMA_BIN" list 2>/dev/null | grep -q "^${BASE_MODEL%%:*}"; then
  log "Pulling $BASE_MODEL (~2GB)"
  "$OLLAMA_BIN" pull "$BASE_MODEL" >/dev/null
fi

log "Building $MODEL"
tmp=$(mktemp)
cat > "$tmp" <<EOF
FROM $BASE_MODEL
PARAMETER num_thread $THREADS
PARAMETER temperature 0
PARAMETER num_predict 8
EOF
"$OLLAMA_BIN" create "$MODEL" -f "$tmp" >/dev/null
rm -f "$tmp"

# Warm it. The first request pays the model load, and paying it here rather than
# on a live call keeps the first caller's classification from arriving 5s late.
log "Warming"
curl -sf -m 120 "http://127.0.0.1:${PORT}/v1/chat/completions" \
  -H 'content-type: application/json' \
  -d "{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":4,\"stream\":false}" \
  >/dev/null || die "warmup request failed"

log "Up"
printf '  %-18s %s\n' endpoint "http://127.0.0.1:${PORT}/v1"
printf '  %-18s %s\n' model "$MODEL"
echo
echo "Point the agent at it in config.yaml:"
echo "  server:"
echo "    sentiment_base_url: \"http://127.0.0.1:${PORT}/v1\""
echo "    sentiment_model: \"${MODEL}\""
echo
echo "Verify GPUs are untouched:  nvidia-smi --query-compute-apps=pid,used_memory --format=csv"
