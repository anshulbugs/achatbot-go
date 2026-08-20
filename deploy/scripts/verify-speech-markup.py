"""Verify kokoro's inline speech markup on real hardware, by ear and by ASR.

Run this ONCE on a new box before trusting tts.markup, and again after any
change to the kokoro image. It exists because this pipeline has twice shipped
expressive markup that the engine did not understand and read aloud to a
caller, and reading the misaki source is not the same as hearing the output.

    python3 deploy/scripts/verify-speech-markup.py

Needs the kokoro service on :8880 and parakeet on :8890. Writes wavs to
~/markupcheck so the differences can be heard, not just measured — the stress
question is a quality question and the meter cannot answer it.

What each section decides:

  1. SPOKEN?    Does any bracket reach the caller? A FAIL here means turn
                tts.markup off in config.yaml and stop.
  2. AUDIBLE?   Does [word](+1) actually change the audio? Identical duration
                AND identical energy means misaki parsed it and the model
                ignored it, which makes the prompt rule dead weight.
  3. LEXICON    Does the JobTalk entry fix the two-word reading?
  4. BANNED     The four non-lexical interjections our prompt still forbids
                ("Hmmm", "Ahh", "Ooh", "Aww"). If they come back as sounds
                rather than as spelled-out letters, the ban can be relaxed.
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
    print(f"  [free]({level:2}): {len(a)/RATE:5.2f}s vs {len(base)/RATE:5.2f}s  "
          f"rms {rms(a):.4f} vs {rms(base):.4f}"
          f"{'   <-- no measurable change' if same_len and abs(rms(a)-rms(base)) < 0.001 else ''}")
print("  Listen to stress_up.wav against plain.wav before believing the meter.")

print("\n=== 3. LEXICON: is the brand name one word? ===")
for name, text in (("brand plain", "Welcome to JobTalk, your hiring assistant."),
                   ("brand forced", "Welcome to [JobTalk](/ʤˈɑbtˌɔk/), your hiring assistant.")):
    pcm = synth(text)
    save(name.replace(" ", "_"), pcm)
    print(f"  {name:14} {asr(pcm)[:56]}")

print("\n=== 4. BANNED interjections: sound, or spelled-out letters? ===")
for word in ("Hmmm… let me check.", "Ahh, now I see.", "Ooh — interesting.", "Aww, that is a shame."):
    pcm = synth(word)
    save("interj_" + word[:4].strip(" ,—…"), pcm)
    print(f"  {word:26} -> {asr(pcm)[:44]}")
print("  If these came back as words rather than letters, the prompt ban on the")
print("  four non-lexical interjections can be lifted. Until then it stays.")

print(f"\nwavs in {OUT}")
