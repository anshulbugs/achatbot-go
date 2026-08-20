# Launching the GH200 on Lambda — the exact clicks

Written while doing it on 2026-08-21, so the next launch is mechanical. Every
choice below has the reason next to it; where a step could reasonably go the
other way, that is said too.

Console: <https://cloud.lambda.ai/workspace/e17fd6d21b0740fe94859643d446ba73/instances>
Account: Talk Account / JobTalk Admin (dev@jobtalk.ai)

## The dialog, step by step

**Instances → Launch instance.**

| Step | Choose | Why |
|---|---|---|
| Instance type | **1x GH200 (96 GB)** — $2.29/hr, 64 vCPU, 432 GiB RAM, 4 TiB SSD, tagged ARM64 + H100 | the target. Note ARM64: see `docs/GH200-BRANCH.md` |
| Region | **Washington DC, us-east-3** | the ONLY region offering GH200 — every other region is greyed out with "Not available for this instance type". Also the right side of the country for Telnyx and Daily |
| Base image | **Lambda Stack 24.04** (default is 22.04 — change it) | see below |
| Filesystem | **Don't attach a filesystem** | see below |
| Security | leave as **Global firewall rules**, click Confirm | we expose nothing: services bind to 127.0.0.1 and every public endpoint is a cloudflared tunnel. Adding a ruleset that opens ports would work against that |
| SSH key | **anshul-rexa-ed25519** (add it once, then pick it) | see below |

Then **Launch instance**. "Instances may take up to 20 minutes to boot, you will
not be charged during this time."

### Why Lambda Stack 24.04, not GPU Base

Four options are offered: Lambda Stack 22.04 / 24.04, GPU Base 22.04 / 24.04.

Lambda Stack ships Docker and the NVIDIA container runtime already wired up.
That is the deciding factor: everything we run is a container, and
`deps-install.sh` deliberately refuses to install root-level things silently —
it checks and prints the fix. GPU Base is driver-only, so it would mean
`get.docker.com` and `nvidia-ctk runtime configure` by hand first.

Lambda Stack's host PyTorch and TensorFlow are dead weight for us — we never
touch host Python — but the instance has 4 TiB of SSD, so it costs nothing that
matters.

24.04 over 22.04 for newer kernel and userland on aarch64. Nothing we use pulls
the other way.

### Why no filesystem

A Lambda persistent filesystem survives instance termination, which would save
re-downloading the ~83 GB of weights (gemma-4, parakeet, kokoro) on every
relaunch. It also bills monthly whether or not an instance is running.

Skipped, because the models re-download on their own in about ten minutes, and
the things that actually hurt to lose — secrets, config, code — are already in
git and in `docs/REBUILD-FROM-SCRATCH.md`. Ten minutes per relaunch is not worth
a standing charge.

**This is the one step you cannot change later.** A filesystem attaches at
launch only. If the relaunch cycle turns out to be frequent, that is the reason
to revisit it — and it means terminating and relaunching the instance.

### Why an existing SSH key, not a generated one

The account had no key, so the dialog jumps to "Add your SSH key". It offers
**Generate a new SSH key**, which makes Lambda produce a private key and
download it through the browser. Don't. Paste the public half of the key that
already exists instead:

```bash
cat ~/.ssh/id_ed25519.pub
# ssh-ed25519 AAAA... anshul@rexa.ai
# fingerprint SHA256:X4MawzjkHRGICcfGwX/SyH/NcHGeQZgdr3GZF2Kqbcg
```

Name it `anshul-rexa-ed25519`. The private key never moves, and it is the same
key the old box used, so existing `~/.ssh/config` habits carry over. Once added
it stays on the account under **SSH keys** — later launches just select it.

## After it boots

```bash
ssh ubuntu@<instance-ip>
git clone https://github.com/anshulbugs/achatbot-go.git && cd achatbot-go
git checkout gh200            # ARM64 image stack lives here, not on main
```

Copy the two secret files across by hand — they are gitignored and must never
be committed:

```bash
scp deploy/rexa-secrets.env deploy/telnyx.env ubuntu@<instance-ip>:achatbot-go/
```

`REXA_REDIS_PASSWORD` is still missing and `DAILY_API_KEY` should be checked
against the current one — see the secrets table in
`docs/REBUILD-FROM-SCRATCH.md`.

Then:

```bash
bash deploy/scripts/up-voice-gh200.sh
python3 deploy/scripts/verify-speech-markup.py   # markup, pauses, reaction sounds
python3 deploy/loadtest/ttsbench.py 30 90        # then asrbench.py
# set server.max_gpu_calls from that result, restart. 61 was the 5090 box.
```

## Things worth knowing before you start

- **Billing starts at the final click**, not before. Boot time is free. An
  instance left running is about $55/day, so terminate it when idle — there is
  no stop-and-keep-disk state, terminating destroys the local SSD.
- **The console showed "Degraded performance"** (amber dot, top right) during
  this launch. Worth a glance before blaming our own stack for a slow boot.
- **Every region except us-east-3 is greyed out for GH200.** If us-east-3 has no
  capacity, there is no second choice — the instance type simply cannot launch.
- **The dialog reflows as the window resizes.** If you script this, screenshot
  immediately before each click rather than reusing coordinates from an earlier
  screenshot; several clicks missed for exactly that reason.
