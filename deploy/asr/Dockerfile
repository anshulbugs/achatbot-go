# GPU Parakeet ASR (NeMo), built for Grace-Hopper (aarch64, sm_90).
#
# BASE IMAGE CHANGED FROM main, for the same reason as deploy/tts/Dockerfile:
# pytorch/pytorch:2.9.0-cuda12.8-* has no arm64 manifest and cannot be pulled
# on Grace. nvidia/cuda is multi-arch; torch comes from the cu128 index, which
# publishes aarch64 (sbsa) wheels.
#
# THE RISK HERE IS NEMO, NOT TORCH. nemo_toolkit[asr] pulls a long dependency
# tree, and any member of it without an aarch64 wheel gets built from source at
# image-build time — slow, and occasionally impossible. If this build fails,
# read which package it stopped on before assuming the base image is wrong.
FROM nvidia/cuda:12.8.1-cudnn-runtime-ubuntu24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        git ffmpeg libsndfile1 python3 python3-pip python3-venv \
        build-essential python3-dev \
    && rm -rf /var/lib/apt/lists/*

ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cu128 \
        torch torchaudio
RUN pip install --no-cache-dir "nemo_toolkit[asr]" fastapi "uvicorn[standard]" numpy

# NeMo is the most likely thing to have quietly reinstalled torch as a CPU
# build while resolving its own pins, so the check runs after it, not before.
RUN python -c "import torch; assert torch.version.cuda, 'CPU-only torch'; print('torch', torch.__version__, 'cuda', torch.version.cuda)"

COPY asr_server.py /app/asr_server.py
WORKDIR /app
EXPOSE 8890
CMD ["python", "asr_server.py"]
