# GPU Kokoro TTS. Base image pinned to torch 2.9 / CUDA 12.8 so it runs on
# Blackwell (sm_120 / RTX 5090). Older ready-made TTS images fail on Blackwell
# with "no kernel image available for execution on the device".
FROM pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime
RUN apt-get update && apt-get install -y --no-install-recommends espeak-ng git \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir kokoro>=0.9 soundfile fastapi "uvicorn[standard]" numpy
COPY tts_server.py /app/tts_server.py
WORKDIR /app
EXPOSE 8880
CMD ["uvicorn", "tts_server:app", "--host", "0.0.0.0", "--port", "8880"]
