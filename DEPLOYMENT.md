# Voice Agent — Deployment Guide

A self-hosted, low-latency voice agent: **VAD → ASR → LLM → TTS** streamed over
WebSockets, with a **browser demo** and **real phone calls** (Telnyx). This guide
sets it up from scratch on any GPU box — RunPod, Vast.ai, Lambda, a bare server,
etc. — and documents every error we hit and how we fixed it.

Median wire latency achieved: **~0.9–1.3 s per turn** (sub-second is possible).

The server runs in two modes, which can be enabled together:

- **Demo mode** — the browser page and `POST /api/call`, driven by one set of
  `TELNYX_*` env vars. This is what §4 sets up.
- **Platform-contract mode** — HMAC-authenticated endpoints the Rexa platform
  dispatches to, with per-call tenant credentials, capacity backpressure and a
  live dashboard. See **[§10](#10-platform-contract-mode)** and
  **[docs/CALL-AGENT-CONTRACT.md](docs/CALL-AGENT-CONTRACT.md)**.

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

### Optional: photoreal video avatar

The browser call can also show a **photoreal talking-head avatar** driven by the
agent's own TTS audio. It runs as a separate GPU service and needs **no change to
the Go pipeline** — the browser forwards the bot audio it already receives, and the
avatar service republishes synchronized audio+video over WebRTC (Daily SFU).

```
   Go server ──audio──► Browser ──ws :8899──► SoulX-FlashHead (GPU)
                            ▲                        │
                            └────── Daily SFU ◄──────┘  (synced A/V, WebRTC)
```

~4 concurrent streams per GPU and ~1.4s added latency, so it's a **demo/premium
feature**, not the mass phone-call path. Full setup, commands, the idle-loop
recipe, and the errors we hit: **[deploy/avatar/README.md](deploy/avatar/README.md)**.

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
| Video avatar (optional) | `soulx`    | `8899`         | own GPU       | WS `/` returns a room |

Contract mode adds these on the same port as the Go server (4321):

| Path | Auth | Purpose |
|------|------|---------|
| `GET /health` | none | Liveness + capacity. Gate dispatch on `accepting` |
| `POST /connection` | HMAC | Outbound call |
| `POST /incoming` | HMAC | Answer a ringing inbound leg |
| `POST /connection_webrtc` | HMAC | Not implemented — returns 503 |
| `GET /dashboard` | none | Live capacity dashboard |

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

> **Superseded numbers.** An earlier revision of this section estimated 500–600 concurrent
> calls from the in-process synthetic harness (`/api/loadtest`). Testing with **real
> agent-to-agent phone calls** produced a far lower figure. The synthetic harness never
> exercised the ASR/TTS HTTP services under real concurrency, so it missed the fact that both
> were serialising. Trust the phone-call numbers below.

### Measured on 4 GPUs (2× LLM, 1× ASR, 1× TTS), all RTX 5090

| Concurrent agent sessions | p50 | p90 | p95 | audio-send errors | verdict |
|---|---|---|---|---|---|
| 30 | 965 ms | 1168 ms | 1218 ms | 2 | ✅ excellent |
| **60** | **1032 ms** | **1350 ms** | **1628 ms** | **0** | ✅ **safe operating point** |
| 80 | 1008 ms | 2924 ms | 4348 ms | 2 | ⚠️ tail degrading |
| 100 | 1112 ms | 4686 ms | 6244 ms | 234 | ❌ breaks |

**~60 concurrent agent sessions on 4 GPUs.** The median is flat across the entire range —
**judge capacity on p90/p95, never p50**. At 100 agents the median still read ~1.1 s while
callers heard multi-second hangs.

Because the test is agent-to-agent (no dead air, ~1 turn per 12.4 s per agent) it is roughly
**1.2–2× heavier** than a human call, so 60 agent sessions ≈ **75–120 concurrent human
calls** — an extrapolation from turn rate, not a measurement.

At 60 agents every tier is already **77–96% utilised**, which is why 80 collapses: no tier has
slack to absorb a burst. Budget **~1.5–2× of every tier** for 100 (≈3–4 LLM GPUs, 2 ASR, 2 TTS).

Full methodology, per-component benchmarks, and the bottleneck list:
**[deploy/loadtest/README.md](deploy/loadtest/README.md)**.

### The bottlenecks were serialisation, not hardware

| Bottleneck | Symptom | Fix | Gain |
|---|---|---|---|
| TTS single worker | Delayed *greeting* (greeting is pure TTS). 8 req/s flat; p50 61 ms → 5252 ms @50 conc, GPU only 12–16% busy | `uvicorn --workers 8` | 8 → **25.7 req/s** |
| ASR single worker | `async def` endpoint calling `transcribe()` synchronously — blocks the event loop | `uvicorn --workers 4` | 14 → **26–36 req/s** |
| LLM under-configured | GPU at 99% with 11.6 GB VRAM unused; decode off the CUDA-graph path | tuned flags | **+34–51%** |
| LLM single replica | One GPU pinned, others idle | 2nd replica + nginx LB | send errors **40 233 → 489** |

A flat req/s that doesn't rise with concurrency is the signature of serialisation — look there
before buying GPUs. Note **more workers is not always better**: ASR at 8 workers was *worse*
than at 4 (contention).

### LLM burst capacity (still valid)

Firing N *simultaneous* requests at one 5090 (Qwen2.5-3B, SGLang):

| Simultaneous LLM requests | TTFT p50 / p90 | Full reply p50 | Fails |
|---|---|---|---|
| 50  | 84 / 101 ms | 898 ms  | 0 |
| 100 | 118 / 172 ms | 922 ms | 0 |
| 150 | 138 / 193 ms | 1028 ms | 0 |
| 200 | 663 / 926 ms | 1251 ms | 0 |

One 5090 absorbs ~150 simultaneous requests under 200 ms TTFT. This matters when a dialer
launches many calls in the same second — throttle CPS to avoid the knee.

### CPU

| Metric | Value |
|---|---|
| Go server at 60 agents | **~5–6 cores** (VAD + µ-law transcode + WS fan-out) |
| Box load at 100 agents | **26** of 255 cores (~10%) |

CPU was never the constraint. There is ample headroom for post-call work (voicemail, webhook
senders, transcript jobs) — run it off the audio path on a queue + worker pool. **Function
calling is not free**: it adds an LLM round-trip per invocation, spending GPU budget.

### WebSocket / connection scaling
"WebSockets don't scale" is a myth at this scale. Each call is **one** WS
carrying 8 kHz µ-law (~64 kbps each way). Go handles **tens of thousands** of
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

## 10. Platform-contract mode

Everything in §4 gives you a working agent driven by the browser demo and
`/api/call`. This section turns on the endpoints the **Rexa platform**
dispatches to. The full wire spec — payloads, auth, callbacks, error codes — is
in **[docs/CALL-AGENT-CONTRACT.md](docs/CALL-AGENT-CONTRACT.md)**; this is just
the operational setup.

### 10.1 What changes

| | Demo mode | Contract mode |
|---|---|---|
| Telnyx credentials | one set, from `TELNYX_API_KEY` | **per call**, from each dispatch (multi-tenant) |
| Who places calls | you, via `/api/call` | the platform, via `POST /connection` |
| Concurrency limit | none | `max_gpu_calls` + `max_total_calls`, enforced |
| Reporting | none | end-of-call report POSTed back, with transcript |

Both can run in one process. The demo path is unchanged when contract mode is
on — it keeps using the env-built client and never touches per-call
credentials.

### 10.2 Enable it

```bash
# Both are REQUIRED and must be set together. With only one, the agent would
# either verify dispatches but never report, or report but accept
# unauthenticated calls — so it refuses to enable the contract and logs why.
export REXA_OUTBOUND_HMAC_SECRET="<32-byte hex from the platform>"
export REXA_INBOUND_HMAC_SECRET="<32-byte hex from the platform>"

# Where the CARRIER reaches us for webhooks + media. Not the platform's URL.
export TELNYX_PUBLIC_URL="https://<your-tunnel-host>"

# Optional: map the platform's voice ids onto kokoro speakers.
# Unknown voices fall back to the configured default and log once.
export REXA_VOICE_MAP="leah=3,marcus=16"
```

Note contract mode does **not** require `TELNYX_API_KEY` — credentials arrive
per dispatch. If you leave it unset, the demo endpoints simply aren't
registered, which is the right shape for a production agent.

Confirm on startup:

```
rexa: contract endpoints enabled (/health /connection /incoming /connection_webrtc),
      dashboard at /dashboard, gpu-call ceiling=61, public=https://...
```

### 10.3 Capacity configuration

```yaml
server:
  max_gpu_calls: 61          # live pipelines. MEASURED — see §8
  max_total_calls: 200       # absolute in-flight cap, incl. zero-GPU calls
  human_answer_weight: 1.0   # or "auto", or a number
```

**`max_gpu_calls: 61`** is measured, not guessed: 60 concurrent agent sessions
held p95 at 1628 ms with zero dropped audio writes, while 100 gave 6244 ms and
234 drops. It is specific to this model, prompt size and GPU layout — remeasure
with `deploy/loadtest` when any of those change.

**`max_total_calls`** is the backstop for what these counters cannot see:
carrier channel caps, CPU for hundreds of concurrent media streams, TTS renders
at dispatch time. In practice `max_gpu_calls` binds first.

**`human_answer_weight`** is the expected GPU cost of one dispatched call. A
call is dispatched while the phone is still *ringing*; its pipeline does not
exist until someone answers, up to 30 s later. So each ringing call is charged
against capacity in advance, and this is how much.

The rule is **weight ≥ the real answer rate**. Admission allows
`max_gpu_calls / weight` calls to ring at once, and whatever fraction of them
answers becomes pipelines — so a weight below the true rate creates more
pipelines than the ceiling allows, and a call a human has already picked up
cannot be refused.

| Value | Behaviour |
|---|---|
| `1.0` | Every dispatch charged a full slot. The **only** value that guarantees `on_gpu` never exceeds `max_gpu_calls`. Under-utilises when most calls hit voicemail. **Use this first.** |
| `auto` | Tracks the measured answer rate × a 2× safety margin, floored at 0.15, capped at 1.0. Starts at 1.0 until enough calls resolve. |
| e.g. `0.3` | Fixed over-subscription, once the rate is known and steady. |

A bare `0` is rejected at boot — it used to mean "adaptive", which reads as
"a ringing call costs nothing". Use `auto`.

### 10.4 Watch it

`GET /dashboard` — self-contained page, no CDN, polls every 2 s. Shows calls by
state (ringing / on GPU / voicemail), weighted GPU cost against the ceiling,
per-tier p95 with ok/degraded/saturated, and the **measured answer rate and ring
time** — the two numbers that decide whether `human_answer_weight` can move off
1.0.

`GET /health` returns the same snapshot as JSON.

Judge tiers on **p95, never p50**: at 100 concurrent sessions the median still
read 1112 ms while callers heard multi-second hangs.

### 10.5 Testing without the platform

`deploy/loadtest/` has the throughput benchmarks. For the contract surface, any
HMAC-signing client works — sign per §1 of the contract doc and POST to
`/connection`. A dispatch with deliberately wrong credentials (`provider:
"twilio"`) is a safe way to exercise auth, validation and error mapping without
placing a real call: it reaches the credential check and returns `412`.

### 10.6 Gotchas

| Symptom | Cause |
|---|---|
| Contract endpoints missing, no error | Only one of the two `REXA_*` secrets set. Check the startup log — it says so |
| `rexa: DISABLED — TELNYX_PUBLIC_URL is unset` | Carrier webhooks would have nowhere to arrive |
| All tiers stuck on `unknown` | Fewer than 20 samples yet, or telemetry initialised after the pools were built |
| `401` on every dispatch | Body re-serialised between signing and sending, or the secret hex-decoded instead of used raw |
| `at_capacity` immediately at 0 calls | `max_gpu_calls` mis-set. `0` means unlimited; a small number means a small ceiling |
| Reservations climbing, `reaped` climbing | Carrier hangup webhooks are being missed. The reaper is covering for it, but find out why |

---

## 11. Operating cheatsheet

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
