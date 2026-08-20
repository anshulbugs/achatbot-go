#!/bin/bash
# Everything the stack needs that is not already on a fresh GPU box.
#
#   bash deploy/scripts/deps-install.sh
#
# Run automatically by up-voice-4gpu.sh; safe to run on its own and idempotent.
#
# SPLIT BY WHO CAN INSTALL IT. Anything needing root — docker, the NVIDIA
# container runtime — is CHECKED and reported with the exact command to fix it,
# never installed silently: on a shared cluster those are the provider's to
# manage and changing them under other tenants is not ours to do. Everything
# else installs into $HOME, so this works without sudo.
#
# Installed here: Go (to build the agent), cloudflared (public ingress).
# ollama is installed by sentiment-start.sh, where it belongs.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.24.0}"
PREFIX="${DEPS_PREFIX:-$HOME}"

# Grace-Hopper boxes are aarch64, and the two downloads below are the only
# things here that care. Everything else is either already on the box or
# comes from a multi-arch container image.
case "$(uname -m)" in
  x86_64)        GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *)             GOARCH="" ;;
esac

log()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[1;32mOK\033[0m   %s\n' "$*"; }
warn() { printf '    \033[1;33mMISS\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

missing_root=0

log "Checking what needs root (not installed here)"

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  ok "docker"
else
  warn "docker — install it, or ask the cluster provider:"
  echo "         curl -fsSL https://get.docker.com | sudo sh"
  echo "         sudo usermod -aG docker \$USER   # then log out and back in"
  missing_root=1
fi

if docker info 2>/dev/null | grep -qi nvidia; then
  ok "nvidia container runtime"
else
  warn "nvidia container runtime — the GPU services cannot start without it:"
  echo "         https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html"
  echo "         sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker"
  missing_root=1
fi

if command -v nvidia-smi >/dev/null 2>&1; then
  ok "nvidia driver ($(nvidia-smi --query-gpu=count --format=csv,noheader | head -1) GPUs visible)"
else
  warn "nvidia-smi — no driver, this is not a GPU box"
  missing_root=1
fi

log "Installing what does not need root (into $PREFIX)"

# Go. Built from source rather than shipping a binary, because the agent links
# CGO against sherpa-onnx and the build must happen on the target's libc.
# sherpa-onnx-go-linux ships prebuilt aarch64 libs alongside x86_64, so the
# CGO link works unchanged on Grace — nothing extra is needed for ARM.
if command -v go >/dev/null 2>&1; then
  ok "go $(go version | awk '{print $3}')"
elif [ -x "$PREFIX/go/bin/go" ]; then
  ok "go (at $PREFIX/go/bin/go — add it to PATH)"
else
  [ -n "$GOARCH" ] || die "unsupported CPU architecture $(uname -m)"
  log "Installing Go $GO_VERSION (linux-$GOARCH)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" \
    | tar -xz -C "$PREFIX" || die "could not download Go"
  ok "go installed at $PREFIX/go/bin/go"
  echo "         add to your shell:  export PATH=\$PATH:$PREFIX/go/bin"
fi

# cloudflared. The box has no public IP and only SSH is open, so every public
# endpoint is a tunnel — this is not optional infrastructure here.
if command -v cloudflared >/dev/null 2>&1; then
  ok "cloudflared"
elif [ -x "$PREFIX/cloudflared" ]; then
  ok "cloudflared (at $PREFIX/cloudflared)"
else
  [ -n "$GOARCH" ] || die "unsupported CPU architecture $(uname -m)"
  log "Installing cloudflared (linux-$GOARCH)"
  curl -fsSL -o "$PREFIX/cloudflared" \
    https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${GOARCH} \
    || die "could not download cloudflared"
  chmod +x "$PREFIX/cloudflared"
  ok "cloudflared installed at $PREFIX/cloudflared"
fi

command -v python3 >/dev/null 2>&1 && ok "python3 (load tests)" || warn "python3 — only needed for deploy/loadtest"
command -v curl >/dev/null 2>&1 && ok "curl" || die "curl is required"

log "Summary"
if [ "$missing_root" = "1" ]; then
  echo "    Root-level dependencies are missing (above). The GPU services will not"
  echo "    start until they are installed. Everything else is ready."
  exit 1
fi
echo "    All dependencies present. Next:  bash deploy/scripts/up-voice-4gpu.sh"
