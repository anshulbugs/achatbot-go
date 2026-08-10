"""Hold a Daily room's session open so its SIP endpoint stays registered.

Daily does not register a room's SIP endpoint when the room is created. It
registers when a SESSION starts, and a session needs a WebRTC participant --
which means the supervisor's own join is what starts the clock. Their SIP leg
then waits for registration to finish, and that wait measured 4.8-5.3s on every
barge. It is the single largest part of the delay.

A SIP leg cannot start the session itself: the endpoint it would dial is not
listening until the session exists. Something WebRTC has to be first, and this
is that something -- a participant that joins when the call is answered,
publishes nothing, subscribes to nothing, and exists only so that registration
happens during the conversation instead of during the handover.

Measured: dialin-ready fires 1.9s after the join, and the process holds ~34MB.
By the time anyone barges, the endpoint has been ready for the whole call.

Deliberately not the listen-in sidecar. That one carries audio, so a fault in
it is a fault on the call. This one touches no media path at all: if it dies,
the barge is merely as slow as it was before.
"""
from __future__ import annotations

import argparse
import signal
import sys
import threading
import time

from daily import CallClient, Daily, EventHandler

# Registration took 1.9s when measured. Well past that and something is wrong
# -- a revoked token, a deleted room -- and saying so beats sitting silent for
# the length of a call.
READY_TIMEOUT = 30.0


class Prewarmer(EventHandler):
    def __init__(self) -> None:
        super().__init__()
        self.ready = threading.Event()
        self.started = time.monotonic()

    def on_dialin_ready(self, sip_endpoint) -> None:
        print(
            f"[prewarm] sip endpoint registered after "
            f"{time.monotonic() - self.started:.2f}s: {sip_endpoint}",
            flush=True,
        )
        self.ready.set()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--room-url", required=True)
    ap.add_argument("--token", required=True)
    ap.add_argument("--session", required=True)
    args = ap.parse_args()

    Daily.init()
    warmer = Prewarmer()
    client = CallClient(event_handler=warmer)

    joined = threading.Event()
    outcome: dict[str, object] = {}

    def on_joined(_data, error) -> None:
        outcome["error"] = error
        joined.set()

    # Publishes nothing and subscribes to nothing. The supervisor sees a muted
    # participant; the caller hears nothing, because there is no audio path
    # between this process and the call at all.
    client.join(
        args.room_url,
        args.token,
        client_settings={"inputs": {"camera": False, "microphone": False}},
        completion=on_joined,
    )
    if not joined.wait(timeout=READY_TIMEOUT) or outcome.get("error"):
        print(f"[prewarm] join failed for session={args.session}: {outcome.get('error')}", flush=True)
        return 1

    if not warmer.ready.wait(timeout=READY_TIMEOUT):
        # Stay anyway. The session is what registration needs, and holding it
        # is still the useful thing even if we missed the event.
        print(f"[prewarm] no dialin-ready within {READY_TIMEOUT:.0f}s for session={args.session}", flush=True)

    # Hold the session until the agent ends the call. Leaving earlier would
    # give back the registration this exists to keep.
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    stop.wait()

    client.leave()
    print(f"[prewarm] left room for session={args.session}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
