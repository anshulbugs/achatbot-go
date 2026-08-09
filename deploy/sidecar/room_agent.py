"""Join a Daily room and bridge its audio to the Go voice pipeline.

WHY THIS PROCESS EXISTS. A browser call reaches the agent through a Daily room,
and Daily has no Go SDK. The first version had Telnyx dial the room's SIP
endpoint, which works and sounds exactly like what it is — G.711 at 8 kHz. Next
to our own test page the difference is obvious. This joins the room natively
instead, so the audio stays wideband end to end, and it drops the carrier leg
every browser call used to burn.

    browser ──► Daily room ◄── this process ──websocket──► Go pipeline

RATES. Daily speaks 48 kHz; the pipeline speaks 16 kHz in and (usually) 24 kHz
out. Resampling happens here rather than in Go because numpy makes it a few
lines and the Go side already has enough audio-format code.

Usage:
    python3 room_agent.py --room-url URL --token TOKEN --session ID --agent-ws WS
"""
from __future__ import annotations

import argparse
import logging
import queue
import signal
import time
import sys
import threading

import numpy as np
from daily import CallClient, Daily
from websocket import WebSocketApp

log = logging.getLogger("room-agent")

# How long to sit in an empty room before giving up. The link has to reach a
# person and be clicked, so this is generous — but a sidecar per abandoned room
# is how a box quietly fills with processes.
JOIN_TIMEOUT = 120

# Matches the Go side's roomInterruptMute. Both ends drop the interrupted
# turn's tail because either alone leaves a window: the Go side cannot unsend
# what is already on the wire, and this cannot unhear what was already queued.
INTERRUPT_FENCE = 0.4

# Echo gating.
#
# The agent hears itself. Daily's browser client cancels echo well when the
# caller wears headphones and imperfectly when they do not, so the agent's own
# voice comes back through the caller's microphone, the pipeline's VAD reads it
# as barge-in, and generation is cancelled and restarted. A real call showed
# bursts of five to eight interrupts firing while the agent was mid-sentence:
# the reply's opening was cut and latency went from ~950ms to ~3000ms because
# every cancellation started the turn over.
#
# So while the agent is speaking, inbound audio has to clear a bar to count as
# speech. Echo is attenuated by the caller's own speakers and microphone, so it
# arrives much quieter than the caller does — which is the whole basis for
# telling them apart without an acoustic model.
ECHO_GATE_MULT = 3.0        # inbound must exceed this multiple of the echo floor
ECHO_FLOOR_MIN = 250.0      # absolute floor, so silence cannot set a low bar
GATE_HOLD = 0.8             # once the caller is through, pass everything for this
                            # long: it must exceed the pauses inside a sentence

DAILY_RATE = 48000
PIPELINE_IN_RATE = 16000
# 20 ms of 48 kHz mono s16 — matches Daily's own frame size, so reads never
# straddle two of its internal buffers.
READ_FRAMES = DAILY_RATE // 50


def resample(pcm: np.ndarray, src: int, dst: int) -> np.ndarray:
    """Linear resample of mono int16.

    Linear rather than a windowed sinc on purpose: this runs per frame on every
    concurrent call, speech is band-limited well below Nyquist at both ends, and
    the artefacts a better filter would remove are inaudible over a voice call.
    CPU per call is the budget that matters here.
    """
    if src == dst or pcm.size == 0:
        return pcm
    n_out = int(round(pcm.size * dst / src))
    if n_out <= 0:
        return np.zeros(0, dtype=np.int16)
    x_in = np.linspace(0.0, 1.0, pcm.size, endpoint=False)
    x_out = np.linspace(0.0, 1.0, n_out, endpoint=False)
    return np.interp(x_out, x_in, pcm.astype(np.float32)).astype(np.int16)


class RoomAgent:
    def __init__(self, room_url: str, token: str, session: str, agent_ws: str):
        self.room_url = room_url
        self.token = token
        self.session = session
        self.agent_ws = f"{agent_ws}?session={session}"

        self.stopping = threading.Event()
        self.ws: WebSocketApp | None = None
        self.ws_ready = threading.Event()

        # Outbound audio waiting to be played into the room. Bounded: if the
        # room stalls, dropping the oldest audio is right — it is stale speech
        # nobody wants to hear late, and an unbounded queue would grow until the
        # process died.
        self.playback: queue.Queue[bytes] = queue.Queue(maxsize=100)
        # The pipeline tells us its output rate implicitly; assume 24 kHz until
        # proven otherwise, which is what the TTS emits.
        self.out_rate = 24000
        # When the caller last barged in. Audio that arrives within the fence
        # after it belongs to the interrupted turn: the Go side mutes for 400ms
        # for the same reason, and this is the second line of defence for
        # anything already in flight when the interrupt was sent.
        self.interrupted_at = 0.0
        # Echo gate state. playing_until is when the audio we have written
        # finishes playing, echo_floor is a running estimate of how loud our own
        # voice comes back, and gate_open_until is set once the caller has
        # genuinely broken through so the rest of their sentence passes.
        self.playing_until = 0.0
        self.echo_floor = ECHO_FLOOR_MIN
        self.gate_open_until = 0.0

        Daily.init()
        # Virtual devices are created through the factory, never constructed
        # directly — the classes exist for typing and refuse instantiation.
        #
        # Names must be unique per process. They are per-session so several
        # sidecars could share one process later without fighting over a device.
        self.speaker = Daily.create_speaker_device(
            f"spk-{session[:8]}", sample_rate=DAILY_RATE, channels=1)
        self.mic = Daily.create_microphone_device(
            f"mic-{session[:8]}", sample_rate=DAILY_RATE, channels=1)
        # Selecting the speaker is what routes the room's mixed audio into it.
        # Without this read_frames blocks forever on silence.
        Daily.select_speaker_device(f"spk-{session[:8]}")
        self.client = CallClient()
        self.mic_name = f"mic-{session[:8]}"

    # ── websocket ──────────────────────────────────────────────────

    def _on_ws_open(self, _ws):
        log.info("connected to the agent pipeline")
        self.ws_ready.set()

    def _on_ws_message(self, _ws, message):
        # Text frames are control. Binary frames are audio for the room.
        if isinstance(message, str):
            if message.strip().lower() == "interrupt":
                # Barge-in, and it has to clear TWO buffers, not one.
                #
                # Ours: everything queued here but not yet written.
                dropped = 0
                while not self.playback.empty():
                    try:
                        self.playback.get_nowait()
                        dropped += 1
                    except queue.Empty:
                        break
                # Daily's: audio already written to the virtual microphone is
                # held by the SDK and keeps playing regardless of what we do
                # here. Clearing our queue alone leaves the caller hearing the
                # tail of the reply they just interrupted — which is exactly
                # what a browser call did.
                try:
                    self.mic.write_frames(b"")
                except Exception:  # noqa: BLE001
                    pass
                self.interrupted_at = time.monotonic()
                # Stop believing we are speaking: the reply was just cancelled,
                # so anything arriving now is the caller, not an echo.
                self.playing_until = 0.0
                self.gate_open_until = self.interrupted_at + GATE_HOLD
                if dropped:
                    log.info("interrupt: dropped %d queued chunks", dropped)
            return
        try:
            self.playback.put_nowait(message)
        except queue.Full:
            # Keep the newest audio, drop the oldest: late speech is worse than
            # missing speech.
            try:
                self.playback.get_nowait()
                self.playback.put_nowait(message)
            except queue.Empty:
                pass

    def _on_ws_close(self, _ws, code, msg):
        log.info("pipeline closed (%s %s)", code, msg)
        self.stopping.set()

    def _on_ws_error(self, _ws, err):
        log.error("pipeline error: %s", err)
        self.stopping.set()

    # ── audio pumps ────────────────────────────────────────────────

    def _pump_room_to_pipeline(self):
        """Read the room's mixed audio and forward it as 16 kHz PCM."""
        while not self.stopping.is_set():
            try:
                buf = self.speaker.read_frames(READ_FRAMES)
            except Exception as e:  # noqa: BLE001
                log.error("speaker read failed: %s", e)
                break
            if not buf:
                continue
            pcm48 = np.frombuffer(buf, dtype=np.int16)
            if not self._passes_echo_gate(pcm48):
                continue
            pcm16 = resample(pcm48, DAILY_RATE, PIPELINE_IN_RATE)
            if pcm16.size and self.ws is not None:
                try:
                    self.ws.send(pcm16.tobytes(), opcode=0x2)  # binary
                except Exception as e:  # noqa: BLE001
                    log.error("send to pipeline failed: %s", e)
                    break
        self.stopping.set()

    def _passes_echo_gate(self, pcm: np.ndarray) -> bool:
        """Should this inbound frame reach the pipeline?

        Everything passes while the agent is silent — that is the normal case
        and gating it would only add a way to lose the caller's words. The gate
        only closes over our own speech, and only for audio quiet enough to be
        an echo of it.
        """
        now = time.monotonic()
        if now >= self.playing_until:
            # Agent is silent. Nothing to echo, so nothing to gate.
            return True
        if now < self.gate_open_until:
            # The caller already broke through; do not chop the rest of their
            # sentence into pieces on its internal pauses.
            return True
        if pcm.size == 0:
            return True

        rms = float(np.sqrt(np.mean(pcm.astype(np.float32) ** 2)))
        # Track the loudest echo seen while speaking, decaying slowly so one
        # loud moment does not deafen the gate for the rest of the call.
        self.echo_floor = max(ECHO_FLOOR_MIN, self.echo_floor * 0.995, rms * 0.6)
        if rms > self.echo_floor * ECHO_GATE_MULT:
            self.gate_open_until = now + GATE_HOLD
            return True
        return False

    def _pump_pipeline_to_room(self):
        """Play the agent's replies into the room."""
        while not self.stopping.is_set():
            try:
                chunk = self.playback.get(timeout=0.2)
            except queue.Empty:
                continue
            if time.monotonic() - self.interrupted_at < INTERRUPT_FENCE:
                continue  # tail of the turn the caller just cut off
            pcm = np.frombuffer(chunk, dtype=np.int16)
            pcm48 = resample(pcm, self.out_rate, DAILY_RATE)
            try:
                self.mic.write_frames(pcm48.tobytes())
            except Exception as e:  # noqa: BLE001
                log.error("mic write failed: %s", e)
                break
            # Extend how long we believe our own voice is audible, which is what
            # the echo gate keys off. Measured from now rather than accumulated,
            # so a stall cannot leave the gate closed indefinitely.
            played = pcm48.size / DAILY_RATE
            self.playing_until = max(self.playing_until, time.monotonic()) + played
        self.stopping.set()

    # ── lifecycle ──────────────────────────────────────────────────

    def run(self) -> int:
        self.ws = WebSocketApp(
            self.agent_ws,
            on_open=self._on_ws_open,
            on_message=self._on_ws_message,
            on_close=self._on_ws_close,
            on_error=self._on_ws_error,
        )
        threading.Thread(target=self.ws.run_forever, daemon=True).start()
        if not self.ws_ready.wait(timeout=15):
            log.error("pipeline did not accept the connection")
            return 1

        log.info("joining %s", self.room_url)
        self.client.update_subscription_profiles({
            # Audio only. Subscribing to video would pull megabits per call for
            # frames nothing here ever looks at.
            "base": {"camera": "unsubscribed", "microphone": "subscribed"}
        })
        self.client.join(
            self.room_url,
            self.token,
            client_settings={
                "inputs": {
                    "camera": False,
                    "microphone": {
                        "isEnabled": True,
                        "settings": {"deviceId": self.mic_name},
                    },
                }
            },
            completion=self._on_joined,
        )

        threading.Thread(target=self._pump_room_to_pipeline, daemon=True).start()
        threading.Thread(target=self._pump_pipeline_to_room, daemon=True).start()

        # Two different waits, and conflating them was a bug: at startup the room
        # is empty because the caller has not arrived YET, which looks identical
        # to the room being empty because they have LEFT. Leaving on the first
        # reading meant the sidecar quit a second after joining, every time.
        deadline = time.monotonic() + JOIN_TIMEOUT
        seen_caller = False
        while not self.stopping.wait(timeout=1.0):
            others = self._other_participants()
            if others > 0:
                if not seen_caller:
                    log.info("the caller joined")
                seen_caller = True
                continue
            if seen_caller:
                log.info("the caller left; shutting down")
                break
            if time.monotonic() > deadline:
                log.info("nobody joined within %ds; shutting down", JOIN_TIMEOUT)
                break
        self.shutdown()
        return 0

    def _on_joined(self, data, error):
        if error:
            log.error("join failed: %s", error)
            self.stopping.set()
        else:
            log.info("joined the room")

    def _other_participants(self) -> int:
        """How many participants besides us are in the room."""
        try:
            counts = self.client.participant_counts()
        except Exception:  # noqa: BLE001
            # Treat an unreadable count as "someone is there": guessing empty
            # would end a live call over a transient API error.
            return 1
        return max(0, counts.get("present", 0) - 1)

    def shutdown(self):
        self.stopping.set()
        try:
            self.client.leave()
        except Exception:  # noqa: BLE001
            pass
        try:
            if self.ws is not None:
                self.ws.close()
        except Exception:  # noqa: BLE001
            pass
        Daily.deinit()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--room-url", required=True)
    ap.add_argument("--token", default="")
    ap.add_argument("--session", required=True)
    ap.add_argument("--agent-ws", default="ws://127.0.0.1:4399/room/media")
    ap.add_argument("--log-level", default="INFO")
    args = ap.parse_args()

    logging.basicConfig(
        level=args.log_level,
        format="%(asctime)s sidecar[" + args.session[:8] + "] %(levelname)s %(message)s",
    )

    agent = RoomAgent(args.room_url, args.token, args.session, args.agent_ws)
    signal.signal(signal.SIGTERM, lambda *_: agent.stopping.set())
    signal.signal(signal.SIGINT, lambda *_: agent.stopping.set())
    return agent.run()


if __name__ == "__main__":
    sys.exit(main())
