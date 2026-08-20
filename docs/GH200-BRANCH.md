# The `gh200` branch

`main` runs the 4x RTX 5090 layout and stays the production branch. This branch
holds the Grace-Hopper port. Nothing here changes how the 5090 box behaves.

## The rule that keeps this from becoming a fork

**Anything architecture-neutral goes to `main` first, then arrives here by
merge. Only genuinely platform-specific things are committed on this branch.**

The Go agent gets fixes weekly — barge timing, prompt rules, transcripts,
webhooks. If those land twice, they will drift, and the drift will be found
during a live campaign. So the test before every commit here is one question:
*would this also be correct on the 5090 box?* If yes, it does not belong on this
branch.

Already applied that way: `deps-install.sh` fetching Go and cloudflared for the
running architecture instead of hardcoding amd64 (`77a29ed` on main). It is
needed for Grace, and it is correct on x86, so it went to main.

## What actually differs, and why

| File | Change | Reason |
|---|---|---|
| `deploy/tts/Dockerfile` | base `pytorch/pytorch:2.9.0-cuda12.8` → `nvidia/cuda:12.8.1-cudnn-runtime-ubuntu24.04`, torch from the cu128 index | the pytorch image has **no arm64 manifest at all** — it cannot be pulled on Grace |
| `deploy/asr/Dockerfile` | same base swap, plus build tools | same, and NeMo's tree may compile from source on aarch64 |
| `deploy/scripts/up-voice-gh200.sh` | new | one GPU, one LLM replica, no load balancer |
| `deploy/scripts/sglang-start.sh` | `--mem-fraction-static` from `$MEM_FRACTION` | the LLM cannot take 85% of a device it shares with ASR and TTS |

The `sglang-start.sh` change defaults to the old value and is a merge-back
candidate; it is here only to keep this branch's diff self-contained.

## What was checked before starting, and what it ruled out

- **`lmsysorg/sglang:latest` publishes linux/arm64.** LLM serving needs no
  change beyond the memory fraction.
- **`sherpa-onnx-go-linux` ships prebuilt aarch64 libs** in
  `lib/aarch64-unknown-linux-gnu`, selected by `build_linux_arm64.go`. The CGO
  link the agent depends on works unchanged. This was recorded as the main
  ARM64 risk; it is not one.
- **`pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime` is amd64-only.** This is the
  real blocker, and it is the whole reason two Dockerfiles differ.

## What is not known and must be measured here

- **Capacity.** `max_gpu_calls: 61` was measured on 4x RTX 5090 and carries no
  information about this box. `up-voice-gh200.sh` writes `20` and says to raise
  it from a loadtest, not from the old number.
- **`LLM_MEM_FRACTION`.** 0.75 is arithmetic, not measurement. Watch
  `nvidia-smi` under load.
- **Whether NeMo builds on aarch64 at all.** If the ASR image fails, read which
  package it stopped on before blaming the base image.
- **ollama for the CPU sentiment classifier.** Needs an arm64 build; the script
  degrades to no-sentiment rather than failing the bring-up.

## Bring-up

```bash
bash deploy/scripts/up-voice-gh200.sh
python3 deploy/scripts/verify-speech-markup.py   # speech markup, pauses, sounds
python3 deploy/loadtest/ttsbench.py 30 90        # then asrbench.py
# set server.max_gpu_calls from that, restart
```

Secrets are not in git. Copy `rexa-secrets.env` and `telnyx.env` across by hand
— see `docs/REBUILD-FROM-SCRATCH.md`.

## When this branch ends

When the GH200 layout is measured and chosen for production, merge it into
`main` and keep both bring-up scripts side by side (`up-voice-4gpu.sh` and
`up-voice-gh200.sh`), with the Dockerfile base selected per architecture rather
than per branch. The branch exists for the bring-up, not forever.
