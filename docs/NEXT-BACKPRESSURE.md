# Next: latency-driven backpressure

Spec for the next piece of work. Written down because the reasoning behind it
came from measurements that are easy to lose.

## The requirement, stated by the operator

> If the platform sends ten calls whose prompts are entirely different and the
> KV cache cannot handle it, `/health` must turn `accepting: false`
> **immediately** — even if that means the claimed concurrency drops from 61 to
> 6.

Read that as a priority order, because it inverts the usual instinct:

**Serving 6 calls well beats accepting 61 and serving all of them badly.**

The ceiling is not a promise. `max_gpu_calls: 61` was measured with ~3k prompts
and good prefix sharing. When the workload is heavier than the workload it was
measured under, the honest response is to advertise less capacity, not to keep
accepting on the strength of a number that no longer applies.

## Why the existing signal is not enough

The tier mechanism is already wired: any tier reporting `saturated` flips
`accepting: false` and makes `/connection` return `at_capacity`. LLM, ASR and
TTS all feed it through the same transport timing, so all three are covered.

Two gaps:

**1. First-turn latency is pooled with warm turns, which hides it.**

Measured at 60 concurrent calls, 3k prompts, one campaign:

| | value |
|---|---|
| overall TTFT p95 | 1725ms |
| turn 1 p95 | 1853ms |
| turn 8 p95 | 727ms |

And the same 60 calls with a different prompt each:

| | value |
|---|---|
| overall TTFT p95 | 6252ms |
| **turn 1 p95** | **9903ms** |
| turn 8 p95 | 4035ms |

Turn 1 is where KV pressure shows first and worst — it is the only turn that
pays a cold prefill. Pooling it with cheap warm turns drags the number below
any threshold that would have fired. **First-turn TTFT has to be its own
tracked series**, and it is the one that should drive `accepting`.

It is also the turn that matters to the caller: the pause after they say
"hello". Ten seconds of silence there is where people hang up.

**2. The thresholds are estimates, not measurements.**

`DefaultThresholds` in `pkg/rexa/metrics.go` — ASR 400/900ms, LLM 900/2000ms,
TTS 700/1600ms — were derived from a turn budget, not measured. They could fire
early or never. They need a real ramp before they are trusted to turn traffic
away.

## What to build

**a. Track first-turn TTFT separately.** A distinct window, fed only by the
first LLM request of each call. Surface it on `/health` and `/dashboard`
alongside the tier states.

**b. Drive `accepting` from it.** Above a configured threshold, stop accepting
regardless of how few calls are in flight — that is exactly the 61-to-6 case.
Keep the existing count-based and tier-based conditions; this is another
independent reason to say no.

**c. Poll SGLang's own metrics in the background.** Cache hit rate and
running/waiting queue depth are a direct read of KV pressure rather than an
inference from latency. Must be a background ticker with a cached value —
`/health` is probed every 5s fleet-wide and must never fan out to downstream
services, or a slow LLM takes the agent out of rotation instead of merely
slowing calls.

**d. Same treatment for ASR and TTS.** Their limiting parameter is throughput
(measured: Parakeet 26-36 req/s at 4 uvicorn workers, Kokoro 25.7 at 8), and
the symptom of exceeding it is queueing, which the existing transport timing
already captures. They need calibrated thresholds, not new plumbing.

**e. Recovery must work.** Whatever turns `accepting` false has to turn it back
on when load drops, without oscillating. The 256-sample window damps this
today; a first-turn window will be much smaller (one sample per call) and needs
its own thought — likely a minimum sample count plus hysteresis.

## Calibration

The thresholds cannot be finished from a desk. Ramp with
`deploy/loadtest/turnbench.py` in `distinct` mode, which reproduces the bad
case on demand, and find where first-turn TTFT crosses from acceptable to not.
Somewhere around 2-3s is the plausible starting point — 2471ms was measured as
the good case and 9903ms as the bad one — but that is a guess until measured.

## Related

- `docs/CALL-AGENT-CONTRACT.md` §9 — the two platform-side asks (prompt layout,
  campaign batching) that prevent this situation arising in the first place.
  Backpressure is the safety net for when they are not honoured.
- `deploy/loadtest/turnbench.py` — produced every number above.
