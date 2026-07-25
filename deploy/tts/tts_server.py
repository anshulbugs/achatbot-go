"""GPU Kokoro TTS server (Blackwell / sm_120 compatible via torch cu128 base).

Endpoints:
  POST /tts  {"input": "...", "voice": "af_heart", "speed": 1.0}
    -> raw little-endian PCM16 mono @ 24000 Hz (application/octet-stream)
  GET  /health

The Go server's kokoro_http TTS provider talks to this over HTTP. Keeping TTS
in its own container means the voice pipeline never links heavy Python/torch,
and one GPU can serve hundreds of concurrent calls (Kokoro is ~82M params,
~1.4 GB VRAM).
"""
import numpy as np
import torch
from fastapi import FastAPI
from fastapi.responses import Response
from pydantic import BaseModel
from kokoro import KPipeline

DEVICE = "cuda" if torch.cuda.is_available() else "cpu"
SAMPLE_RATE = 24000

app = FastAPI()
# 'a' = American English. British voices (b*) also work under this pipeline.
pipeline = KPipeline(lang_code="a", device=DEVICE)


class TTSReq(BaseModel):
    input: str
    voice: str = "af_heart"
    speed: float = 1.0


def to_pcm16(audio: np.ndarray) -> bytes:
    a = np.clip(audio, -1.0, 1.0)
    return (a * 32767.0).astype("<i2").tobytes()


def synth(text: str, voice: str, speed: float) -> np.ndarray:
    chunks = []
    for _gs, _ps, audio in pipeline(text, voice=voice, speed=speed):
        if hasattr(audio, "detach"):
            audio = audio.detach().cpu().numpy()
        chunks.append(np.asarray(audio, dtype=np.float32).reshape(-1))
    if not chunks:
        return np.zeros(0, dtype=np.float32)
    return np.concatenate(chunks)


@app.on_event("startup")
def warmup():
    try:
        synth("warm up", "af_heart", 1.0)
        name = torch.cuda.get_device_name(0) if DEVICE == "cuda" else "cpu"
        print(f"kokoro-gpu ready on {DEVICE} ({name})", flush=True)
    except Exception as e:
        print(f"warmup error: {e}", flush=True)


@app.get("/health")
def health():
    return {"status": "ok", "device": DEVICE, "sample_rate": SAMPLE_RATE}


@app.post("/tts")
def tts(req: TTSReq):
    audio = synth(req.input, req.voice, req.speed)
    return Response(content=to_pcm16(audio), media_type="application/octet-stream")
