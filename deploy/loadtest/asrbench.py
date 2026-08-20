"""ASR concurrency benchmark.

    python3 asrbench.py <concurrency> <requests> [pcm-file]

The fixture is 16 kHz mono PCM16 of real speech. It used to be read from
/tmp/spk16.pcm unconditionally, which was a hand-made file on the old box
documented nowhere — so this benchmark died with FileNotFoundError on a rebuilt
one, exactly when capacity most needed re-measuring.

With no file given it now SYNTHESISES the fixture from our own kokoro service
and caches it. That is better than a checked-in wav as well as more portable:
parakeet then transcribes the same voice it hears on a real call, so the
measurement reflects the deployment rather than a stock sample.
"""
import os
import json
import subprocess
import time, threading, urllib.request, sys

CONC = int(sys.argv[1]); N = int(sys.argv[2])
PCM = sys.argv[3] if len(sys.argv) > 3 else os.path.expanduser("~/.cache/asrbench-16k.pcm")


def make_fixture(path):
    """Render a representative call utterance at 16 kHz via kokoro."""
    text = ("Thanks for taking my call. Could I ask roughly how many recruiters "
            "you have on the team at the moment?")
    body = json.dumps({"input": text, "voice": "af_heart", "speed": 1.1})
    r = subprocess.run(["curl", "-s", "-m", "60", "-X", "POST",
                        "-H", "Content-Type: application/json", "-d", body,
                        "http://127.0.0.1:8880/tts"], capture_output=True)
    if not r.stdout:
        sys.exit("no fixture at %s and kokoro on :8880 did not answer — start TTS, "
                 "or pass a 16kHz PCM16 file as the third argument" % path)
    import array
    a = array.array("h")
    a.frombytes(r.stdout)
    # kokoro emits 24 kHz; parakeet wants 16 kHz. 3:2 decimation, taking 2 of
    # every 3 samples, is crude but this is a load fixture, not a quality test.
    out = array.array("h", (a[i] for i in range(len(a)) if i % 3 != 2))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(out.tobytes())
    print("wrote fixture %s (%.1fs of audio)" % (path, len(out) / 16000.0))


if not os.path.exists(PCM):
    make_fixture(PCM)
DATA = open(PCM, "rb").read()
lat=[]; lock=threading.Lock(); sem=threading.Semaphore(CONC)
def one():
    with sem:
        r=urllib.request.Request("http://127.0.0.1:8890/asr",data=DATA,headers={"Content-Type":"application/octet-stream"})
        t0=time.time()
        try:
            with urllib.request.urlopen(r,timeout=120) as resp: resp.read()
            with lock: lat.append((time.time()-t0)*1000)
        except Exception as e:
            with lock: lat.append(-1)
ths=[threading.Thread(target=one) for _ in range(N)]
t0=time.time()
for t in ths: t.start()
for t in ths: t.join()
ok=sorted(v for v in lat if v>0); el=time.time()-t0
p=lambda q: ok[min(len(ok)-1,int(len(ok)*q))] if ok else 0
print("ASR conc=%3d n=%3d | p50=%5.0fms p95=%5.0fms | %.1f req/s | fails=%d"%(CONC,N,p(.5),p(.95),len(ok)/el,sum(1 for v in lat if v<0)))
