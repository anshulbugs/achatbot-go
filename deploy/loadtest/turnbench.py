"""How many concurrent CALLS can the LLM tier carry, multi-turn and paced?

Every earlier benchmark fired single-turn requests back to back, which is not
what a call does. A real call:

  - reuses the same system prompt every turn, so RadixAttention keeps that
    prefix cached across the whole call
  - accumulates history, so each turn's prompt is longer than the last
  - thinks between turns -- the caller talks, VAD waits out the pause, ASR
    transcribes, TTS speaks. Measured on this stack: about one turn per 12.4 s
    per call. Firing turns without that gap overstates load by roughly 10x.

So this models N concurrent calls, each running T paced turns, and reports how
first-token latency behaves as history grows and as N rises.

Measures the LLM tier ONLY. The 61-call figure is end-to-end with ASR and TTS
also at 77-96% utilisation, so this says where the LLM stops being comfortable,
not where the whole stack does.

Usage:
    python3 turnbench.py <port> <calls> <turns> <think_secs> <prompt_tokens> <mode>
"""
import json
import statistics
import sys
import threading
import time
import urllib.request

PORT = sys.argv[1]
CALLS = int(sys.argv[2])
TURNS = int(sys.argv[3])
THINK = float(sys.argv[4])
PROMPT_TOKENS = int(sys.argv[5])
MODE = sys.argv[6] if len(sys.argv) > 6 else "shared"
MODEL = "google/gemma-4-E4B-it"

# Matches server.chat_history_size 12 -> 2*(12+1) = 26 retained messages.
HISTORY_CAP = 26

WORDS = int(PROMPT_TOKENS / 1.4)
BASE = ("You are a warm, professional voice recruiter on a live phone call. "
        "Follow the steps and never invent details. ") * (WORDS // 18 + 1)
BASE = " ".join(BASE.split()[:WORDS])

USER_TURNS = [
    "Yes, go ahead, I have a few minutes.",
    "I live in Brooklyn so the location works for me.",
    "I'm looking for around ninety thousand a year.",
    "Yes, I'm authorised to work in the US.",
    "No, I won't need sponsorship.",
    "I've spent about four years doing digital marketing analytics.",
    "Mostly Sitecore and Figma, some JavaScript.",
    "Yes, I'd be happy to speak to the recruiter.",
    "Could you tell me a bit more about the team first?",
    "That sounds good, thanks.",
]

ttft_all, ttft_by_turn, errs = [], {}, [0]
lock = threading.Lock()


def one_call(call_id):
    system = BASE if MODE == "shared" else f"Campaign {call_id} ref {call_id*7919}. " + BASE
    history = [{"role": "assistant", "content": "Hi, thanks for taking my call. How are you today?"}]
    for turn in range(TURNS):
        history.append({"role": "user", "content": USER_TURNS[turn % len(USER_TURNS)]})
        msgs = [{"role": "system", "content": system}] + history[-HISTORY_CAP:]
        body = json.dumps({"model": MODEL, "messages": msgs, "max_tokens": 70,
                           "temperature": 0.7, "stream": True}).encode()
        req = urllib.request.Request(f"http://127.0.0.1:{PORT}/v1/chat/completions",
                                     data=body, headers={"content-type": "application/json"})
        t0 = time.time()
        first = None
        reply = []
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                for line in r:
                    if line.startswith(b"data:") and b"content" in line:
                        if first is None:
                            first = time.time() - t0
                        try:
                            chunk = json.loads(line[5:].strip())
                            c = chunk["choices"][0]["delta"].get("content")
                            if c:
                                reply.append(c)
                        except Exception:
                            pass
            if first is not None:
                with lock:
                    ttft_all.append(first * 1000)
                    ttft_by_turn.setdefault(turn, []).append(first * 1000)
        except Exception:
            with lock:
                errs[0] += 1
        history.append({"role": "assistant", "content": "".join(reply) or "Understood."})
        # Pace the next turn the way a real conversation does.
        if turn < TURNS - 1:
            time.sleep(THINK)


start = time.time()
threads = [threading.Thread(target=one_call, args=(i,)) for i in range(CALLS)]
for t in threads:
    t.start()
for t in threads:
    t.join()
wall = time.time() - start


def pct(v, q):
    if not v:
        return 0
    v = sorted(v)
    return v[min(len(v) - 1, int(len(v) * q))]


req = len(ttft_all)
print(f"mode={MODE:8s} calls={CALLS:3d} turns={TURNS} think={THINK}s tokens~{PROMPT_TOKENS} | "
      f"TTFT p50={pct(ttft_all, .5):5.0f}ms p95={pct(ttft_all, .95):6.0f}ms | "
      f"{req/wall:5.1f} req/s | errs={errs[0]}")
# Growth across turns is the thing single-turn benchmarks cannot show.
first_t = pct(ttft_by_turn.get(0, []), .95)
last_t = pct(ttft_by_turn.get(TURNS - 1, []), .95)
print(f"          TTFT p95 turn1={first_t:5.0f}ms -> turn{TURNS}={last_t:5.0f}ms "
      f"({'+' if last_t >= first_t else ''}{last_t - first_t:.0f}ms as history grows)")
