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

    mode: shared | distinct | contact | campaignsN   (see build_prompt)
"""
import hashlib
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
# Shifts which campaigns this process uses, so two processes pointed at two
# replicas can be given DISJOINT campaign sets. That is how perfect prefix
# affinity is emulated without touching the load balancer: it is the routing
# nginx would produce if it hashed on campaign instead of counting connections.
CAMPAIGN_OFFSET = int(sys.argv[7]) if len(sys.argv) > 7 else 0
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

# The per-contact block: the only part that genuinely differs call to call.
# Placed at the END in the realistic modes, which is the whole point -- SGLang
# caches by PREFIX, so two prompts share work only up to their first differing
# byte. The same facts moved to the front share nothing.
CONTACT = ("Candidate on this call: {name}, id {cid}. Phone {phone}. "
           "Applied {days} days ago for req {req}. Resume summary: {years} years "
           "in digital marketing analytics, last role at {employer}, based in "
           "{city}. Address them by first name and do not read the id aloud.")

# A campaign block: shared by every call in one campaign, different between
# campaigns. Sits after the invariant preamble and before the contact block.
CAMPAIGN = ("Campaign {c}: hiring a Digital Marketing Analyst for client {c}. "
            "Budget band {band}k. Onsite in city {c}. Recruiter extension {ext}. "
            "Screen for Sitecore, Figma and analytics depth, in that order. ")


def build_prompt(call_id):
    """The system prompt for one call, laid out the way each mode describes.

    Modes exist to answer one question: how much does prompt LAYOUT cost?
    Every mode carries roughly the same information, so any difference between
    them is caused by where the varying bytes sit, not by prompt size.

      shared      every call byte-identical. The unreachable best case; it
                  exists as the ceiling to measure the others against.
      distinct    a unique string at the FRONT of every prompt. The worst case,
                  and what happens when an API user templates the contact block
                  into the top of the prompt.
      contact     one campaign, per-contact block LAST. What a single campaign
                  should look like in production.
      campaignsN  N campaigns interleaved, each with its own block, contact
                  block last -- but all N still share the same invariant
                  preamble. True when every campaign is built from one
                  template; generous otherwise.
      uniqcampN   N campaigns whose prompts differ from the FIRST BYTE, five
                  calls sharing each. No common preamble at all. This is what
                  independent API users writing their own prompts actually
                  produces, and it is the number that decides whether the gate
                  fires in production.
    """
    if MODE == "shared":
        return BASE
    if MODE == "distinct":
        return f"Campaign {call_id} ref {call_id * 7919}. " + BASE
    contact = CONTACT.format(
        name=f"Candidate {call_id}", cid=100000 + call_id,
        phone=f"+1555{2000000 + call_id}", days=call_id % 30 + 1,
        req=9000 + call_id, years=call_id % 9 + 2,
        employer=f"Employer {call_id}", city=f"City {call_id % 40}")
    if MODE == "contact":
        return BASE + " " + contact
    if MODE.startswith("uniqcamp"):
        n = int(MODE[len("uniqcamp"):])
        c = call_id % n + CAMPAIGN_OFFSET
        # The campaign's own words come FIRST and nothing precedes them, so two
        # campaigns share zero cached prefix. Within a campaign the five calls
        # still share everything up to their contact block, which is the whole
        # question: is per-campaign sharing enough on its own?
        head = (f"You are recruiter number {c} for client {c}, calling about "
                f"requisition {7000 + c * 13} in market {c}. ") * 3
        return head + BASE + " " + contact
    if MODE.startswith("campaigns"):
        n = int(MODE[len("campaigns"):])
        # Round-robin, which is the pessimistic interleaving: consecutive
        # dispatches land on different campaigns, so nothing arrives back to
        # back with a warm prefix.
        c = call_id % n
        return BASE + " " + CAMPAIGN.format(c=c, band=90 + c, ext=4000 + c) + contact
    raise SystemExit(f"unknown mode {MODE!r}")


ttft_all, ttft_by_turn, errs = [], {}, [0]
lock = threading.Lock()


def one_call(call_id):
    system = build_prompt(call_id)
    # The same routing tag the agent sets (pkg/rexa/route.go): a hash of the
    # system prompt's leading 4KB, so calls sharing a campaign prefix pin to one
    # replica. Only equality matters -- nginx compares keys, it does not care
    # which hash produced them -- so this need not match Go's FNV.
    prefix_key = hashlib.sha1(system[:4096].encode()).hexdigest()[:16]
    history = [{"role": "assistant", "content": "Hi, thanks for taking my call. How are you today?"}]
    for turn in range(TURNS):
        history.append({"role": "user", "content": USER_TURNS[turn % len(USER_TURNS)]})
        msgs = [{"role": "system", "content": system}] + history[-HISTORY_CAP:]
        body = json.dumps({"model": MODEL, "messages": msgs, "max_tokens": 70,
                           "temperature": 0.7, "stream": True}).encode()
        req = urllib.request.Request(f"http://127.0.0.1:{PORT}/v1/chat/completions",
                                     data=body, headers={"content-type": "application/json",
                                                         "x-prefix-key": prefix_key})
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
