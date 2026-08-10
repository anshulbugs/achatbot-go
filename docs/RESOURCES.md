# What this agent needs to run

Measured on the box, not estimated. Where a number was measured, how is said.

## The machine

| | |
|---|---|
| GPUs | 8 × RTX 5090, 32 GB each — **4 are used by the voice stack** |
| CPU | 255 cores |
| RAM | 503 GB (191 GB in use, 311 GB available) |
| Disk | 879 GB, **99% full — 13 GB free** |

The disk is the one number here that is a problem. `/var/snap` and `/root/.cache`
hold most of it. Nothing in this document assumes it gets fixed, but a model
pull or a Docker build will fail before anything else does.

## The concurrency ceiling is 61 calls, and it is GPU-bound

`max_gpu_calls: 61` in `config.yaml`. It is not a guess: past 61 concurrent
agents, p95 latency doubles. `max_total_calls: 200` exists as a second bound but
`max_gpu_calls` always binds first.

The four GPUs carry:

| service | GPUs | what it serves |
|---|---|---|
| sglang, sglang2 | 2 | the LLM, one instance per GPU |
| parakeet | 1 | ASR |
| kokoro | 1 | TTS |

**Only these three tiers decide the ceiling.** Everything else described below
is CPU and RAM, and none of it competes for a GPU slot.

## Per-call cost off the GPU

| what | when | cost |
|---|---|---|
| media socket + pipeline goroutines | every call | small; the whole server process is ~110 MB RSS |
| pre-rendered greeting/voicemail audio | every call with a room | bounded cache, 256 MB total across all calls, FIFO eviction |
| **room pre-joiner** (`live_room_prewarm`) | every **answered** call that has a listen-in room | **34 MB RSS**, one Python process, no GPU — measured |
| listen-in sidecar (`room_agent.py`) | only browser calls on `/connection_webrtc` | one Python process, carries audio |

### Does the pre-joiner affect the 61-call ceiling?

**No.** At the full ceiling it is 61 processes × 34 MB ≈ **2 GB of RAM out of
311 GB available**, and 61 processes across 255 cores. It touches no GPU, so it
cannot take a slot from ASR, TTS or the LLM. The ceiling stays where the
measurement put it.

What it does cost is **one Daily participant for the full length of every
answered call that has a room**, whether or not anyone ever barges. That is a
billing question rather than a capacity one — check it against your
participant-minute rate. Turn it off with `live_room_prewarm: false`.

## What each optional feature needs

Every one of these degrades silently when its dependency is missing. The
preflight block in `contract-start.sh` prints which are on at every start —
read it rather than assuming.

| feature | needs | without it |
|---|---|---|
| listen-in and barge | `DAILY_API_KEY` | no rooms; barge is unavailable |
| instant barge detection | Daily `participant.joined` webhook registered | falls back to the presence sweep, which lags a real join by ~5.6s |
| SIP registered before a barge | `live_room_prewarm: true` + `SIDECAR_PYTHON` + `PREWARM_SCRIPT` | barge waits ~4.8s for Daily to register the room's SIP endpoint |
| browser calls | `SIDECAR_PYTHON` + `SIDECAR_SCRIPT` (`sidecar-install.sh` creates both) | `/connection_webrtc` is refused |
| recordings | `RECORD_CALLS` + Telnyx storage, or Daily cloud recording | calls complete with no recording and no error |
| live events in Redis | redis details on the dispatch | the dialer's wallboard shows nothing |

## Where the barge time actually goes

Measured on real calls. Useful when someone reports barge as slow — it says
which part to look at.

```
0.8s   Daily room + meeting token     — prepublished, so already spent at barge
1.6s   Telnyx conference + SIP dial   — ours
0.0s   Daily SIP registration         — with live_room_prewarm on
4.8s   Daily SIP registration         — with it off
```

Registration completes **1.9–2.2s after the pre-joiner joins**, which happens as
the call is answered. So by the time anyone barges it has been ready for the
whole conversation.

The click-to-webhook gap is browser-side, in the dialer, and is not measured
here.
