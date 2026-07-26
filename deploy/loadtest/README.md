# Load testing & measured capacity

How we load-test the stack with **real phone calls** (agent-to-agent), what the numbers came
out as, and where the bottlenecks actually were.

Everything here was measured on one box: **8× RTX 5090 (32 GB), 255 CPU cores**, using 4 GPUs
for the voice stack.

---

## 1. Headline result

| Concurrent agent sessions | p50 | p90 | p95 | worst | audio-send errors | verdict |
|---|---|---|---|---|---|---|
| 30 | 965 ms | 1168 ms | 1218 ms | — | 2 | ✅ excellent |
| **60** | **1032 ms** | **1350 ms** | **1628 ms** | 2150 ms | **0** | ✅ **safe operating point** |
| 80 | 1008 ms | 2924 ms | 4348 ms | 5478 ms | 2 | ⚠️ tail degrading |
| 100 | 1112 ms | 4686 ms | 6244 ms | — | 234 | ❌ breaks |

**~60 concurrent agent sessions on 4 GPUs** (2× LLM, 1× ASR, 1× TTS).

Note the median barely moves across the whole range — **p50 is a useless health metric here**.
The tail is what callers experience: at 100 agents the median still looked like 1.1 s while
listeners heard multi-second hangs. Always judge on p90/p95.

### Reading these numbers for human traffic

The test is **agent-to-agent**, which is heavier than a human call:

- Each bot alternates speak/listen ~50/50, with **no dead air** — humans pause, hesitate, and
  talk for stretches while the agent only listens.
- Bots reply the instant VAD fires, so turns come faster. **Measured: 1 turn per 12.4 s per
  agent**, versus roughly one agent turn per 15–25 s in human conversation.

That makes agent-to-agent about **1.2–2× heavier per session**, so 60 agent sessions ≈
**75–120 concurrent human calls**. That is an extrapolation from turn rate, not a measurement —
verify against your own traffic before promising it.

### CPU: not a constraint

| Metric | Value |
|---|---|
| Go server CPU at 60 agents | **~5–6 cores** (VAD + µ-law transcode + WS fan-out) |
| Box load average at 100 agents | **26** of 255 cores (~10%) |

Every bottleneck we hit was **GPU or per-service serialisation**. There is ample CPU headroom
for post-call work (voicemail processing, webhook senders, transcript jobs) — keep it off the
audio path via a queue + worker pool so a slow customer endpoint can't stall a live call.
Note that **function calling is not free**: it adds an LLM round-trip per invocation, so it
spends GPU budget, not CPU.

---

## 2. What actually limited throughput

In order of discovery. Each was a *serialisation* problem, not a hardware limit.

| Bottleneck | Symptom | Fix | Gain |
|---|---|---|---|
| **TTS single worker** | Delayed *greeting* (greeting is pure TTS, no LLM). 8 req/s flat regardless of concurrency; p50 61 ms → 5252 ms at 50 concurrent. GPU only 12–16% busy. | `uvicorn --workers 8` | **8 → 25.7 req/s** |
| **ASR single worker** | `async def` endpoint calling `model.transcribe()` synchronously — blocks the event loop, so one request at a time. 14 req/s flat. | `uvicorn --workers 4` | **14 → 26–36 req/s** |
| **LLM under-configured** | GPU pinned at 99% while 11.6 GB VRAM sat unused; decode batches fell off the CUDA-graph path. | tuned flags (`mem-fraction-static 0.85`, `cuda-graph-max-bs 256`, flashinfer, `lpm`) | **+34–51% throughput** |
| **LLM single replica** | One GPU saturated while others idled. | 2nd replica + nginx `least_conn` LB | audio-send errors **40 233 → 489** |

A flat req/s that does not rise with client concurrency is the signature of serialisation —
look there before adding hardware.

### Worker counts are not "more is better"

ASR at **8** workers was *worse* than at **4** (20 req/s vs 26–36) — the copies contend for the
same GPU. Measure per model and per card; don't assume.

---

## 3. Scaling beyond 60

At 60 agents every tier is already **77–96% utilised** — there is no slack left, which is
exactly why 80 collapses: all four tiers queue simultaneously.

For ~100 agents, budget roughly **1.5–2× of every tier** (≈3–4 LLM GPUs, 2 ASR, 2 TTS).
Adding one tier alone will not help — the stack is balanced.

Load-reduction levers, if your prompts are under your control (ours are not — they come from
end users, so system-prompt size and `max_tokens` are fixed inputs):

1. Shorter system prompt — it is re-prefilled on *every turn of every call*.
2. Cap `max_tokens` — an uncapped generation holds a decode slot far longer than a caller listens.
3. Lower `chat_history_size`.
4. FP8 weights (Blackwell has native FP8).
5. Coarser TTS chunking — the aggregator sends one TTS request per sentence, so a 4-sentence
   reply is ~4 requests. Bigger chunks mean fewer requests but **slower first audio**; this
   trades away the latency you tuned for, so weigh it carefully.

---

## 4. Running the tests

### Prerequisites

- Telephony configured (`telnyx.env`) and the server reachable on a public URL.
- **A number whose inbound calls route to this server.** Inbound routing follows the *Call
  Control Application's* `webhook_event_url`, **not** the per-call webhook used for outbound.
  ⚠️ If that app is shared with production, pointing it here **redirects production inbound
  traffic**. Prefer a separate app + spare number. `restore_prod_webhook.sh` restores the
  original URL — back it up first.
- `server.max_call_secs` set (e.g. 150). **Two agents never hang up on each other**; without
  this, calls run forever and bill forever.
- Pools sized above your target: `vad/asr/tts pool_size` and `server.max_conns`.

### Commands

```bash
# N concurrent pairs -> N*2 agent sessions. Ramp: 5 -> 15 -> 30 -> 50.
./callramp.sh 30 +1XXXXXXXXXX 300      # <pairs> <dest number> <stagger ms>

# component benchmarks (find serialisation before blaming hardware)
python3 ttsbench.py 50 150             # <concurrency> <requests>
python3 asrbench.py 30 90
python3 llmbench.py 8001 100 200       # <port> <concurrency> <requests>

# recordings (set server.record_calls: true first)
./getrecordings.sh 10 recordings

# restore the production webhook when finished  ← do not skip
./restore_prod_webhook.sh
```

### Reading results

`callramp.sh` prints connected streams, latency percentiles and per-GPU utilisation every
20–90 s. Judge on:

- **p90/p95**, never p50.
- **`send_payload error` count** in `server-run.log` — these are *dropped audio writes*, the
  clearest signal you are over capacity.
- **GPU utilisation across all four tiers** — if one sits at 0% while another is pinned, your
  load balancing is broken, not your capacity.

---

## 5. Gotchas that cost us hours

| Symptom | Cause |
|---|---|
| Flat ~7 s latency, LLM GPU at 0%, `/v1/models` still 200 | `--context-length` below the real prompt size → SGLang 400s every request. Check `docker logs sglang \| grep 400`. |
| Greeting delayed but LLM healthy | Greeting bypasses the LLM entirely — it is pure TTS. Look at TTS queueing. |
| Second GPU idle behind the LB | Streaming broken through nginx. `proxy_buffering off` is mandatory for SSE. Verify with `curl -sN … \| grep -c '^data:'`. |
| Test calls stop working after several runs | **Session leak** — media sessions never log "ended" and never release pool slots, so pools exhaust. Restart the Go server between runs. *(Open bug.)* |
| `pkill -f callramp` kills your own SSH session | The pattern matches the ssh command itself. Use `pkill -f "callramp[.]sh"`. |
| Recordings download as tiny/empty files | Pre-signed URLs expire quickly; and if the account uses a custom S3 bucket, `download_urls` are `s3://` URIs needing AWS credentials, not HTTPS links. |

---

## 6. Files

| File | Purpose |
|---|---|
| `callramp.sh` | Places N concurrent agent-to-agent calls, samples latency + GPU during the run |
| `ttsbench.py` / `asrbench.py` / `llmbench.py` | Per-component concurrency benchmarks |
| `getrecordings.sh` | Lists and downloads Telnyx recordings |
| `restore_prod_webhook.sh` | Restores the saved production `webhook_event_url` |
