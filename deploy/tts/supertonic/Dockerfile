# GPU Supertonic-3 TTS. ONNX Runtime rather than torch, so the base is a plain
# CUDA image -- but it still has to be CUDA 12.8+/cu13 for Blackwell (sm_120 /
# RTX 5090); a cu126 build has no kernels for these cards.
FROM nvidia/cuda:12.8.0-cudnn-runtime-ubuntu24.04

RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# onnxruntime-gpu[cuda,cudnn] pulls the nvidia-* wheels that carry the CUDA EP's
# shared libraries. Installing plain `onnxruntime` alongside it is fatal: the two
# ship the same package directory and the CPU build wins on import, silently
# leaving you with no CUDAExecutionProvider. `supertonic` depends on
# `onnxruntime`, so it is installed FIRST and the GPU build is force-reinstalled
# over the top -- order matters, do not merge these into one pip call.
RUN pip install --no-cache-dir --break-system-packages \
      supertonic==1.3.1 fastapi "uvicorn[standard]" numpy scipy soundfile \
 && pip uninstall -y --break-system-packages onnxruntime \
 && pip install --no-cache-dir --break-system-packages --force-reinstall --no-deps \
      onnxruntime-gpu==1.28.0 \
 && pip install --no-cache-dir --break-system-packages \
      "onnxruntime-gpu[cuda,cudnn]==1.28.0"

# The nvidia wheels put their .so files under site-packages/nvidia/*/lib, which
# is not on the default loader path.
ENV LD_LIBRARY_PATH=/usr/lib/python3/dist-packages/nvidia:/usr/local/lib/python3.12/dist-packages/nvidia

# Weights are vendored rather than downloaded: upstream archived the project on
# 2026-07-23 and Hugging Face hosting is not guaranteed to outlive it. See
# deploy/tts/supertonic3/PROVENANCE.md.
COPY supertonic3 /app/supertonic3
COPY supertonic/server.py /app/server.py
COPY supertonic/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

WORKDIR /app
EXPOSE 8881
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8881"]
