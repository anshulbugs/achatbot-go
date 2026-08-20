"""Verify kokoro's markup, pause marks and reaction sounds on real hardware.

Run this ONCE on a new box before trusting tts.markup, and again after any
change to the kokoro image. It exists for two reasons. This pipeline has
shipped expressive markup the engine did not understand and read aloud to a
caller. And the previous round of punctuation measurement here tested the wrong
characters — "-", "--" and "..." rather than the em dash and the ellipsis the
engine actually knows — and drew the wrong conclusion from it.

    python3 deploy/scripts/verify-speech-markup.py

Needs the kokoro service on :8880 and parakeet on :8890. Writes wavs to
~/markupcheck, because half of these questions are settled by ear, not by a
meter.

What each section decides:

  1. SPOKEN?    Does any bracket reach the caller? A FAIL here means turn
                tts.markup off in config.yaml and stop.
  2. AUDIBLE?   Does [word](+1) actually change the audio? Identical duration
                AND identical energy means misaki parsed it and the model
                ignored it, which makes the prompt rule dead weight.
  3. LEXICON    Does the JobTalk entry fix the two-word reading?
  4. SOUNDS     The eight reaction sounds the prompt allows, spelled the way
                the prompt spells them. Decide by ear — whether one lands as a
                sound a person makes is not something ASR can tell you.
  5. PAUSES     What each mark buys INSIDE a sentence. Misaki's punctuation set
                is ';:,.!?' plus the em dash and the ellipsis character; the
                hyphen is junk to it and "..." ends the sentence. Both the real
                marks and their ASCII lookalikes are measured here, so the
                difference between them is visible rather than assumed.
"""
import json
import os
import subprocess
import wave

import numpy as np

RATE = 24000
VOICE = "af_heart"
SPEED = 1.10
GAIN = 1.4
OUT = os.path.expanduser("~/markupcheck")

EM_DASH = "—"
ELLIPSIS = "…"


def synth(text, speed=SPEED):
    body = json.dumps({"input": text, "voice": VOICE, "speed": speed})
    r = subprocess.run(["curl", "-s", "-m", "60", "-X", "POST",
                        "-H", "Content-Type: application/json",
                        "-d", body, "http://127.0.0.1:8880/tts"],
                       capture_output=True)
    return r.stdout


def asr(pcm):
    r = subprocess.run(["curl", "-s", "-m", "90", "--data-binary", "@-",
                        "-H", "Content-Type: application/octet-stream",
                        "http://127.0.0.1:8890/asr"],
                       input=pcm, capture_output=True)
    try:
        return json.loads(r.stdout.decode()).get("text", "").strip()
    except Exception:
        return r.stdout.decode()[:70]


def save(name, pcm):
    a = np.frombuffer(pcm, dtype="<i2").astype(np.float32)
    a = np.clip(a * GAIN, -32768, 32767).astype(np.int16)
    with wave.open(f"{OUT}/{name}.wav", "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(a.tobytes())
    return a


def rms(a):
    a = a.astype(np.float32) / 32768.0
    return float(np.sqrt(np.mean(a ** 2))) if len(a) else 0.0


def middle_gap(a, thresh=0.012):
    """Longest internal silence in ms, ignoring lead-in and tail."""
    win = int(0.01 * RATE)
    frames = [float(np.sqrt(np.mean(a[i:i + win] ** 2)))
              for i in range(0, len(a) - win, win)]
    voiced = [i for i, f in enumerate(frames) if f >= thresh]
    if not voiced:
        return 0
    frames = frames[voiced[0]:voiced[-1] + 1]
    best = run = 0
    for f in frames:
        run = run + 1 if f < thresh else 0
        best = max(best, run)
    return best * 10


os.makedirs(OUT, exist_ok=True)

print("=== 1. SPOKEN? nothing in brackets may reach the caller ===")
print(f"{'case':22} {'dur':>6}  transcript")
SPOKEN = [
    ("plain",           "That part is free for you."),
    ("stress up",       "That part is [free](+1) for you."),
    ("stress down",     "That part is [free](-1) for you."),
    ("phonemes",        "Welcome to [JobTalk](/ʤˈɑbtˌɔk/)."),
    # A target misaki does NOT recognise. The claim under test is that it drops
    # the whole link rather than speaking it.
    ("unknown target",  "That part is [free](wibble) for you."),
    ("two marks",       "[Your](+1) demo is [ready](+1) today."),
]
for name, text in SPOKEN:
    pcm = synth(text)
    if not pcm:
        print(f"{name:22}  NO AUDIO")
        continue
    a = save(name.replace(" ", "_"), pcm)
    heard = asr(pcm)
    bad = any(t in heard.lower() for t in ("plus", "minus", "bracket", "slash", "wibble"))
    print(f"{name:22} {len(a)/RATE:5.2f}s  {heard[:52]}{'   <-- FAIL' if bad else ''}")

print("\n=== 2. AUDIBLE? does the mark change the audio at all ===")
base = np.frombuffer(synth("That part is free for you."), dtype="<i2")
for level in ("+1", "+2", "-1"):
    a = np.frombuffer(synth(f"That part is [free]({level}) for you."), dtype="<i2")
    same_len = abs(len(a) - len(base)) < RATE * 0.01
    flat = same_len and abs(rms(a) - rms(base)) < 0.001
    print(f"  [free]({level:2}): {len(a)/RATE:5.2f}s vs {len(base)/RATE:5.2f}s  "
          f"rms {rms(a):.4f} vs {rms(base):.4f}"
          f"{'   <-- no measurable change' if flat else ''}")
print("  Listen to stress_up.wav against plain.wav before believing the meter.")

print("\n=== 3. LEXICON: is the brand name one word? ===")
BRAND = [
    ("brand plain", "Welcome to JobTalk, your hiring assistant."),
    ("brand forced",
     "Welcome to [JobTalk](/ʤˈɑbtˌɔk/), your hiring assistant."),
]
for name, text in BRAND:
    pcm = synth(text)
    save(name.replace(" ", "_"), pcm)
    print(f"  {name:14} {asr(pcm)[:56]}")

print("\n=== 4. The eight reaction sounds, spelled as the prompt spells them ===")
SOUNDS = [
    ("hmmm",   "Hmmm" + ELLIPSIS + " let me check that for you."),
    ("well",   "Well" + ELLIPSIS + " it depends on the size of your team."),
    ("ahh",    "Ahh, now I see what you mean."),
    ("aww",    "Aww, that is a shame."),
    ("ooh",    "Ooh " + EM_DASH + " that is interesting."),
    ("wow",    "Wow! That is a big team."),
    ("oh",     "Oh! I should mention one thing."),
    ("really", "Really? I had not heard that."),
]
for name, text in SOUNDS:
    pcm = synth(text)
    if not pcm:
        print(f"  {name:8} NO AUDIO")
        continue
    save("sound_" + name, pcm)
    print(f"  {name:8} {text[:34]:36} -> {asr(pcm)[:40]}")
print("  ASR is the wrong judge here: parakeet writing Hmmm as Hm proves nothing")
print("  either way. Play sound_*.wav and decide whether each one is a sound a")
print("  person makes or a word being spelled out.")

print("\n=== 5. Pause hierarchy, in the characters the engine actually knows ===")
A, B = "I understand", "let me check that for you"
MARKS = [
    ("none",            f"{A} {B}."),
    ("comma",           f"{A}, {B}."),
    ("semicolon",       f"{A}; {B}."),
    ("em dash U+2014",  f"{A} {EM_DASH} {B}."),
    ("ellipsis U+2026", f"{A}{ELLIPSIS} {B}."),
    # The lookalikes, for contrast. An earlier round measured THESE, found
    # nothing, and concluded dashes do not work. They are not the same
    # characters as the two above.
    ("hyphen (junk)",   f"{A} - {B}."),
    ("two hyphens",     f"{A} -- {B}."),
    ("three stops",     f"{A}... {B}."),
]
print(f"  {'mark':17} {'dur':>6} {'longest internal pause':>24}")
for name, text in MARKS:
    pcm = synth(text)
    if not pcm:
        print(f"  {name:17}  NO AUDIO")
        continue
    a = np.frombuffer(pcm, dtype="<i2").astype(np.float32) / 32768.0
    save("pause_" + name.split()[0], pcm)
    print(f"  {name:17} {len(a)/RATE:5.2f}s {middle_gap(a):20}ms")
print("  The prompt asks for the em dash and the ellipsis specifically. If they")
print("  do not separate from comma and semicolon here, say so and the rule can")
print("  be simplified. Note the last row never happens on a real call: three")
print("  full stops end the sentence for our aggregator, so the two halves go")
print("  to kokoro as separate requests.")

print(f"\nwavs in {OUT}")
