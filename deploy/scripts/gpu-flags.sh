#!/bin/bash
# How this box hands a GPU to a container — detected, not assumed.
#
#   source deploy/scripts/gpu-flags.sh
#   gpu_docker_flags 0          # sets the GPU_FLAGS array
#   docker run "${GPU_FLAGS[@]}" ...
#
# WHY THIS EXISTS. The 4x RTX 5090 box registered a docker runtime named
# "nvidia", so every start script passed --runtime=nvidia with
# NVIDIA_VISIBLE_DEVICES. The Lambda GH200 (Ubuntu 24.04, docker 29.2,
# container-toolkit with CDI) does NOT register that runtime at all:
#
#   $ docker run --runtime=nvidia ... nvidia-smi -L
#   docker: Error response from daemon: unknown or invalid runtime name: nvidia
#
#   $ docker run --device nvidia.com/gpu=0 ... nvidia-smi -L
#   GPU 0: NVIDIA GH200 480GB (UUID: GPU-2175d97d-...)
#
# Both mechanisms are correct on the box that has them, and neither is correct
# on both, so the flags are chosen at run time. That also means this file is
# right on the 5090 box as well, and is a merge-back candidate for main.
#
# GPU_MODE forces it: "runtime", "cdi", or "auto" (default).

# gpu_docker_flags <gpu-index-or-all>
#
# Sets the global array GPU_FLAGS. Returns non-zero, with an explanation on
# stderr, if the box offers no GPU passthrough at all — callers should treat
# that as fatal rather than starting a container that will run on the CPU.
gpu_docker_flags() {
  local n="${1:-0}"
  local mode="${GPU_MODE:-auto}"
  local info
  info="$(docker info 2>/dev/null)" || info=""

  if [ "$mode" = "auto" ]; then
    # Word-boundary match: "Runtimes: io.containerd.runc.v2 runc" must NOT
    # count, and a CDI line mentioning nvidia.com must not be mistaken for a
    # registered runtime — that confusion is exactly what made the old
    # dependency check pass on a box where --runtime=nvidia fails.
    if printf '%s' "$info" | grep -qE '^[[:space:]]*Runtimes:.*(^|[[:space:]])nvidia([[:space:]]|$)'; then
      mode="runtime"
    elif printf '%s' "$info" | grep -q 'cdi: nvidia\.com/gpu='; then
      mode="cdi"
    else
      mode="none"
    fi
  fi

  case "$mode" in
    runtime) GPU_FLAGS=(--runtime=nvidia -e "NVIDIA_VISIBLE_DEVICES=$n") ;;
    cdi)     GPU_FLAGS=(--device "nvidia.com/gpu=$n") ;;
    *)
      echo "no GPU passthrough: docker registers neither an 'nvidia' runtime nor CDI devices" >&2
      echo "  install the NVIDIA container toolkit, then:" >&2
      echo "  sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker" >&2
      return 1
      ;;
  esac
  GPU_MODE_RESOLVED="$mode"
  return 0
}
