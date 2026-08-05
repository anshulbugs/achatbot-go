# TTS load test.  usage: ttsbench.py CONC N [URL] [VOICE]
#
# Defaults target Kokoro on :8880. To benchmark Supertonic on :8881 the voice
# must change too -- the two engines share no voice names, and asking Supertonic
# for "af_heart" is a 400 that shows up here as a fail, not as a slow request:
#
#   python3 ttsbench.py 50 150 http://127.0.0.1:8881/tts F2
import json, sys, threading, time, urllib.request

CONC = int(sys.argv[1])
N = int(sys.argv[2])
URL = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:8880/tts"
VOICE = sys.argv[4] if len(sys.argv) > 4 else "af_heart"

TXT = "Hi there, thanks so much for taking my call today. How is your day going so far?"
lat = []
audio = [0]
lock = threading.Lock()
sem = threading.Semaphore(CONC)


def one():
    with sem:
        b = json.dumps({"input": TXT, "voice": VOICE, "speed": 1.1}).encode()
        r = urllib.request.Request(URL, data=b, headers={"Content-Type": "application/json"})
        t0 = time.time()
        try:
            with urllib.request.urlopen(r, timeout=90) as resp:
                body = resp.read()
            with lock:
                lat.append((time.time() - t0) * 1000)
                audio[0] += len(body)
        except Exception:
            with lock:
                lat.append(-1)


ths = [threading.Thread(target=one) for _ in range(N)]
t0 = time.time()
for t in ths:
    t.start()
for t in ths:
    t.join()
wall = time.time() - t0

ok = sorted(v for v in lat if v > 0)
p = lambda q: ok[min(len(ok) - 1, int(len(ok) * q))] if ok else 0
fails = sum(1 for v in lat if v < 0)
print(
    "TTS conc=%3d n=%3d | p50=%5.0fms p95=%5.0fms max=%5.0fms | %.1f req/s | fails=%d"
    % (CONC, N, p(0.5), p(0.95), max(ok) if ok else 0, len(ok) / wall, fails)
)
if fails:
    print("  %d failed -- check the voice name matches the engine at %s" % (fails, URL))
