"""Supertonic-3 GPU TTS service.

Speaks the same HTTP contract as deploy/tts/tts_server.py (Kokoro) so the Go
side can swap between them by changing one config key:

    POST /tts  {"input": str, "voice": str, "speed": float} -> raw LE PCM16
    GET  /health                                            -> {"status": "ok", ...}

Three things about this model are load-bearing and none of them are in the
upstream docs. The upstream repo was archived on 2026-07-23, so they will never
be fixed there -- see deploy/tts/supertonic3/PROVENANCE.md.

1. The `supertonic` package hard-codes CPUExecutionProvider when it builds its
   four ONNX sessions. Rebinding them to CUDA is the difference between 3x and
   126x realtime. If the rebind ever silently fails, this service still works
   and just gets 40x slower, which is exactly the kind of regression that only
   shows up under load -- so REQUIRE_CUDA makes it fail loudly at startup
   instead.

2. Concurrency lives INSIDE this process, not across uvicorn workers. Several
   uvicorn workers means several CUDA contexts on one card, and they time-slice
   badly: measured 20.3 req/s at 1 worker rising only to 31.6 at 8, for 8x the
   VRAM. A pool of model instances sharing a single context reached 56 req/s at
   4 instances. So run this with `--workers 1` and scale SUPERTONIC_INSTANCES.

   The pool must stay bounded. ONNX Runtime's CUDA provider allocates a cuBLAS
   handle per calling thread, and enough threads dies with "CUBLAS failure 3:
   resource allocation failed" -- seen at 61, safe at 8. The executor is sized
   to the instance count so the two can never drift apart.

3. The advertised <laugh>/<breath>/<sigh> expression tags are passed straight
   through to the model as characters -- nothing strips them, and they do change
   the output. Whether they produce real laughter rather than generic vocalised
   noise is unverified (an invented tag changes the audio just as much), so this
   service forwards them untouched and leaves the judgement to the caller.
"""

import asyncio
import logging
import os
import queue
import time
from concurrent.futures import ThreadPoolExecutor

import numpy as np
import onnxruntime as ort
from fastapi import FastAPI, HTTPException, Response
from pydantic import BaseModel
from scipy.signal import resample_poly

log = logging.getLogger("supertonic")

NATIVE_RATE = 44100
# Kokoro emits 24kHz and the browser AudioContext is pinned to it, so matching
# that rate makes this a true drop-in: no frame-size math, no browser change,
# no resampling anywhere downstream. 24kHz carries 12kHz of bandwidth, far above
# anything in speech, so nothing audible is lost. Set SUPERTONIC_RATE=44100 to
# serve native instead -- but then PLAYBACK_RATE in the UI must change to match,
# or the browser resamples and dulls the treble.
OUT_RATE = int(os.environ.get("SUPERTONIC_RATE", "24000"))
MODEL_DIR = os.environ.get("SUPERTONIC_MODEL_DIR", "/app/supertonic3")
DEFAULT_VOICE = os.environ.get("SUPERTONIC_VOICE", "F2")
# 5 is fastest (184x realtime), 12 is best quality (90x). 8 is upstream default.
TOTAL_STEPS = int(os.environ.get("SUPERTONIC_STEPS", "8"))
REQUIRE_CUDA = os.environ.get("SUPERTONIC_REQUIRE_CUDA", "1") == "1"
# Model copies in this process, each ~1.7GB VRAM. This is the concurrency knob;
# uvicorn workers are not (see module docstring note 2). Keep well under the
# thread count that exhausts cuBLAS handles.
INSTANCES = int(os.environ.get("SUPERTONIC_INSTANCES", "4"))

VOICES = ["F1", "F2", "F3", "F4", "F5", "M1", "M2", "M3", "M4", "M5"]

app = FastAPI()

_pool: "queue.Queue" = queue.Queue()
_executor: ThreadPoolExecutor = None
_ready = False
_instances: list = []
_styles: list = []
_providers: list = []


class TTSReq(BaseModel):
    input: str
    voice: str = DEFAULT_VOICE
    speed: float = 1.0


def _bind_cuda(tts) -> list:
    """Rebuild the four ONNX sessions on CUDA.

    The upstream pipeline builds these with CPUExecutionProvider only, and
    exposes no way to choose. Rebinding after construction is the supported-
    enough seam: each session keeps its own _model_path, so we reopen the same
    graph with a different provider list.
    """
    model = tts.model
    providers = [("CUDAExecutionProvider", {"device_id": 0}), "CPUExecutionProvider"]
    bound = []
    for name in ("text_enc_ort", "dp_ort", "vector_est_ort", "vocoder_ort"):
        session = getattr(model, name)
        opts = ort.SessionOptions()
        opts.log_severity_level = 3
        rebound = ort.InferenceSession(session._model_path, sess_options=opts, providers=providers)
        setattr(model, name, rebound)
        bound.append(rebound.get_providers()[0])
    return bound


def to_pcm16(wav: np.ndarray) -> bytes:
    audio = np.asarray(wav).squeeze().astype(np.float32)
    if OUT_RATE != NATIVE_RATE:
        g = np.gcd(OUT_RATE, NATIVE_RATE)
        audio = resample_poly(audio, OUT_RATE // g, NATIVE_RATE // g)
    audio = np.clip(audio, -1.0, 1.0) * 32767.0
    return audio.astype("<i2").tobytes()


@app.on_event("startup")
def startup() -> None:
    global _executor, _providers, _ready, _styles
    from supertonic import TTS

    t0 = time.time()
    for i in range(INSTANCES):
        inst = TTS(model="supertonic-3", model_dir=MODEL_DIR, auto_download=False)
        bound = _bind_cuda(inst)
        if i == 0:
            _providers = bound
        if REQUIRE_CUDA and not all(p == "CUDAExecutionProvider" for p in bound):
            raise RuntimeError(
                f"CUDA rebind failed on instance {i}, sessions bound to {bound}. "
                "Refusing to start: on CPU this model runs ~40x slower and would "
                "silently blow the call budget rather than fail. Set "
                "SUPERTONIC_REQUIRE_CUDA=0 to override."
            )
        styles = {v: inst.get_voice_style(voice_name=v) for v in VOICES}
        # Warm each instance individually -- the first synthesis on a session
        # pays CUDA kernel autotuning, and an unwarmed instance would hand its
        # first caller a multi-second reply.
        inst.synthesize(
            text="Warming up the synthesiser.",
            lang="en",
            voice_style=styles[DEFAULT_VOICE],
            total_steps=TOTAL_STEPS,
            speed=1.0,
        )
        _styles.append(styles)
        _pool.put(i)
        _instances.append(inst)

    # Sized to the pool: a request can only run when it holds an instance, so
    # more threads would just queue on _pool while adding cuBLAS handles.
    _executor = ThreadPoolExecutor(max_workers=INSTANCES, thread_name_prefix="tts")
    _ready = True
    log.warning(
        "supertonic-3 ready: instances=%d providers=%s rate=%d steps=%d startup=%.0fms",
        INSTANCES, _providers, OUT_RATE, TOTAL_STEPS, (time.time() - t0) * 1000,
    )


@app.get("/health")
def health() -> dict:
    return {
        "status": "ok" if _ready else "loading",
        "model": "supertonic-3",
        "providers": _providers,
        "instances": INSTANCES,
        "idle": _pool.qsize(),
        "sample_rate": OUT_RATE,
        "native_rate": NATIVE_RATE,
        "steps": TOTAL_STEPS,
        "voices": VOICES,
    }


@app.post("/tts")
async def tts(req: TTSReq) -> Response:
    if not _ready:
        raise HTTPException(status_code=503, detail="model still loading")

    if req.voice not in VOICES:
        raise HTTPException(
            status_code=400, detail=f"unknown voice {req.voice!r}; have {VOICES}"
        )

    text = req.input.strip()
    if not text:
        return Response(content=b"", media_type="application/octet-stream")

    loop = asyncio.get_running_loop()
    pcm = await loop.run_in_executor(_executor, _synthesize, text, req.voice, req.speed)
    return Response(content=pcm, media_type="application/octet-stream")


def _synthesize(text: str, voice: str, speed: float) -> bytes:
    """Borrow an instance, synthesize, return it. Runs on the executor.

    Blocking on an empty pool is the intended backpressure: the executor is the
    same size as the pool, so a queued request is already waiting for a thread
    and the get() only ever blocks briefly during shutdown races.
    """
    idx = _pool.get()
    try:
        wav, _ = _instances[idx].synthesize(
            text=text,
            lang="en",
            voice_style=_styles[idx][voice],
            total_steps=TOTAL_STEPS,
            speed=speed,
        )
        return to_pcm16(wav)
    finally:
        _pool.put(idx)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8881, log_level="warning")
