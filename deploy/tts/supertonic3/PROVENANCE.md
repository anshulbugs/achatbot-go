# Supertonic-3 — vendored model weights

These are third-party model weights, mirrored into this repository deliberately.

## What is here and what is not

The four ONNX graphs (~380MB) are **not** committed. achatbot-go is a public
fork, and GitHub bills git-lfs storage to the fork parent, so it rejects LFS
uploads from forks: *"can not upload new objects to public fork"*. No repo-side
setting changes that.

Run `./fetch-weights.sh` to download them and verify against `SHA256SUMS`. Point
`SUPERTONIC_MIRROR` at our own copy rather than relying on upstream.

Committed here: the ten voice styles (2.9MB, small enough for plain git),
`config.json`, `LICENSE`, `SHA256SUMS`, and this file.

## Why these files are in git

Supertone archived the Supertonic project on **2026-07-23**:

> This repository will be archived, and there will be no further development or
> official support for the open-source Supertonic models.
> Voice Builder will no longer be accessible after August 31, 2026.

Nothing obliges Supertone to keep hosting the weights on Hugging Face. Since we
cannot rebuild or re-derive them and there will never be another release, the
only way to guarantee we can still deploy this model in a year is to hold our
own copy. That is worth ~386MB of LFS.

## Provenance

| | |
|---|---|
| Source | `https://huggingface.co/Supertone/supertonic-3` |
| Mirrored | 2026-08-06 |
| Upstream code | `https://github.com/supertone-inc/supertonic` (archived) |
| Licence | OpenRAIL-M (see `LICENSE`) |

Verify integrity against `SHA256SUMS` with `sha256sum -c SHA256SUMS`.

## Licence obligations — read before redistributing

The weights are OpenRAIL-M, **not** Apache-2.0 like Kokoro. Commercial use is
permitted, but the licence carries use-based restrictions that travel with the
files. Two matter to us in practice:

1. **Machine-generated content must be intelligibly disclaimed.** Our agent
   greeting already identifies itself as an AI assistant, which satisfies this —
   but that disclosure is now a licence condition, not a courtesy, and must not
   be removed from a tenant's greeting.
2. **No impersonation without consent.** We ship only the ten preset voices and
   have no cloning path, so this is satisfied by construction. It would stop
   applying the moment anyone wires up a custom voice.

Anyone we distribute these files to must receive a copy of `LICENSE` and be
bound by the same restrictions. This repository is public, so `LICENSE` must
stay alongside the weights.

## Contents

```
onnx/text_encoder.onnx        36MB   text -> latent
onnx/duration_predictor.onnx   4MB   phoneme durations
onnx/vector_estimator.onnx   257MB   flow-matching field (the total_steps loop)
onnx/vocoder.onnx            101MB   latent -> 44.1kHz waveform
onnx/tts.json                        runtime graph config
onnx/unicode_indexer.json            text -> token index table
voice_styles/{F1..F5,M1..M5}.json    speaker embeddings, ~290KB each
```

## Known limitations of this model

- **Expression tags reach the model, but their effect is unverified.** The model
  card advertises `<laugh>`, `<breath>` and `<sigh>`. The text pipeline does
  *not* strip them: `_preprocess_text` removes only `[♥☆♡©\]`, and
  `_text_to_unicode_values` passes every character through as `ord(char)`, so
  the model receives `<en>Hello <laugh> world.</en>` verbatim — the same
  character-level form as the `<en>…</en>` language tokens. Tagged output is
  measurably different from untagged (about +0.28s for `<laugh>`).

  What is *not* established is whether the model produces actual laughter. An
  invented tag such as `<xyzzy>` changes the audio by a comparable amount and
  with comparable energy, so duration and RMS cannot distinguish a learned
  expression token from the model simply vocalising unfamiliar characters. ASR
  is no help either: Parakeet transcribes tagged output as if the tag were
  absent, which is equally consistent with real laughter (which it would not
  transcribe) and with a meaningless noise.

  Resolving this needs a listening comparison of `<laugh>` against `<xyzzy>` —
  see `voice-samples/supertonic3-tags/`. Do not build features on tags until
  that is settled.
- **The upstream Python package hard-codes `CPUExecutionProvider`.** Sessions
  must be rebound to CUDA explicitly or throughput drops ~40x (126x realtime ->
  3x). See `deploy/tts/supertonic/server.py`.
- **ONNX Runtime's CUDA provider allocates a cuBLAS handle per thread.** Calling
  it from many threads fails with `CUBLAS failure 3: resource allocation
  failed`. It must sit behind a bounded worker pool.
- No further upstream fixes will ever arrive for any of the above.
