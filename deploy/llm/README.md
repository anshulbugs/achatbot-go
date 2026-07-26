# LLM serving: tuning and horizontal scaling

The LLM is the **first thing that saturates** under concurrency — ASR and TTS have large
headroom long after the LLM is pinned. This covers how it's tuned, how to add replicas,
and what we measured.

Default model: `RedHatAI/Qwen2.5-7B-Instruct-FP8-dynamic` on SGLang 0.5.16 (see §4b for
why 7B-FP8 rather than 3B).

---

## 1. Why the LLM saturates first

A single call is cheap. What kills you is **turn rate × prompt size**:

- Each turn re-sends the **system prompt + `chat_history_size` turns**. A long system
  prompt (ours is ~500 tokens) is re-prefilled on *every* turn of *every* call.
- Replies are 2–4 sentences, so decode is non-trivial too.
- ASR/TTS work is per-utterance and cheap by comparison.

The greeting is **not** a factor — it goes straight to TTS and never reaches the LLM.

> **Agent-to-agent load tests are a worst case, not a typical one.** Two bots talk with
> ~100% duty cycle: neither pauses to think or listen, so a "call" produces several times
> the LLM load of a human call, where the agent typically talks 25–30% of the time.
> Numbers below are from that worst case — treat them as a floor, not a forecast.

---

## 2. Tuned launch flags

```bash
docker run -d --name sglang \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=6 --shm-size 8g \
  -p 127.0.0.1:8001:8000 \
  -v $PWD/hf-cache:/root/.cache/huggingface \
  lmsysorg/sglang:latest \
  python3 -m sglang.launch_server \
    --model-path Qwen/Qwen2.5-3B-Instruct --host 0.0.0.0 --port 8000 \
    --context-length 4096 \
    --mem-fraction-static 0.85 \
    --cuda-graph-max-bs 256 \
    --schedule-policy lpm
```

| Flag | Default we started with | Tuned | Why |
|---|---|---|---|
| `--mem-fraction-static` | 0.6 | **0.85** | 0.6 left **11.6 GB idle** on a 32 GB card. Raising it grew usable KV cache from 364k to **588k tokens (+61%)**, so more requests batch concurrently. |
| `--cuda-graph-max-bs` | 24 (default) | **256** | Decode batches larger than this fall off the CUDA-graph fast path. At 100 concurrent agents we were far past 24 on every step. |
| `--attention-backend` | `triton` (explicit) | *(omit)* | SGLang picks **flashinfer** on Blackwell automatically; the explicit `triton` was the slower path. |
| `--schedule-policy` | `fcfs` | **`lpm`** | Longest-prefix-match scheduling groups requests that share our long system prompt, improving prefix-cache reuse. |
| `--context-length` | 4096 | **32768** | ⚠️ **Do not lower below your real prompt size.** See the trap below. |

Prefix caching (RadixAttention) is **on by default** (`disable_radix_cache=False`) and matters
a lot here — every agent shares the same system prompt, so it is prefilled once and reused.

### ⚠️ Trap: `--context-length` too low returns HTTP 400 on every request

Setting `--context-length 2048` looked like a cheap way to double concurrency. Instead the
system prompt + history exceeded it and SGLang rejected **every** request with
`400 Bad Request`. Symptoms are misleading:

- LLM GPU sits at **0% utilization** (no work is ever done)
- Call latency pins at a flat **~7 s** (the client timing out and falling back)
- `/v1/models` still returns 200, so naive health checks look fine

Diagnose with: `docker logs sglang | grep '400 Bad Request'`.

Size it as: `system prompt + chat_history_size × avg turn + max_tokens`, plus margin.

### Measured effect of the flags (same GPU, both warm)

| Concurrency | Config | TTFT p50 | Total p50 | Throughput |
|---|---|---|---|---|
| 40 | before | 1315 ms | 2184 ms | 18.3 req/s |
| 40 | **tuned** | 1322 ms | **1796 ms** | **27.6 req/s** (+51%) |
| 100 | before | 2379 ms | 4016 ms | 28.5 req/s |
| 100 | **tuned** | **1981 ms** | **2791 ms** | **38.3 req/s** (+34%) |

Benchmark with `deploy/loadtest/llmbench.py` (see §5 for its caveats).

---

## 3. Horizontal scaling: replicas behind a load balancer

A 3B model fits comfortably on one card, so **data parallelism beats tensor parallelism**:
run one full replica per GPU and balance across them. No Go changes are required — point the
server at a proxy instead of a backend.

```bash
# replica 2 on another GPU, different host port
docker run -d --name sglang2 \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=1 --shm-size 8g \
  -p 127.0.0.1:8002:8000 \
  -v $PWD/hf-cache:/root/.cache/huggingface \
  lmsysorg/sglang:latest \
  python3 -m sglang.launch_server --model-path Qwen/Qwen2.5-3B-Instruct \
    --host 0.0.0.0 --port 8000 --context-length 4096 \
    --mem-fraction-static 0.85 --cuda-graph-max-bs 256 --schedule-policy lpm

# nginx in front (config: deploy/llm/nginx-llm-lb.conf)
mkdir -p ~/lb && cp deploy/llm/nginx-llm-lb.conf ~/lb/default.conf
docker run -d --name llm-lb --network host \
  -v ~/lb:/etc/nginx/conf.d:ro nginx:alpine
```

Then in `config.yaml`:

```yaml
llm:
  base_url: "http://127.0.0.1:8011/v1"   # the balancer, not a backend
```

Notes that matter:

- **`proxy_buffering off` is mandatory.** Token streaming is SSE; with buffering on, nginx
  holds the response and first-audio latency collapses into one late burst.
- `least_conn` suits variable-length generations better than round-robin.
- On snap-installed Docker, bind-mounting a *file* from `/tmp` fails. Mount a **directory**
  from `$HOME` (as above).
- Pick a free port. On our box `8010` was already taken; we used `8011`.

Verify it actually streams before trusting it:

```bash
curl -sN http://127.0.0.1:8011/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-3B-Instruct","messages":[{"role":"user","content":"say hi"}],"max_tokens":8,"stream":true}' \
  | grep -c '^data:'      # must be > 0
```

Confirm both GPUs are actually engaged during load — if one stays at 0%, traffic is not
being balanced:

```bash
watch -n2 'nvidia-smi --query-gpu=index,utilization.gpu --format=csv,noheader'
```

---

## 4. Measured impact (agent-to-agent, 50 pairs = 100 live agents)

| | 1 GPU, default | 1 GPU, tuned | **2 GPUs, tuned** |
|---|---|---|---|
| turn latency p50 | 2691 ms | 2763 ms | 2774 ms |
| turn latency p95 | 6648 ms | 5635 ms | 6593 ms |
| **audio-send errors** | 40 233 | 13 070 | **1 089 (−97%)** |

The headline is **reliability, not latency**: dropped audio writes fell by 97%. Latency at
100 continuously-talking agents stays ~2.7 s p50 — that needs *more capacity*, not more
tuning. Scale-up curve on one tuned GPU:

| Load | p50 | p95 | verdict |
|---|---|---|---|
| 10 agents | 951 ms | 1.2 s | ✅ healthy |
| 40 agents | 1822 ms | 4.0 s | ⚠️ degraded |
| 100 agents | 2676 ms | 6.6 s | ❌ over capacity |

Rule of thumb from these runs: **~10–15 continuously-talking agents per tuned GPU** for
sub-second turns. Human calls are far lighter — budget by measuring your own traffic.

---

## 4b. Model choice: why 7B-FP8, not 3B

**Qwen2.5-3B could not hold the instruction set.** With a 1.8k-word conversational prompt it
collapsed into a fixed template — acknowledge the caller's words, then a stock closing
question — and violated explicit rules on *every* turn:

| Prompt said | 3B did |
|---|---|
| "Never reuse a stock closing line" | "Need to do anything else?" ×5 in one call |
| "Do not end every turn with a question" | ended every turn with a question |
| "Never mention transcription" | "Sometimes when I'm processing speech, it can get a bit garbled" |

Four prompt rewrites produced the same failure. That is a capacity ceiling, not a prompt
problem — stop rewriting the prompt and change the model.

**FP8, not bf16.** Concurrency here is bound by KV cache, and KV cache = VRAM − weights.
Quantizing the weights buys the capacity straight back:

| Config | KV cache / replica | Warm TTFT | Est. agents (2 replicas) |
|---|---|---|---|
| 3B bf16 | 588k | 38 ms | ~60 |
| 7B bf16 | 220k | 33 ms | ~22 |
| **7B FP8** | **334k** | **44 ms** | **~60** |

`RedHatAI/Qwen2.5-7B-Instruct-FP8-dynamic` — Blackwell (sm_120) has native FP8, so it costs
essentially no latency. **Quantize weights only**: FP8 *KV cache* has been reported slower on
SM120 due to extra quantize/shuffle kernels.

**Avoid reasoning / "thinking" models entirely.** Thinking tokens are emitted before the
answer, and TTFT is the only latency that matters for voice — TTS starts on the first token.

### Prompt length is cheap; prompt *count* is not

| System prompt | Warm TTFT | Concurrency impact |
|---|---|---|
| 218 words | 30 ms | baseline |
| 1,352 words | 35 ms | none measurable |
| 17,255 words (~23k tok) | 87 ms | ~25 agents instead of ~60 |

Prefix caching makes a long prompt nearly free *within* a call: it is prefilled once and
reused every turn. But caching is per-identical-prefix, so if every caller supplies their own
long prompt there is no sharing *across* calls, and each call's prompt occupies KV cache for
its whole duration. Long prompts cost concurrency, not latency.

⚠️ `--context-length` must exceed system prompt + history + max_tokens, or SGLang rejects
**every** request with HTTP 400 while `/v1/models` still returns 200. See the trap in §2.

## 5. Further levers (not yet applied)

Ordered by expected value for this workload:

1. **More replicas.** Cheapest and most predictable. Add one per free GPU.
2. **Shorten the system prompt.** It is re-prefilled every turn of every call. Cutting a
   500-token prompt in half is a direct prefill saving across the whole fleet.
3. **Cap `max_tokens`.** Voice replies should be short; an uncapped generation ties up a
   decode slot far longer than the caller will listen.
4. **FP8 weights.** Blackwell (sm_120) has native FP8, and SGLang supports FP8/ModelOpt
   quantization. Expect lower memory and higher throughput on the same card. Note that FP8
   **KV cache** has been reported to *slow down* SM120 due to extra quantize/shuffle kernels —
   quantize weights first and measure before touching KV cache dtype.
5. **Smaller model.** Qwen2.5-1.5B roughly doubles throughput if quality allows.
6. **Speculative decoding** (EAGLE) for decode-bound workloads.

---

## 6. Files

| File | Purpose |
|---|---|
| `nginx-llm-lb.conf` | Least-conn balancer across replicas, SSE-safe |
| `../loadtest/llmbench.py` | Concurrent TTFT/throughput benchmark against one endpoint |

Sources: [SGLang SM120 optimization](https://github.com/sgl-project/sglang/issues/19637),
[FP8 & ModelOpt quantization](https://deepwiki.com/sgl-project/sglang/9.2-fp8-and-modelopt-quantization),
[FlashInfer FP4/FP8](https://deepwiki.com/flashinfer-ai/flashinfer/4.3-fp4-and-fp8-quantization)
