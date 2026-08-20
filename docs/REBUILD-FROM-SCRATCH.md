# Rebuilding the box

The original GPU box (38.65.239.47) was lost on 2026-08-14. Everything in this
repo survived; three things never lived here and have to be supplied again.

## 1. Secrets — you must fetch these, they are not recoverable from git

Copy the examples and fill them in:

| file | holds | where to get it |
|---|---|---|
| `rexa-secrets.env` | `REXA_OUTBOUND_HMAC_SECRET`, `REXA_INBOUND_HMAC_SECRET` | the platform (Rexa) side — both ends must match |
| `rexa-secrets.env` | `DAILY_API_KEY` | Daily dashboard. Ours, not a tenant's |
| `rexa-secrets.env` | `REXA_REDIS_PASSWORD` | managed Redis, only needed when a dispatch omits it |
| `telnyx.env` | `TELNYX_API_KEY` | Telnyx portal. Note per-tenant keys arrive in the dispatch body, so this one is only for account-level calls |

`contract-start.sh` refuses to start without the two HMAC secrets, which is the
intended behaviour: the contract endpoints would otherwise register unsigned.

## 2. config.yaml — the values that were actually running

`deploy/config.yaml.example` is the documented template. These are the settings
the lost box ran, verified by reading its live config during the 14 Aug session.
Anything not listed was the example's default.

```yaml
server:
  addr: ":4399"
  max_gpu_calls: 61          # MEASURED on 4x RTX 5090 — re-measure on new hardware
  max_total_calls: 200
  human_answer_weight: 1.0
  force_voice_id: 3          # every call pinned to af_heart while a voice was tuned
  dial_timeout_secs: 45
  voicemail_detection: premium
  live_room_prepublish: true
  turn_gate_enabled: false
  turn_gate_model: llama3.2:3b
  turn_gate_max_wait_secs: 2.5
  first_chunk_words: 0

tts:
  model: kokoro_http
  http_url: "http://127.0.0.1:8880"
  speaker_id: 3              # af_heart, the only Grade-A voice
  speed: 1.1                 # was 1.28; lowered 14 Aug and never tested on a call
  gain: 1.8                  # tuned for kokoro, which is quiet. Do not reuse for another engine
  pool_size: 240

vad:
  stop_secs: 0.5             # half of the ~1s per-turn latency is this
```

**`max_gpu_calls: 61` is hardware-specific.** It was measured on four RTX 5090s
(~7.2 TB/s aggregate bandwidth). Re-run `deploy/loadtest` before trusting it
anywhere else — see docs/RESOURCES.md.

## 3. Bring the stack up

```bash
bash deploy/scripts/deps-install.sh
bash deploy/scripts/up-voice-4gpu.sh     # builds kokoro + parakeet images, starts sglang x2
bash deploy/scripts/sidecar-install.sh   # only if browser calls are needed
bash deploy/scripts/contract-start.sh    # builds and starts the agent, prints a preflight
```

`up-voice-4gpu.sh` builds `kokoro-gpu:local` and `parakeet-gpu:local` from
`deploy/tts` and `deploy/asr`, so no image registry is needed. Model weights
re-download on first run.

Read the preflight block `contract-start.sh` prints. Every optional feature
degrades SILENTLY when its key is missing — no Daily key means no listen-in and
no browser calls, with no error anywhere.

## What was lost and is not coming back

- **The call logs.** ~419k lines covering every call. The numbers derived from
  them are preserved in docs/RESOURCES.md, the memory notes and the commit
  messages, but the raw material is gone.
- **Built docker images and model caches.** Both rebuild from this repo.
- **`config.yaml.kokoro`**, a hand-made rollback copy. Superseded by the block
  above.

## Secrets status (as of 14 Aug, after the box was lost)

Held locally in `deploy/rexa-secrets.env` and `deploy/telnyx.env`, both
gitignored. Copy them to the new instance by hand — scp them, do not commit
them.

| variable | status |
|---|---|
| `REXA_OUTBOUND_HMAC_SECRET` | have it |
| `REXA_INBOUND_HMAC_SECRET` | have it |
| `TELNYX_API_KEY` | recovered from `Agentv2/demo.env`. 58 chars, the same length the lost box ran |
| `TELNYX_APP_ID` | recovered, `2580092767426316174` — matches the connection id seen in the running config |
| `TELNYX_FROM_NUMBER` | `+18483010124`, from the agent's own startup line |
| `DAILY_API_KEY` | recovered from `Agentv2/demo.env` — **verify it is current**, that file belongs to another project and may hold a rotated key |
| `REXA_REDIS_PASSWORD` | **still missing.** Only needed when a dispatch names a Redis host without a password; live call events fail silently without it |
| `AGENT_BASE_URL` | not a secret. The tunnel hostname changes on every restart and `contract-start.sh` sets it |

A `.gitignore` hole was closed at the same time: `deploy/rexa-secrets.env` was
not matched by any rule, so a `git add -A` would have committed the HMAC
secrets that authenticate every platform callback. The rules now cover `*.env`
with an exception for `*.env.example`.
