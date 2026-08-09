"""Measure what prefix sharing is actually worth on this stack.

The question this answers: when a campaign's calls share most of their system
prompt, SGLang's RadixAttention stores that prefix's KV cache once for the
whole fleet. When every concurrent call carries a DIFFERENT prompt — which is
what "one call per campaign, round-robin for fair allocation" produces — there
is nothing to share, and `--schedule-policy lpm` has nothing to match on.

Three modes, same prompt size and same concurrency, so the only variable is
where the prompts differ:

  shared    every request sends an identical system prompt.
            Best case: one prefix for the fleet.
  suffix    identical prompt, unique text APPENDED.
            What resolvePrompt already produces, and what a campaign with
            per-contact details looks like.
  distinct  unique text PREPENDED, so prompts diverge at the first token.
            Worst case: one call per campaign, no sharing possible.

`shared` and `suffix` should land close together. If `distinct` is far worse,
the round-robin-across-campaigns scheduling is the thing to change — the
prompts themselves are fine.

Usage:
    python3 prefixbench.py <port> <concurrency> <requests> <prompt_tokens> <mode>
    python3 prefixbench.py 8002 30 60 6000 shared

Note the prompt must fit the server's --context-length along with max_tokens,
or every request returns 400 and the numbers are meaningless. The script says
so explicitly rather than reporting a suspiciously fast failure.
"""

import json
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request

PORT = sys.argv[1]
CONC = int(sys.argv[2])
N = int(sys.argv[3])
PROMPT_TOKENS = int(sys.argv[4])
MODE = sys.argv[5] if len(sys.argv) > 5 else "shared"
MODEL = sys.argv[6] if len(sys.argv) > 6 else "google/gemma-4-E4B-it"

if MODE not in ("shared", "suffix", "distinct"):
    sys.exit("mode must be one of: shared, suffix, distinct")

# ~1.4 tokens per word for this kind of prose, so aim slightly high and let the
# tokenizer land where it lands — the exact count matters less than all three
# modes using the SAME count.
WORDS = int(PROMPT_TOKENS / 1.4)
BASE = ("You are a warm, professional voice assistant making an outbound call. "
        "Speak naturally and never mention being an AI. ") * (WORDS // 20 + 1)
BASE = " ".join(BASE.split()[:WORDS])

lat, ttft, errs, codes = [], [], [0], {}
lock = threading.Lock()


def prompt_for(i):
    """Build request i's system prompt for the configured mode."""
    if MODE == "shared":
        return BASE
    # A per-request marker long enough that it cannot be confused with noise.
    uniq = f"Campaign {i} reference {i * 7919:d}. " * 8
    if MODE == "suffix":
        return BASE + " " + uniq
    return uniq + " " + BASE  # distinct: diverges at the first token


def one(i):
    body = json.dumps({
        "model": MODEL,
        "messages": [
            {"role": "system", "content": prompt_for(i)},
            {"role": "assistant", "content": "Hi, thanks for taking my call. How are you today?"},
            {"role": "user", "content": "Good thanks. Can you tell me more about the role?"},
        ],
        "max_tokens": 80,
        "temperature": 0.7,
        "stream": True,
    }).encode()
    req = urllib.request.Request(
        f"http://127.0.0.1:{PORT}/v1/chat/completions",
        data=body, headers={"Content-Type": "application/json"})
    t0 = time.time()
    first = None
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            for line in r:
                if line.startswith(b"data:") and b"content" in line and first is None:
                    first = time.time() - t0
        total = time.time() - t0
        with lock:
            if first is not None:
                ttft.append(first * 1000)
            lat.append(total * 1000)
    except urllib.error.HTTPError as e:
        detail = e.read().decode()[:120]
        with lock:
            errs[0] += 1
            codes[e.code] = codes.get(e.code, 0) + 1
            codes["detail"] = detail
    except Exception as e:  # noqa: BLE001 - any failure is a failed request
        with lock:
            errs[0] += 1
            codes[type(e).__name__] = codes.get(type(e).__name__, 0) + 1


sem = threading.Semaphore(CONC)


def run(i):
    with sem:
        one(i)


start = time.time()
threads = [threading.Thread(target=run, args=(i,)) for i in range(N)]
for t in threads:
    t.start()
for t in threads:
    t.join()
wall = time.time() - start


def pct(values, q):
    if not values:
        return 0
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * q))]


rps = len(lat) / wall if wall > 0 else 0
print(f"mode={MODE:8s} tokens~{PROMPT_TOKENS:6d} conc={CONC:3d} n={N:4d} | "
      f"TTFT p50={pct(ttft, .5):6.0f}ms p95={pct(ttft, .95):7.0f}ms | "
      f"total p50={pct(lat, .5):6.0f}ms p95={pct(lat, .95):7.0f}ms | "
      f"{rps:5.1f} req/s | ok={len(lat)} errs={errs[0]}")
if errs[0]:
    print(f"    errors: {codes}")
    if 400 in codes:
        print("    NOTE: 400 usually means the prompt exceeds --context-length. "
              "These numbers are meaningless until that is fixed.")
