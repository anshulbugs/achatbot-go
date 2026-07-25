# Voice Agent — Deployment Guide

A self-hosted, low-latency voice agent: **VAD → ASR → LLM → TTS** streamed over
WebSockets, with a **browser demo** and **real phone calls** (Telnyx). This guide
sets it up from scratch on any GPU box — RunPod, Vast.ai, Lambda, a bare server,
etc. — and documents every error we hit and how we fixed it.

Median wire latency achieved: **~0.9–1.3 s per turn** (sub-second is possible).

---

## 1. Architecture

```
                         ┌─────────────────────────────────────────────┐
   Browser mic  ─wss──►  │  Go server (examples/websocket)              │
   Telnyx media ─wss──►  │  ── VAD (Silero, CPU) ── ASR ── LLM ── TTS ──│
                         │     serializer / echo-gate / aggregator      │
                         └───────┬─────────────┬──────────────┬─────────┘
                                 │ HTTP        │ HTTP         │ OpenAI HTTP
                                 ▼             ▼              ▼
                          Parakeet ASR   Kokoro TTS      SGLang LLM
                          (GPU, 8890)    (GPU, 8880)     (GPU, 8001)

   Public URL:  cloudflared tunnel ──► http://127.0.0.1:4321  (UI + /api + /ws + /telnyx)
```

- **Go server** — the pipeline orchestrator. Serves the demo UI at `/`, the
  browser voice WebSocket at `/ws`, the REST API at `/api/*`, and Telnyx media
  at `/telnyx/*`. VAD (Silero) runs in-process on CPU; ASR/TTS/LLM are separate
  GPU services it calls over HTTP.
- **ASR** — NVIDIA **Parakeet-TDT-0.6b-v2** in a NeMo container (`deploy/asr`).
- **TTS** — **Kokoro** in a container (`deploy/tts`), 24 kHz PCM out.
- **LLM** — **SGLang** serving an OpenAI-compatible endpoint (default
  Qwen2.5-3B-Instruct).
- **Public ingress** — `cloudflared` tunnel (quick or named).

Why HTTP microservices for ASR/TTS/LLM? The Go binary stays light (no torch),
each model scales independently, and one GPU per service serves hundreds of
concurrent calls.

---

## 2. Prerequisites

### Hardware
- **1+ NVIDIA GPU.** Minimal single-GPU dev works; production wants dedicated
  GPUs per service (see §8 Scaling). Tested on **RTX 5090 (Blackwell, sm_120)**.
  VRAM per service: LLM 3B ≈ 20 GB, Kokoro TTS ≈ 1.5 GB, Parakeet ASR ≈ 3.5 GB.
- **CPU:** VAD runs one Silero instance per concurrent call. ~0.16 vCPU/call
  observed; 100 calls ≈ 16 cores. Size accordingly (a 64-vCPU box ≈ 300–400 calls
  before VAD-on-CPU is the limit).
- **RAM:** ~0.3 GB per concurrent call at steady state (pools + buffers). 100
  calls ≈ 34 GB.
- **Disk:** ~30 GB for images + model caches (SGLang model, Parakeet weights).

### Software
- **NVIDIA driver** supporting CUDA 12.8 (needed for Blackwell). `nvidia-smi` must work.
- **Docker** + **nvidia-container-toolkit** (so `--runtime=nvidia` works).
- **Go 1.24+** and a **C toolchain** (`gcc`, `make`) — the server links
  sherpa-onnx (CGO) for VAD.
- **git**, and outbound internet for the first model downloads.
- Optional: a **Telnyx** account (API key, Voice API app, a phone number) for the
  phone demo. Optional: a **Cloudflare** account + domain for a stable public URL.

Quick prerequisite check:
```bash
nvidia-smi                       # GPUs visible
docker info | grep -i runtime    # shows "nvidia"
docker run --rm --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=0 \
  pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime \
  python -c "import torch;print(torch.__version__, torch.cuda.is_available(), torch.cuda.get_device_name(0))"
go version                       # >= 1.24
gcc --version
```

---

## 3. Ports & services reference

| Service        | Container / process | Host port      | GPU (default) | Health |
|----------------|---------------------|----------------|---------------|--------|
| Go voice server| `server-bin`        | `4321`         | — (CPU/VAD)   | `GET /api/options` |
| LLM (SGLang)   | `sglang`            | `127.0.0.1:8001` | `LLM_GPU=0` | `GET /v1/models` |
| TTS (Kokoro)   | `kokoro-tts`        | `127.0.0.1:8880` | `TTS_GPU=1` | `GET /health` |
| ASR (Parakeet) | `parakeet`          | `127.0.0.1:8890` | `ASR_GPU=2` | `GET /health` |
| Public tunnel  | `cloudflared`       | → 4321         | —             | `tunnel-url.txt` |

GPU services bind to `127.0.0.1` only — they're reached by the Go server on the
same host. Only port 4321 is exposed publicly (via the tunnel).

---

## 4. Step-by-step setup (fresh box)

Run from the repo root. Set the GPU layout for your box first:

```bash
git clone https://github.com/anshulbugs/achatbot-go.git
cd achatbot-go

# Pick GPUs. Single-GPU box: set all three to 0 (they time-share one card — fine
# for dev, not for load). Multi-GPU: give each service its own card.
export LLM_GPU=0 TTS_GPU=1 ASR_GPU=2
export HF_CACHE=$HOME/hf-cache          # persist model downloads (see RunPod/Vast §7)
```

### 4.1 Build the GPU service images
Docker build context must be a directory Docker can read (see Error #2). Build
from each service dir:
```bash
docker build -t kokoro-gpu:local   deploy/tts
docker build -t parakeet-gpu:local deploy/asr
```

### 4.2 Start the GPU services
```bash
bash deploy/scripts/sglang-start.sh     # LLM  -> :8001  (downloads model on first run)
bash deploy/scripts/kokoro-start.sh     # TTS  -> :8880
bash deploy/scripts/parakeet-start.sh   # ASR  -> :8890  (downloads weights on first run)

# wait for all three to report ready, then verify:
curl -s http://127.0.0.1:8001/v1/models | head -c 120 ; echo
curl -s http://127.0.0.1:8880/health ; echo
curl -s http://127.0.0.1:8890/health ; echo
```
First-run model downloads take minutes; watch `docker logs -f sglang` /
`parakeet`. They land in `$HF_CACHE` so restarts are instant.

### 4.3 Configure the Go server
```bash
cp deploy/config.yaml.example config.yaml       # edit voice/prompt/pool_size as needed
cp deploy/telnyx.env.example  telnyx.env        # optional: real Telnyx creds for phone calls
```

### 4.4 Public URL (for the browser demo's mic + Telnyx webhooks)
The browser demo needs **HTTPS** (getUserMedia requires a secure context) and
Telnyx needs a public webhook URL. A cloudflared quick tunnel gives both:
```bash
# put the cloudflared binary on PATH, then:
bash deploy/scripts/tunnel-start.sh             # writes tunnel-url.txt
```
For a **stable** URL (share with others / production), use a **named** tunnel —
see the comment block in `deploy/scripts/tunnel-start.sh`.

### 4.5 Build & start the server
```bash
bash deploy/scripts/server-start.sh             # builds server-bin (CGO) and launches it
```
Open the public URL from `tunnel-url.txt` in a browser → the demo console.
Locally: `http://localhost:4321`.

### 4.6 Verify end to end
```bash
PUB=$(cat tunnel-url.txt)
curl -s "$PUB/api/options" | head -c 200 ; echo          # pipeline info
# Browser: open $PUB, click "Start call", talk.
# Phone (needs telnyx.env): use the Phone Call tab, or:
curl -s -X POST http://127.0.0.1:4321/api/call \
  -H 'Content-Type: application/json' \
  -d '{"to":"+15551234567","hello":"Hi, this is a test call.","voice":3,"speed":1.2,"volume":1.4}'
```

---

## 5. Single-GPU vs multi-GPU

- **Single GPU (dev):** `export LLM_GPU=0 TTS_GPU=0 ASR_GPU=0`. All three services
  share one card. Works for testing a few calls; the LLM will contend with TTS/ASR
  under load. A 3B LLM (20 GB) + Kokoro (1.5 GB) + Parakeet (3.5 GB) fit together
  on a 24 GB+ card. Lower SGLang `--mem-fraction-static` if you hit OOM.
- **Recommended (4 GPUs):** 2× LLM, 1× TTS, 1× ASR (see §8). Run two SGLang
  containers on two cards behind an nginx upstream on `:8001`, or one SGLang with
  `--tp 2`.

---

## 6. Config & customization

- **Switch models via `config.yaml`** (no code change):
  - `llm.model` / `llm.base_url` — any OpenAI-compatible endpoint (SGLang, vLLM, Ollama).
  - `tts.model`: `kokoro_http` (GPU) or `kokoro` (CPU sherpa). `tts.speaker_id`
    selects the voice (see the UI list; 3 = Heart, 21 = Emma).
  - `asr.model`: `parakeet_http` (GPU) or CPU sherpa models (`sense_voice`,
    `whisper`, `moonshine`, …).
- **Per-call overrides** — the demo UI and `/api/call` accept `voice`, `speed`,
  `volume`, `hello` (greeting), `system_prompt`, `llm`. The browser WS accepts the
  same as query params on `/ws`.
- **The greeting is direct-to-TTS** — never sent to the LLM, so there is no
  first-response delay and no LLM burst when a batch of calls connects at once.
- **`pool_size`** (vad/asr/tts) must be ≥ your max concurrent calls, or sessions
  block waiting for a pool instance. Raise it for load tests (we used 110).
- **`vad.stop_secs`** is the endpointing sensitivity: too low (≤0.4) chops
  sentences on thinking pauses; 0.6 is a good default.

---

## 7. RunPod / Vast.ai / cloud specifics

- **Persist model caches** on a volume so pod restarts don't re-download: mount a
  persistent volume and point `HF_CACHE` at it (e.g. `/workspace/hf-cache`).
  SGLang and Parakeet both read `/root/.cache/huggingface` inside their
  containers, which the start scripts bind to `$HF_CACHE`.
- **Expose only port 4321.** Prefer the cloudflared tunnel (also gives HTTPS,
  which the browser mic requires). If you instead use the provider's proxy, make
  sure it supports **WebSocket upgrades** (Telnyx and the browser both need wss).
- **`--runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=<n>`** is used instead of
  `--gpus` (see Error #3). Both work on most hosts; the env form is the portable
  one and lets you pin each service to a specific card.
- **Docker + snap:** if `docker build` fails to read your context (see Error #2),
  build from a normal home directory, not `/tmp`.
- **File descriptors:** the server raises `ulimit -n 65535` for many concurrent
  WebSockets; if your container caps it lower, raise the host/pod limit.

---

## 8. Scaling & capacity (measured)

All numbers from load tests on **one RTX 5090 per service**; see `/api/loadtest`
and `deploy` benchmark notes.

**LLM burst (the key number).** Firing N *simultaneous* requests at one 5090
(Qwen2.5-3B via SGLang):

| Simultaneous LLM requests | TTFT p50 / p90 | Full reply p50 | Fails |
|---|---|---|---|
| 50  | 84 / 101 ms | 898 ms  | 0 |
| 100 | 118 / 172 ms | 922 ms | 0 |
| 150 | 138 / 193 ms | 1028 ms | 0 |
| 200 | 663 / 926 ms | 1251 ms | 0 |

One 5090 absorbs **~150 simultaneous requests under 200 ms TTFT**; the knee is
~200 (SGLang queues, never drops). **Two LLM 5090s ≈ ~300 simultaneous.**

**Concurrent calls ≠ simultaneous LLM requests.** Each call hits the LLM only
once per turn (~every 8–10 s). So **600–800 concurrent calls generate only
~60–100 in-flight LLM requests** at any instant when naturally desynced — easy
for one GPU, trivial for two. The burst limit only matters if you *launch* many
outbound calls in the same second (throttle your dialer's CPS to avoid it).

**Per-subsystem util at 100 concurrent calls (1 GPU each):**

| Subsystem | Util @100 calls | Realistic ceiling |
|---|---|---|
| LLM (SGLang 3B) | 79% peak | ~600–800 steady (2 GPUs) |
| ASR (Parakeet)  | 7%       | 1000+ |
| TTS (Kokoro)    | ~20%     | ~500–600 (first ceiling) |
| VAD/CPU         | 16% of 255 vCPU | host-dependent |

**Bottom line for 2 LLM + 1 STT + 1 TTS (all 5090):** ~**500–600 concurrent
calls comfortably**. The **TTS GPU** is the first limiter (~500–600), not the LLM.
For a solid 800, add a 2nd TTS GPU (or move VAD to GPU if CPU-bound on a smaller host).

### WebSocket / connection scaling
"WebSockets don't scale" is a myth at this scale. Each call is **one** WS
carrying 8 kHz µ-law (~64 kbps each way). Go handles **tens of thousands** of
concurrent goroutine-backed connections; 500–600 is nothing. The bottleneck is
**GPU compute, not the socket layer.** Practical notes:
- Raise `ulimit -n` (done in `server-start.sh`) so FDs don't cap you.
- WS is inherently **sticky** — a call's media stays on the instance that owns
  it. That's fine: pipeline state is per-connection anyway.
- **Scale horizontally by GPU capacity, not connection count.** When one box is
  GPU-bound, run more server instances (each with its own GPU services) and split
  calls across them; Telnyx can route per number, or put a WS-aware load balancer
  in front. No shared state between calls means this is straightforward.

---

## 9. Errors we hit & fixes

| # | Symptom | Cause | Fix |
|---|---------|-------|-----|
| 1 | GPU containers crash: **"no kernel image available for execution on the device"** | Prebuilt CUDA images don't include Blackwell/sm_120 (RTX 5090) kernels | Build our own images on **`pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime`** (torch 2.9 + cu128). Verified `torch.cuda` works on the 5090. |
| 2 | `docker build` fails: **"2B transferring dockerfile"** / `/var/lib/snapd/void/Dockerfile` not found | Snap-confined Docker can't read `/tmp` as a build context | Build from a normal **home directory**, not `/tmp`. (Repo `deploy/*` dirs are fine.) |
| 3 | `docker run --gpus ...` errors on some hosts | `--gpus` flag not supported by that host's runtime | Use **`--runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=<n>`** (also pins each service to one card). |
| 4 | Ollama ~5.7 s per turn | Model swapping / reload thrash | `OLLAMA_MAX_LOADED_MODELS=3`, `OLLAMA_NUM_PARALLEL=2`, `OLLAMA_KEEP_ALIVE=-1`, `OLLAMA_CONTEXT_LENGTH=4096`. (We later moved the LLM to SGLang.) |
| 5 | Ollama hangs on start (Blackwell) | Vulkan backend | `OLLAMA_LLM_LIBRARY=cuda_v13`, `OLLAMA_VULKAN=0`. |
| 6 | Kokoro-FastAPI GPU image fails on Blackwell | same as #1 (prebuilt kernels) | Custom Kokoro container (`deploy/tts`) on the cu128 base. |
| 7 | Browser UI: `data_frames.proto` / `protobuf.min.js` **404** | UI opened as loose files / wrong base path | UI is now **embedded (`go:embed`) and served at `/`**; browser WS moved to `/ws`. Whole app served from the tunnel URL. |
| 8 | Bot voice **"breaks"/garbled at the start of every utterance** | `SynthesizeStream` handed the reused HTTP read buffer into async audio frames; the next read overwrote in-flight audio | **Copy** each chunk into a fresh slice before pushing it downstream. |
| 9 | Bot audio **breaks mid-sentence** | Fast-first aggregator split on commas → each clause synthesized separately, audible seams | Split only on true sentence boundaries (`.?!`), not commas/colons. |
| 10 | Bot **answers half-sentences** on thinking pauses | `vad.stop_secs=0.4` ends the turn too eagerly; filler words ("uh") trigger turns | Raised to **0.6** and added a **filler-only transcript filter** (drop "uh/um/hmm"). |
| 11 | "705 ms ASR→LLM gap" | **Measurement artifact** — a log printed at chat *end*, not turn time | Added per-stage `STAGE` instrumentation; real transit ≈ 0 ms. |
| 12 | Semantic turn-gate over-holds live ASR (marks everything incomplete) | Gate too aggressive on partial transcripts | **Disabled** (`turn_gate_enabled: false`); rely on `stop_secs`. |
| 13 | Moonshine ASR worse on real voice than SenseVoice | Synthetic-audio benchmarks misled | Reverted; later moved to **Parakeet** (best English WER + no silence hallucinations). |
| 14 | Load-test handler **hangs**, pool instances **leak** (110→101) | Pipeline `task.Run()` never returns when a synthetic conn closes (nothing cancels it) | Session takes a **stop channel**; on close it calls `task.Cancel()` so `Run()` returns and pool defers fire. |
| 15 | Load-test latency **p50 = 15 ms** (bogus) | Latency hook started its timer on the bot's own echo / barge-in mid-reply | Only start the timer on a **clean turn** (bot idle). |
| 16 | Load-test p90 stuck at ~4.5 s at *every* concurrency | Synthetic sessions were **phase-locked** → synchronized LLM bursts | **Desynchronize** session phases so requests arrive steadily (like real calls). |
| 17 | Apparent "duplicate ASR emission" at high concurrency | **Misdiagnosis** — it was 100 sessions speaking the same rotating clips in the same window | Confirmed clean with a single-session run; no code change. |
| 18 | Windows `go build` fails: *build constraints exclude all Go files in sherpa-onnx-go-windows* | sherpa CGO libs are Linux-only in this setup | Build the server **on the Linux GPU box**, not on Windows. |

---

## 10. Operating cheatsheet

```bash
# bring everything up (after images are built + config in place)
bash deploy/scripts/sglang-start.sh
bash deploy/scripts/kokoro-start.sh
bash deploy/scripts/parakeet-start.sh
bash deploy/scripts/tunnel-start.sh
bash deploy/scripts/server-start.sh

# logs
docker logs -f sglang | parakeet | kokoro-tts
tail -f server-run.log

# restart just the Go server after a code change
REBUILD=1 bash deploy/scripts/server-start.sh

# load test (needs pool_size >= N in config.yaml)
curl -s "http://127.0.0.1:4321/api/loadtest?n=100&secs=60"

# stop
pkill -f "server-bin -config"; pkill -f "cloudflared tunnel"
docker rm -f sglang kokoro-tts parakeet
```

See `deploy/` for the container sources, launcher scripts, and config templates.
