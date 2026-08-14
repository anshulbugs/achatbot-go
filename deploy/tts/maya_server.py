"""Maya1 TTS service, speaking the same /tts contract as kokoro.

  POST /tts {"input": "...", "voice": "warm", "speed": 1.0} -> raw PCM16 @24kHz

Same shape as deploy/tts/tts_server.py on purpose: the Go side already has a
generic provider for this contract (tts.NewContractProvider), so nothing in the
pipeline needs to know which model is behind it.

WHY MAYA1. It is the only model of fifteen tested that has real expressive
control AND enough throughput: 61 concurrent calls on one GPU at batch 16,
104ms to first audio at 61 concurrent, Apache-2.0 with commercial use granted.
See docs/RESOURCES.md.

TWO THINGS THIS SERVICE IS RESPONSIBLE FOR, because getting them wrong is
audible to a caller:

1. UNKNOWN TAGS NEVER REACH THE MODEL. The LLM writes the text, and if it
   invents <thoughtful> the model has no such token and the markup can be read
   out loud. That exact failure has bitten this project before, when unparsed
   gemma-4 tool-call markup was spoken to a caller. Only the twenty real tags
   survive; everything else in angle brackets is stripped.

2. ONE VOICE PER PERSONA, FOREVER. Maya1's voice is sampled, so the same
   description gives a different-sounding person on each call unless the seed
   is pinned. A campaign whose agent sounds like a new human every call is
   worse than one with a flat voice, so each persona carries a fixed seed.
"""
import os
import re
import struct

import numpy as np
import torch
import uvicorn
from fastapi import FastAPI, Response
from fastapi.responses import StreamingResponse
from pydantic import BaseModel
from snac import SNAC
from vllm import SamplingParams
from vllm.engine.arg_utils import AsyncEngineArgs
from vllm.engine.async_llm_engine import AsyncLLMEngine

# Official Maya1 token ids. The values in community discussion #8 are wrong:
# lowering the floor to 128256 swallows the control tokens (CODE_START 128257,
# CODE_END 128258, SOH/EOH/SOA 128259-61) as if they were audio, which shifts
# every 7-token frame boundary and produces robotic speech. Verified by ear.
SOH_ID, EOH_ID, SOA_ID = 128259, 128260, 128261
BOS_ID, TEXT_EOT_ID = 128000, 128009
CODE_START_TOKEN_ID, CODE_END_TOKEN_ID = 128257, 128258
CODE_TOKEN_OFFSET = 128266
SNAC_MIN_ID, SNAC_MAX_ID = 128266, 156937
RATE = 24000

MODEL = os.environ.get("MAYA_MODEL", "maya-research/maya1")
SNAC_REPO = os.environ.get("SNAC_REPO", "hubertsiuzdak/snac_24khz")
GPU_FRAC = float(os.environ.get("MAYA_GPU_FRACTION", "0.80"))
# 2048 caps one generation at about 22 seconds of audio, past which the model
# is cut off mid-sentence and races. Our turns are 3-8s, so this is generous,
# but raise it if a campaign uses long scripted paragraphs.
MAX_LEN = int(os.environ.get("MAYA_MAX_MODEL_LEN", "2048"))

# The twenty tags that actually exist in the model's tokenizer. Anything else
# is markup the caller would otherwise hear.
VALID_TAGS = {
    "angry", "appalled", "chuckle", "cry", "curious", "disappointed",
    "excited", "exhale", "gasp", "giggle", "gulp", "laugh_harder", "laugh",
    "mischievous", "sarcastic", "scream", "sigh", "sing", "snort", "whisper",
}
TAG_RE = re.compile(r"<([a-zA-Z_]+)>")

# MAYA1'S OWN DESCRIPTION GRAMMAR, not free prose.
#
# The first live call used flowing English ("warm, friendly and reassuring
# customer-service tone...") and sounded flat and generic. The official Space
# uses a structured attribute format instead:
#
#   Realistic <gender> voice in the <age> age with a <accent> accent. <pitch>
#   pitch, <timbre> timbre, <pacing> pacing, <tone> tone delivery at
#   <intensity> intensity, <domain> domain, <role> role, <delivery> delivery
#
# which is almost certainly what it was trained on. In this format the slots
# actually bite: pacing alone moved an identical sentence between 129 and 268
# words per minute, with no speed parameter anywhere.
#
# These two were picked by ear from six candidates.
PERSONAS = {
    "brisk_warm": (
        "Realistic female voice in the 30s age with a american accent. Normal "
        "pitch, warm timbre, brisk pacing, friendly tone delivery at medium "
        "intensity, customer_service domain, sales_agent role, natural delivery",
        1234),
    "low_calm": (
        "Realistic female voice in the 30s age with a american accent. Normal "
        "pitch, throaty timbre, conversational pacing, calm tone delivery at "
        "low intensity, podcast domain, interviewer role, formal delivery",
        4321),
}
DEFAULT_PERSONA = os.environ.get("MAYA_VOICE", "brisk_warm")

# Streaming: emit the first audio as soon as a few frames exist, then in larger
# blocks. The first block is what a caller waits for, so it is deliberately
# small; later blocks are bigger because re-decoding has a cost and nobody is
# waiting on them.
FIRST_EMIT_FRAMES = 4          # ~0.33s of audio
THEN_EMIT_FRAMES = 12          # ~1s of audio
WARMUP_SAMPLES = 2048          # decoder settling noise, dropped from the start

app = FastAPI()
engine: AsyncLLMEngine | None = None
snac_model = None
tokenizer = None


class TTSReq(BaseModel):
    input: str
    voice: str = ""
    speed: float = 1.0


def sanitize(text: str) -> str:
    """Drop any angle-bracket markup that is not a real Maya1 tag."""
    return TAG_RE.sub(lambda m: m.group(0) if m.group(1).lower() in VALID_TAGS else "", text)


def resolve_voice(voice: str) -> tuple[str, int]:
    """A persona name, or a raw description (seeded by its own hash)."""
    key = (voice or DEFAULT_PERSONA).strip()
    if key in PERSONAS:
        return PERSONAS[key]
    if len(key) > 40:                      # looks like a description, not a name
        return key, abs(hash(key)) % (2**31)
    return PERSONAS[DEFAULT_PERSONA]


def build_ids(description: str, text: str) -> list[int]:
    ids = tokenizer.encode(f'<description="{description}"> {text}',
                           add_special_tokens=False)
    return [SOH_ID, BOS_ID] + ids + [TEXT_EOT_ID, EOH_ID, SOA_ID, CODE_START_TOKEN_ID]


def unpack(token_ids) -> tuple[list, list, list] | None:
    snac = [t for t in token_ids if SNAC_MIN_ID <= t <= SNAC_MAX_ID]
    frames = len(snac) // 7
    if frames == 0:
        return None
    l1, l2, l3 = [], [], []
    for i in range(frames):
        s = snac[i * 7:(i + 1) * 7]
        l1.append((s[0] - CODE_TOKEN_OFFSET) % 4096)
        l2.extend([(s[1] - CODE_TOKEN_OFFSET) % 4096,
                   (s[4] - CODE_TOKEN_OFFSET) % 4096])
        l3.extend([(s[2] - CODE_TOKEN_OFFSET) % 4096,
                   (s[3] - CODE_TOKEN_OFFSET) % 4096,
                   (s[5] - CODE_TOKEN_OFFSET) % 4096,
                   (s[6] - CODE_TOKEN_OFFSET) % 4096])
    return l1, l2, l3


def to_pcm16(audio: np.ndarray) -> bytes:
    a = np.clip(audio, -1.0, 1.0)
    return struct.pack(f"<{len(a)}h", *(a * 32767).astype(np.int16))


@app.on_event("startup")
async def startup():
    global engine, snac_model, tokenizer
    engine = AsyncLLMEngine.from_engine_args(AsyncEngineArgs(
        model=MODEL, dtype="bfloat16", gpu_memory_utilization=GPU_FRAC,
        max_model_len=MAX_LEN, enable_prefix_caching=True))
    tokenizer = engine.get_tokenizer()
    if hasattr(tokenizer, "__await__"):
        tokenizer = await tokenizer
    snac_model = SNAC.from_pretrained(SNAC_REPO).eval().to("cuda")

    # WARM THE BATCH SHAPES. vLLM captures a CUDA graph the first time it sees
    # a given batch size: measured 2.2s on that first request against 57ms once
    # warm. Paying that here costs a few seconds of startup; paying it on a live
    # call is two seconds of silence after the caller stops speaking.
    import asyncio
    for n in (1, 2, 4, 8, 16, 32):
        await asyncio.gather(*[_generate("Warming up.", "warm") for _ in range(n)])
    print(f"maya1 ready ({MODEL}, {RATE}Hz, personas: {', '.join(PERSONAS)})",
          flush=True)


async def _generate(text: str, voice: str) -> np.ndarray | None:
    description, seed = resolve_voice(voice)
    sp = SamplingParams(temperature=0.4, top_p=0.9, repetition_penalty=1.1,
                        max_tokens=MAX_LEN, min_tokens=28, seed=seed,
                        stop_token_ids=[CODE_END_TOKEN_ID])
    rid = f"tts-{abs(hash((text, voice, seed)))}"
    final = None
    async for out in engine.generate({"prompt_token_ids": build_ids(description, text)},
                                     sp, rid):
        final = out
    if final is None:
        return None
    levels = unpack(list(final.outputs[0].token_ids))
    if levels is None:
        return None
    codes = [torch.tensor(l, dtype=torch.long, device="cuda").unsqueeze(0)
             for l in levels]
    with torch.inference_mode():
        audio = snac_model.decoder(snac_model.quantizer.from_codes(codes))
    a = audio[0, 0].float().cpu().numpy()
    # The decoder's first samples are warm-up noise, not speech.
    return a[2048:] if len(a) > 4096 else a


@app.get("/health")
def health():
    return {"status": "ok", "model": MODEL, "sample_rate": RATE,
            "voices": list(PERSONAS)}


async def _stream(text: str, voice: str):
    """Yield PCM as it is generated, rather than after the whole clip.

    THIS IS THE DIFFERENCE BETWEEN 1.8s AND ~100ms PER TURN. The first live
    call waited for the entire utterance before any audio played, and measured
    a median of 1796ms against kokoro's ~1000ms. The Go client already reads
    this response incrementally (HTTPTTSProvider.SynthesizeStream), so all that
    was missing was a server that writes incrementally.

    Frames are re-decoded from the start each time and only the new tail is
    emitted. Decoding just the new frames in isolation would put a seam at
    every block boundary, because the decoder has no left context there.
    """
    description, seed = resolve_voice(voice)
    sp = SamplingParams(temperature=0.4, top_p=0.9, repetition_penalty=1.1,
                        max_tokens=MAX_LEN, min_tokens=28, seed=seed,
                        stop_token_ids=[CODE_END_TOKEN_ID])
    rid = f"tts-{abs(hash((text, voice, seed)))}"

    emitted = 0          # samples already sent
    last_frames = 0      # frames already decoded
    ids = []
    async for out in engine.generate(
            {"prompt_token_ids": build_ids(description, text)}, sp, rid):
        ids = list(out.outputs[0].token_ids)
        levels = unpack(ids)
        if levels is None:
            continue
        frames = len(levels[0])
        need = FIRST_EMIT_FRAMES if emitted == 0 else THEN_EMIT_FRAMES
        if frames - last_frames < need:
            continue
        last_frames = frames
        codes = [torch.tensor(l, dtype=torch.long, device="cuda").unsqueeze(0)
                 for l in levels]
        with torch.inference_mode():
            audio = snac_model.decoder(snac_model.quantizer.from_codes(codes))
        a = audio[0, 0].float().cpu().numpy()
        if emitted == 0:
            a = a[WARMUP_SAMPLES:] if len(a) > 2 * WARMUP_SAMPLES else a
        # Hold back the last frame's worth: the tail of a partial decode is
        # still changing as more codes arrive.
        tail_guard = 512
        new = a[emitted:max(emitted, len(a) - tail_guard)]
        if len(new):
            emitted += len(new)
            yield to_pcm16(new)

    # Whatever is left after generation stops.
    levels = unpack(ids) if ids else None
    if levels:
        codes = [torch.tensor(l, dtype=torch.long, device="cuda").unsqueeze(0)
                 for l in levels]
        with torch.inference_mode():
            audio = snac_model.decoder(snac_model.quantizer.from_codes(codes))
        a = audio[0, 0].float().cpu().numpy()
        if emitted == 0:
            a = a[WARMUP_SAMPLES:] if len(a) > 2 * WARMUP_SAMPLES else a
        if len(a) > emitted:
            yield to_pcm16(a[emitted:])


@app.post("/tts")
async def tts(req: TTSReq):
    text = sanitize(req.input).strip()
    if not text:
        return Response(content=b"", media_type="application/octet-stream")
    return StreamingResponse(_stream(text, req.voice),
                             media_type="application/octet-stream")


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8881")),
                log_level="warning")
