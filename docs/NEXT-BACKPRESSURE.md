# Latency-driven backpressure — built, not yet calibrated

Was a spec; now a status page. The mechanism is in and tested. The thresholds
are still estimates, and that is the one thing left.

## The requirement, stated by the operator

> If the platform sends ten calls whose prompts are entirely different and the
> KV cache cannot handle it, `/health` must turn `accepting: false`
> **immediately** — even if that means the claimed concurrency drops from 61 to
> 6.

Read as a priority order, because it inverts the usual instinct:

**Serving 6 calls well beats accepting 61 and serving all of them badly.**

`max_gpu_calls: 61` was measured with ~3k prompts and good prefix sharing. When
the workload is heavier than the one it was measured under, the honest response
is to advertise less capacity — not to keep accepting on the strength of a
number that no longer applies.

## What was built

**First-turn TTFT is its own signal** (`pkg/rexa/metrics.go`). One sample per
call: the wait between the caller finishing their first sentence and the first
word of the reply. It gates `accepting` independently of every count, so it can
refuse at 10 calls against a ceiling of 61.

Why it needed separating, measured at 60 concurrent calls with 3k prompts:

| | one campaign | a different prompt per call |
|---|---|---|
| p95 across all turns | 1725 ms | 6252 ms |
| **p95 of turn 1** | **1853 ms** | **9903 ms** |
| p95 of turn 8 | 727 ms | 4035 ms |

Turn 1 is the only turn that pays a cold prefill. Pooled with cheap warm turns
it lands at 6252 ms — under any threshold that would have fired.

**It trips on one sample, not a percentile.** Ten unrelated prompts produce ten
samples in total; waiting for a comfortable count means answering "send more"
twice before reacting. `first_turn_critical_ms` trips on a single first turn.

**It is a duty cycle, not a latch.** The window is fed only by new calls, so a
gate that stayed shut until the numbers recovered would cut off the samples that
could prove recovery and never reopen. It shuts for the cooldown, reopens,
re-measures on fresh calls, trips again if the workload is still too heavy.
Under sustained overload that settles into admitting a trickle — which is the
behaviour asked for.

**The LLM tier had no samples at all.** It was never wired to the transport, and
would have been wrong if it had been: with SSE the HTTP round trip returns when
response headers arrive, before the model has produced anything, so a
transport-timed LLM tier reports single-digit milliseconds while the caller
waits ten seconds. It is now fed real per-turn TTFT and its thresholds moved to
match what is actually being measured.

**SGLang's own metrics are polled** in the background (`pkg/rexa/sglang.go`) and
shown on `/health` and `/dashboard`. Reported, never acted on — no calibrated
crossover exists, and gating on an uncalibrated signal refuses work for a
reading nobody has correlated with a bad call. They answer *why* latency moved:
falling cache hit rate means prompts stopped sharing prefixes, growing queue
means there are simply too many. Needs `--enable-metrics` on SGLang; without it
`/metrics` 404s while `/v1/models` still answers 200, and the panel reads "not
polling".

ASR and TTS need no new plumbing. Their limiting parameter is throughput
(Parakeet 26-36 req/s at 4 workers, Kokoro 25.7 at 8) and the symptom of
exceeding it is queueing, which the existing transport timing already captures.
They need calibrated thresholds, same as everything else here.

## What is left: calibration

`first_turn_saturated_ms: 4000`, `first_turn_critical_ms: 8000` and the tier
thresholds are derived from two measured points — 2471 ms good, 9903 ms bad —
not from a ramp across the crossover. Too high and the gate fires after callers
have already been dropped; too low and it turns away work the stack can serve.

Ramp with the tool that reproduces the bad case on demand:

```
python3 deploy/loadtest/turnbench.py <port> <calls> 8 12.4 3000 distinct
```

Raise `<calls>` until first-turn p95 crosses from acceptable to not, and set the
thresholds from where that happens rather than from where they are now.

## Related

- `docs/CALL-AGENT-CONTRACT.md` §5 — what the platform sees and must do about it.
- §9 — the two dispatch asks (per-contact block last, batch per campaign) that
  stop this arising at all. This gate is the safety net for when they are not
  honoured; `first_turn.trips` climbing while calls flow is the signal that they
  are not.
