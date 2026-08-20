# GPU Kokoro TTS, built for Grace-Hopper (aarch64, sm_90).
#
# BASE IMAGE CHANGED FROM main. main uses pytorch/pytorch:2.9.0-cuda12.8-*,
# which is published for linux/amd64 ONLY — there is no arm64 manifest, so it
# cannot be pulled on Grace at all. nvidia/cuda is multi-arch, and torch comes
# from the cu128 index, which publishes aarch64 (sbsa) wheels.
#
# CUDA 12.8 is kept even though Hopper is sm_90 and does not need Blackwell
# kernels: it is what the rest of the stack is pinned to, and changing two
# variables at once on a new architecture makes a failure impossible to
# attribute.
FROM nvidia/cuda:12.8.1-cudnn-runtime-ubuntu24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        espeak-ng git python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

# 24.04 marks the system python as externally managed, and this image has no
# other python. A venv is the supported way to own the interpreter here.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# torch first and on its own, so a resolver backtrack cannot quietly pull an
# x86 wheel or a CPU build in as a dependency of something else.
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cu128 \
        torch torchaudio
RUN pip install --no-cache-dir "kokoro>=0.9" soundfile fastapi "uvicorn[standard]" numpy

# Fail the BUILD rather than the first call if the wheel resolution went wrong.
# A CPU-only torch here looks like a working image and shows up as a 40x
# slowdown under load, which is a miserable thing to diagnose live.
RUN python -c "import torch; assert torch.version.cuda, 'CPU-only torch'; print('torch', torch.__version__, 'cuda', torch.version.cuda)"

COPY tts_server.py /app/tts_server.py
WORKDIR /app
EXPOSE 8880
CMD ["uvicorn", "tts_server:app", "--host", "0.0.0.0", "--port", "8880"]
