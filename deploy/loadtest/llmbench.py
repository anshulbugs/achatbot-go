"""LLM concurrency benchmark: TTFT and throughput at a given concurrency.

    python3 llmbench.py <port> <concurrency> <requests> [sysprompt-file]

TTFT is the number that matters. A caller hears the gap between finishing their
sentence and the agent starting its reply, and on this stack the LLM's first
token is the largest single contributor to it. Throughput only decides how many
of those can happen at once.

TWO THINGS THIS USED TO GET WRONG, both of which only showed on a fresh box:

  - It read /tmp/sysprompt.txt and died if absent. That file was created by hand
    on the old box and written down nowhere, so the benchmark was unrunnable
    after a rebuild. There is now a built-in prompt of a realistic size, and the
    path is an optional argument.

  - It hardcoded "Qwen/Qwen2.5-3B-Instruct" as the model while the box served
    something else entirely. The served name is now read from /v1/models, so the
    benchmark cannot silently measure a different deployment than the one
    running.

PROMPT SIZE IS NOT INCIDENTAL. Prefill is charged per token of the system
prompt, and a campaign prompt here runs about 3k tokens. Benchmarking with a
short prompt flatters the result badly, so the built-in one is padded to a
comparable length.
"""
import json
import sys
import threading
import time
import urllib.request

PORT = sys.argv[1]
CONC = int(sys.argv[2])
N = int(sys.argv[3])
SYS_FILE = sys.argv[4] if len(sys.argv) > 4 else None

BASE = (
    "You are Sarah, an outbound voice agent calling on behalf of JobTalk, a "
    "recruitment technology company. Your goal on this call is to find out how "
    "the person's team currently handles candidate screening, whether they feel "
    "that process is slow, and if so whether they would take a short demo. Be "
    "warm and brief. Never claim capabilities you were not told about. If asked "
    "about pricing, say a specialist will follow up with exact numbers. If the "
    "person is busy, offer to call back and end the call politely. "
)

if SYS_FILE:
    SYS = open(SYS_FILE).read()
else:
    # ~3k tokens, matching a real campaign prompt. Repetition is fine: the cost
    # under test is prefill length, not prose quality.
    SYS = BASE * 30

# Ask the server what it is serving, rather than asserting it.
try:
    with urllib.request.urlopen("http://127.0.0.1:%s/v1/models" % PORT, timeout=10) as r:
        MODEL = json.load(r)["data"][0]["id"]
except Exception as e:
    sys.exit("could not read the served model from /v1/models on port %s: %s" % (PORT, e))

MSGS = [
    {"role": "system", "content": SYS},
    {"role": "assistant", "content": "Hey there, thanks so much for taking my call! How is your day going?"},
    {"role": "user", "content": "It is going pretty well thanks. Can you tell me more about the role and what the team looks like?"},
]

lat = []
ttft = []
errs = [0]
lock = threading.Lock()


def one():
    body = json.dumps({"model": MODEL, "messages": MSGS, "max_tokens": 80,
                       "temperature": 0.7, "stream": True}).encode()
    req = urllib.request.Request("http://127.0.0.1:%s/v1/chat/completions" % PORT,
                                 data=body, headers={"Content-Type": "application/json"})
    t0 = time.time()
    first = None
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            for line in r:
                if line.startswith(b"data:") and b"content" in line and first is None:
                    first = time.time() - t0
        tot = time.time() - t0
        with lock:
            if first is not None:
                ttft.append(first * 1000)
            lat.append(tot * 1000)
    except Exception:
        with lock:
            errs[0] += 1


sem = threading.Semaphore(CONC)
ths = []


def run():
    with sem:
        one()


t0 = time.time()
for _ in range(N):
    t = threading.Thread(target=run)
    t.start()
    ths.append(t)
for t in ths:
    t.join()
wall = time.time() - t0


def p(a, q):
    a = sorted(a)
    return a[min(len(a) - 1, int(len(a) * q))] if a else 0


print("model=%s sys_tokens~%d" % (MODEL, len(SYS) // 4))
print("port=%s conc=%d n=%d | TTFT p50=%4.0fms p95=%4.0fms | total p50=%4.0fms p95=%5.0fms | %5.1f req/s | errs=%d" % (
    PORT, CONC, N, p(ttft, .5), p(ttft, .95), p(lat, .5), p(lat, .95), len(lat) / wall, errs[0]))
