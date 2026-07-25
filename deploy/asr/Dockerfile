# GPU Parakeet ASR (NeMo). torch 2.9 / CUDA 12.8 base for Blackwell (sm_120).
# nemo_toolkit[asr] installs cleanly on this base without downgrading torch.
FROM pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime
RUN apt-get update && apt-get install -y --no-install-recommends git ffmpeg libsndfile1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir "nemo_toolkit[asr]" fastapi "uvicorn[standard]" numpy
COPY asr_server.py /app/asr_server.py
WORKDIR /app
EXPOSE 8890
CMD ["python", "asr_server.py"]
