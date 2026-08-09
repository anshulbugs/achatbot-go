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
import sys
import threading

import numpy as np
from daily import CallClient, Daily
from websocket import WebSocketApp

log = logging.getLogger("room-agent")

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
                # Barge-in. Drop everything queued: the caller has started
                # talking and the rest of the previous reply is now noise they
                # deliberately cut off.
                dropped = 0
                while not self.playback.empty():
                    try:
                        self.playback.get_nowait()
                        dropped += 1
                    except queue.Empty:
                        break
                log.debug("interrupt: dropped %d queued chunks", dropped)
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
            pcm16 = resample(pcm48, DAILY_RATE, PIPELINE_IN_RATE)
            if pcm16.size and self.ws is not None:
                try:
                    self.ws.send(pcm16.tobytes(), opcode=0x2)  # binary
                except Exception as e:  # noqa: BLE001
                    log.error("send to pipeline failed: %s", e)
                    break
        self.stopping.set()

    def _pump_pipeline_to_room(self):
        """Play the agent's replies into the room."""
        while not self.stopping.is_set():
            try:
                chunk = self.playback.get(timeout=0.2)
            except queue.Empty:
                continue
            pcm = np.frombuffer(chunk, dtype=np.int16)
            pcm48 = resample(pcm, self.out_rate, DAILY_RATE)
            try:
                self.mic.write_frames(pcm48.tobytes())
            except Exception as e:  # noqa: BLE001
                log.error("mic write failed: %s", e)
                break
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

        while not self.stopping.wait(timeout=1.0):
            # Leave when the caller does. Nobody else is coming, and a process
            # per abandoned room is how a box runs out of memory overnight.
            if self._room_empty():
                log.info("the caller left; shutting down")
                break
        self.shutdown()
        return 0

    def _on_joined(self, data, error):
        if error:
            log.error("join failed: %s", error)
            self.stopping.set()
        else:
            log.info("joined the room")

    def _room_empty(self) -> bool:
        try:
            counts = self.client.participant_counts()
        except Exception:  # noqa: BLE001
            return False
        # We are always present, so "one participant" means only us.
        return counts.get("present", 0) <= 1

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
