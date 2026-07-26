# Photoreal video avatar — SoulX-FlashHead + Daily WebRTC

Adds a **photorealistic talking-head avatar** to the browser call. The agent's TTS audio
drives a real person's face, generated live on a GPU and delivered to the browser over
WebRTC as one synchronized audio+video stream.

The Go voice pipeline is **not modified**. The browser forwards the bot audio it already
receives to this service; the service generates video and republishes audio+video via Daily.

- **Model:** [SoulX-FlashHead](https://github.com/Soul-AILab/SoulX-FlashHead) (Apache-2.0, 1.3B)
- **Transport:** [Daily](https://daily.co) SFU via `daily-python`
- **Runs on:** RTX 5090 / Blackwell (sm_120) natively — CUDA 12.8, no `mmcv`

---

## 1. Why this stack

| Option | Verdict |
|---|---|
| **SoulX-FlashHead** | ✅ Audio-driven, single photo, real-time, cu128 (Blackwell-native), Apache-2.0 |
| MuseTalk | ❌ Needs `mmpose`/`mmcv` — no wheels for torch 2.7/cu128; source build on sm_120 is a wall |
| 3D avatars (TalkingHead + RPM/Avaturn/MetaPerson) | ❌ Lightweight and cheap, but never truly looks like a specific person |
| Raw WebRTC (aiortc) direct to browser | ❌ Box is behind **symmetric NAT**; ICE never leaves `checking`. Needs TURN |
| **Daily SFU** | ✅ Handles NAT traversal, A/V sync and jitter buffering for us |

**Measured on one RTX 5090 (Lite model):**

| Metric | Value |
|---|---|
| Throughput | **4.6× realtime** (0.21s to generate 0.96s of video) |
| Concurrent streams | **~4 per GPU** |
| First-frame latency (server) | **~0.65s** (0.32s audio buffer + 0.31s generate) |
| First-frame latency (in browser) | **~1.4s** |
| Warmup (once at boot) | ~45s (`torch.compile`) |

`pro` model is ~7× slower (≈0.65× realtime) — **offline/pre-render only**, not live on one GPU.

> **Scaling reality:** this is a GPU-per-stream cost. Great for demos (~4 calls/GPU); it does
> **not** scale to the 500–800 concurrent phone-call path. Treat it as a premium/demo feature.

---

## 2. Install (fresh box)

Assumes Docker + NVIDIA runtime, as in the main [DEPLOYMENT.md](../../DEPLOYMENT.md).

### 2.1 Clone + weights (~15 GB)

```bash
cd ~
git clone --depth 1 https://github.com/Soul-AILab/SoulX-FlashHead.git
cd SoulX-FlashHead && mkdir -p models

python3 -m venv .hf && . .hf/bin/activate && pip install -q --upgrade pip huggingface_hub
hf download Soul-AILab/SoulX-FlashHead-1_3B --local-dir models/SoulX-FlashHead-1_3B
hf download facebook/wav2vec2-base-960h      --local-dir models/wav2vec2-base-960h
```

### 2.2 Container

The stock `pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime` image already has Python 3.11 +
CUDA 12.8, so no image build is needed — install on top of it.

```bash
docker run -d --name soulx -p 8899:8899 \
  --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=0 \
  -v /home/ubuntu/SoulX-FlashHead:/workspace/SoulX-FlashHead \
  -w /workspace/SoulX-FlashHead \
  pytorch/pytorch:2.9.0-cuda12.8-cudnn9-runtime sleep infinity
```

> Use `--runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=<n>`, **not** `--gpus` — see main deploy guide.
> Port **8899** must be published; it serves the control WebSocket.

### 2.3 Dependencies

```bash
docker exec soulx bash -c '
  set -e
  cd /workspace/SoulX-FlashHead
  pip install --no-cache-dir torch==2.7.1 torchvision==0.22.1 --index-url https://download.pytorch.org/whl/cu128
  # SoulX pins nvidia-nccl-cu12==2.27.3 which conflicts with torch 2.7.1 (needs 2.26.2).
  # It is only used for multi-GPU; drop the pin for single-GPU inference.
  grep -v "nvidia-nccl-cu12" requirements.txt > requirements_fixed.txt
  pip install --no-cache-dir -r requirements_fixed.txt
  pip install --no-cache-dir "https://github.com/Dao-AILab/flash-attention/releases/download/v2.8.0.post2/flash_attn-2.8.0.post2+cu12torch2.7cxx11abiFALSE-cp311-cp311-linux_x86_64.whl"
  pip install --no-cache-dir daily-python aiohttp requests websockets
  # runtime image lacks these: opencv/mediapipe need GUI libs, Triton JIT needs a C compiler
  apt-get update -qq && apt-get install -y -qq libgl1 libxcb1 libxrender1 libsm6 libxext6 libglib2.0-0 build-essential ffmpeg
  python -c "import torch,xformers,flash_attn,mediapipe,cv2,daily; print(\"ok\", torch.__version__, torch.cuda.get_device_name(0))"
'
```

### 2.4 Daily credentials

Create a [Daily](https://dashboard.daily.co) account and copy the API key.

```bash
cp deploy/avatar/daily.env.example ~/SoulX-FlashHead/daily.env
# edit it, then lock it down:
chmod 600 ~/SoulX-FlashHead/daily.env
```

The key is **server-side only**. The browser never sees it — it receives a short-lived
meeting token minted by this service.

### 2.5 Avatar identity + idle loop

```bash
cp your_face.jpg ~/SoulX-FlashHead/avatar_current.jpg   # the face to animate
cp idle_loop.mp4 ~/SoulX-FlashHead/idle_loop.mp4        # optional, see §5
```

Source photo quality dominates output quality: **sharp, front-facing, evenly lit, clean
background**. A soft still grabbed from a video produces soft, mediocre output.

### 2.6 Run

```bash
cp deploy/avatar/avatar_daily.py deploy/avatar/idle_gen.py ~/SoulX-FlashHead/
docker exec -d soulx bash -c 'cd /workspace/SoulX-FlashHead && CC=cc python avatar_daily.py > avatar_daily.log 2>&1'

# ~60s: model load + avatar prep + torch.compile warmup, then:
grep -E "idle video loaded|bot joined|serving on" ~/SoulX-FlashHead/avatar_daily.log
```

Expected:

```
[daily] idle video loaded: idle_loop.mp4 (250 frames)
[daily] bot joined room https://<you>.daily.co/xxxxxxxx
[daily] serving on :8899
```

---

## 3. Wiring the frontend

The UI talks to the avatar service at `ws://<box>:8899`, set in **Advanced → Avatar service**.

**Over HTTPS you must use `wss://`** — browsers block plain `ws://` from a secure page.
Expose it with a tunnel and point the UI at that:

```bash
./cloudflared tunnel --url http://127.0.0.1:8899 --no-autoupdate > cloudflared-avatar.log 2>&1 &
grep -hoE "https://[a-z0-9-]+\.trycloudflare\.com" cloudflared-avatar.log | tail -1
# use the wss:// form of that host as the Avatar service URL
```

Bake it into the embedded UI before building the Go server:

```bash
sed -i "s#ws://38.65.239.47:8899#wss://<your-tunnel-host>#g" examples/websocket/ui/index.html
CGO_ENABLED=1 go build -o server-bin ./examples/websocket
```

### Protocol

| Direction | Message | Meaning |
|---|---|---|
| server → client | `{"type":"room","url":…,"token":…}` | Sent on connect; client joins this Daily room |
| client → server | binary | PCM16 mono @16 kHz of the bot's speech |
| client → server | `{"type":"eot"}` | End of turn — render the buffered tail |
| client → server | `{"type":"flush"}` | Barge-in — drop everything pending |

**`eot` is required, not optional.** The model generates in fixed 0.96s chunks; without an
explicit end-of-turn the remainder of each reply stays buffered and is emitted at the start
of the *next* turn (audible as "the last few words show up late"). The browser sends `eot`
450ms after it forwards the **last** audio chunk.

> Timing subtlety: the Go server pushes TTS **faster than realtime**, but the browser forwards
> it to the avatar at **playback pace**. The `eot` timer must therefore be driven by the
> *forward*, not the *arrival* — otherwise it fires mid-utterance.

---

## 4. Room lifecycle: one call per process

Daily bills **participant-minutes**, so a bot that sits in a room between calls bills all
day for nothing. But `daily-python` only publishes reliably from the client that owned the
virtual camera at process start — three attempts at reusing one process across rooms all
failed:

| Attempt | What broke |
|---|---|
| `leave()` then `join()` a new room | SFU keeps a stale track; viewers see `v:loading a:playable` |
| `client.release()` and rebuild | orphans the virtual camera — `frames_written` drops to 0 |
| unique camera/mic device per session | `write_frame` blocks; 0 frames published |

So the lifecycle is **one call per process**:

1. Idle — process is warm, listening on `:8899`, **in no room**. Zero Daily billing.
2. Viewer connects on `/ws` → create a room (`exp` = 1 h), join, publish.
3. Viewer disconnects → `leave()`, `DELETE /v1/rooms/<name>`, `os._exit(0)`.
4. `run_avatar.sh` restarts a fresh process.

**Tradeoff: ~45–60 s re-warm between calls** (7 s model load + 3 s avatar prep + ~35 s
`torch.compile`). A call started inside that window gets connection-refused on `:8899`. One
process serves one concurrent viewer; run more processes on more GPUs for more.

> ⚠️ The watchdog **must** skip its health check while `ROOM["url"]` is unset. It probes
> `participant_counts()`, which fails when the bot is deliberately in no room — without the
> guard it hits 3 strikes and kills the process every ~90 s, forever.

Rooms are cheap but not free of clutter: every process start under the old always-join
design created one. Ours accumulated ~20 k. They do not bill, but most were created without
`exp` and stay joinable, so prune them periodically via `GET/DELETE /v1/rooms`.

## 5. Operating

```bash
# logs
tail -f ~/SoulX-FlashHead/avatar_daily.log

# per-turn evidence
grep -E "\[t\] eot|\[t\] flush" ~/SoulX-FlashHead/avatar_daily.log
#   [t] eot: tail 8400 samples (525ms)   <- tail flushed at end of a turn (want 1 per turn)
#   [t] flush: dropping N buffered ...   <- a barge-in fired

# restart (must kill inside the container: the process runs as root)
docker exec soulx bash -c 'pkill -9 python; sleep 3'
docker exec -d soulx bash -c 'cd /workspace/SoulX-FlashHead && CC=cc python avatar_daily.py > avatar_daily.log 2>&1'

# benchmark throughput / concurrency
docker exec soulx bash -c 'cd /workspace/SoulX-FlashHead && CC=cc python stream_latency_test.py lite avatar_current.jpg some_16k.wav'

# render an offline clip (pro = best quality, not realtime)
docker exec soulx bash -c 'cd /workspace/SoulX-FlashHead && mkdir -p sample_results && CC=cc python generate_video.py \
  --ckpt_dir models/SoulX-FlashHead-1_3B --wav2vec_dir models/wav2vec2-base-960h \
  --model_type pro --cond_image avatar_current.jpg --audio_path clip_16k.wav \
  --audio_encode_mode stream --save_file sample_results/out.mp4'
```

Audio fed to the model must be **16 kHz mono**:

```bash
ffmpeg -y -f s16le -ar 24000 -ac 1 -i kokoro.pcm -ar 16000 -ac 1 out_16k.wav
```

---

## 6. The idle loop (what the avatar does while listening)

Without this the avatar freezes on a still frame between replies, which reads as broken.

**SoulX cannot generate idle motion.** Measured frame-to-frame delta driving it with quiet
audio: **1.2–2.0 out of 255** (<1%) — effectively a still image. Driving it harder just moves
the mouth (measured eye/mouth motion ratio 0.75 — it looks like silent mumbling). The model is
built to hold identity still and animate only the mouth.

So the idle loop is **pre-rendered once** and looped during silence. Two sources:

### Option A — supply a video (recommended)

Generate a short idle clip with any image-to-video model, from the **exact frame SoulX
renders** (not the original photo — otherwise the framing jumps when it cuts to speaking).

Extract that frame:

```bash
# the service writes sample_results/idle_preview.mp4 on boot; frame 0 is the neutral face
ffmpeg -y -i sample_results/idle_preview.mp4 -vf "select=eq(n\,0)" -vframes 1 idle_base.png
```

Feed `idle_base.png` to the video model with:

> A person sitting still, listening quietly to someone off-camera. Subtle idle motion only:
> natural relaxed eye blinks every few seconds, slow gentle breathing, tiny head movements and
> micro-shifts in posture. Mouth stays closed and relaxed the entire time — she is not speaking,
> not smiling, not talking. Eyes stay open and looking toward the camera between blinks.
> Locked-off static camera, no zoom, no pan, no parallax. Background completely unchanged and
> static. Photorealistic, consistent lighting, same identity throughout, no morphing.

Negative prompt: `talking, speaking, mouth opening, lip movement, smiling, camera movement, zoom, pan, cuts, hands, text, watermark, face distortion, identity change`

Target **512×512, 25 fps, 6–10s, MP4**. The two constraints that matter most: **mouth closed**
(any lip movement reads as mumbling to itself) and **locked camera**.

Then normalize and install it:

```bash
ffmpeg -y -i raw_idle.mp4 -vf "scale=512:512:flags=lanczos,fps=25" -an \
  -c:v libx264 -pix_fmt yuv420p idle_fwd.mp4

# ping-pong for a seamless loop, unless it already loops cleanly
docker exec soulx python - <<'PY'
import imageio.v2 as iio, numpy as np
p="/workspace/SoulX-FlashHead/"
fr=[np.asarray(f)[:,:,:3] for f in iio.get_reader(p+"idle_fwd.mp4")]
d=[float(np.abs(fr[i].astype(np.int16)-fr[i-1].astype(np.int16)).mean()) for i in range(1,len(fr))]
m=sum(d)/len(d); seam=float(np.abs(fr[-1].astype(np.int16)-fr[0].astype(np.int16)).mean())
print("motion %.2f seam %.2f"%(m,seam))
seq = fr + fr[-2:0:-1] if seam > 2.0*m else fr      # ping-pong only if it would jump
w=iio.get_writer(p+"idle_loop.mp4",format="mp4",mode="I",fps=25,codec="libx264",pixelformat="yuv420p")
for f in seq: w.append_data(f)
w.close(); print("installed", len(seq), "frames")
PY
```

Restart the service to pick it up. Rule of thumb: **seam > 2× mean motion → ping-pong it**
(one real clip measured seam 17.3 vs motion 1.87 — a visible jump every 3s; ping-pong took it
to 3.7, and a better clip went 3.04 → **0.82**).

### What does NOT work: idle motion from the model

SoulX only animates the **mouth from audio** — it has no notion of blinking, breathing or
head movement. Measured frame-to-frame motion (of 255) when driving it to "idle":

| Driving input | Total motion | eye/head | mouth | verdict |
|---|---|---|---|---|
| silence / low noise | 1.16–1.48 | 1.3 | 1.0 | effectively a still image |
| quiet speech ×0.03 | ~2.0 | 2.96 | 3.59 | mouth moves **more** than eyes → looks like mumbling |

So "just stream the model with nothing said" produces a frozen face. Supply a clip (Option A).

A good trick: export a **model-generated** clip (`sample_results/idle_source_for_edit.mp4`)
and edit *that* with an image-to-video tool to close the mouth. Because the source came from
the model, identity, framing and lighting already match the speaking frames exactly — which
external clips never do.

> ⚠️ imageio writes H.264 **High 4:4:4 Predictive**, which many players and AI video tools
> refuse to decode. Re-encode before handing the clip to anything else:
> `ffmpeg -i in.mp4 -c:v libx264 -profile:v high -pix_fmt yuv420p -crf 16 -movflags +faststart out.mp4`
> Some editors also require ≥720px input — upscale with `scale=1024:1024:flags=lanczos`.

### Option B — synthesized fallback

If `idle_loop.mp4` is absent, `idle_gen.py` synthesizes a loop from the neutral frame:
breathing scale, slow sway, and real blinks (eyelid warped down using MediaPipe eye landmarks).
Motion ≈0.32 mean delta — alive, but clearly weaker than a real video clip.

---

## 7. Errors hit & fixes

| Symptom | Cause | Fix |
|---|---|---|
| `ResolutionImpossible: nvidia-nccl-cu12` | SoulX pins 2.27.3, torch 2.7.1 needs 2.26.2 | Drop the pin (multi-GPU only) |
| `ImportError: libxcb.so.1` | Runtime image lacks GUI libs opencv needs | `apt-get install libgl1 libxcb1 libxrender1 libsm6 libxext6 libglib2.0-0` |
| `InductorError: Failed to find C compiler` | Triton JIT-compiles kernels; no gcc in image | `apt-get install build-essential`, run with `CC=cc` |
| `FileNotFoundError: sample_results` | Script only auto-creates the dir when `--save_file` is omitted | `mkdir -p sample_results` |
| `FileNotFoundError: 'ffmpeg'` | ffmpeg missing **inside** the container (audio mux step) | `apt-get install ffmpeg` |
| `address already in use :8899` | Old instance still holding the port | `docker exec soulx bash -c 'pkill -9 python'` before restart |
| `kill: Operation not permitted` | Service runs as root in container; host user is `ubuntu` | Kill **inside** the container via `docker exec` |
| ICE stuck at `checking`, no media | Box is behind **symmetric NAT** (different external port per candidate) | Use Daily (SFU+TURN); direct aiortc needs a TURN relay |
| Last words of a reply appear in the next turn | Partial <0.96s chunk left buffered | Client sends `{"type":"eot"}`; server pads and renders the tail |
| `eot` fires mid-utterance | Timer keyed to audio **arrival** (faster than realtime) | Key it to the **forward** to the avatar (playback pace) |
| Video stutters over a tunnel | ~15 Mbps of JPEG through cloudflared | Send video via Daily; tunnel carries only audio + signaling |
| Avatar frozen between replies | SoulX generates no idle motion (see above) |
| Viewer sees `tracks v:loading a:playable`, blank avatar | The bot left and rejoined a room. `client.publishing()` reports local intent only — the SFU keeps a stale video track. **Never leave()/join() to hand out fresh rooms**; join once per process. |
| Frames stop entirely after a rejoin | `client.release()` orphans the virtual camera device, so `write_frame` silently stops. |
| Viewer prompted for camera access | `daily-js` grabs local mic+camera by default. Join receive-only: `createCallObject({videoSource:false, audioSource:false})` — it also stops fighting the voice pipeline for the mic. |
| Hard cut between idle and speech | Different sources. The writer crossfades over `XFADE` frames (~400ms) in both directions. | Pre-rendered idle loop (§5) |
| Idle loop jumps every N seconds | Last frame far from first | Ping-pong the clip |
| Browser plays unsynced duplicate audio | Raw Go audio playing alongside the Daily stream | Mute `botGain` in video mode; Daily carries the synced audio |

---

## 8. Files

| File | Purpose |
|---|---|
| `avatar_daily.py` | The service: SoulX generation → Daily publish, WS control, idle loop |
| `idle_gen.py` | Fallback idle animator (sway, breathing, MediaPipe blinks) |
| `stream_latency_test.py` | Throughput/latency benchmark (`lite` vs `pro`) |
| `daily.env.example` | Template for the Daily API key (never commit the real one) |
