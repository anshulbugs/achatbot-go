"""GPU ASR server: NVIDIA Parakeet-TDT (NeMo), Blackwell / sm_120 compatible.

Endpoints:
  POST /asr   body = raw little-endian PCM16 mono @ 16000 Hz
    -> {"text": "..."}
  GET  /health

The Go server's parakeet_http ASR provider posts each utterance's PCM here and
gets text back. Parakeet-TDT-0.6b-v2 is English-first, tops the Open ASR
Leaderboard (~6% WER), runs at ~3000x realtime on a GPU, does not hallucinate
during silence (unlike Whisper), and supports word boosting.
"""
import os
import numpy as np
import torch
from fastapi import FastAPI, Request
import uvicorn

DEVICE = "cuda"
print("torch", torch.__version__, "cuda", torch.version.cuda,
      "avail", torch.cuda.is_available(), flush=True)

import nemo.collections.asr as nemo_asr

MODEL = os.environ.get("ASR_MODEL", "nvidia/parakeet-tdt-0.6b-v2")
print("loading", MODEL, flush=True)
model = nemo_asr.models.ASRModel.from_pretrained(MODEL)
model = model.to(DEVICE).eval()
print("model ready", flush=True)

app = FastAPI()


@app.get("/health")
def health():
    return {"status": "ok", "model": MODEL}


def _extract_text(out):
    o = out[0] if isinstance(out, (list, tuple)) else out
    if hasattr(o, "text"):
        return o.text
    if isinstance(o, (list, tuple)):
        return _extract_text(o)
    return str(o)


@app.post("/asr")
async def asr(request: Request):
    raw = await request.body()
    if len(raw) < 2:
        return {"text": ""}
    audio = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    with torch.no_grad():
        out = model.transcribe([audio], batch_size=1, verbose=False)
    return {"text": _extract_text(out).strip()}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8890, log_level="warning")
