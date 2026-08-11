package main

// Live-listening relay: a phone call's audio into an operator's Daily room,
// without a SIP leg.
//
// WHY THIS EXISTS. Bridging the operator in over SIP costs about five seconds,
// every time, and none of it is ours: Daily only registers a room's SIP
// endpoint once a session exists, and the operator's own join is what starts
// the session, so registration is serialised in front of our INVITE. Measured
// repeatedly at 4.5-5.3s.
//
// Nothing about that is needed. We already hold the call's audio on the Telnyx
// media socket, and we already run a sidecar that can join a Daily room and
// move PCM in both directions — it is what browser calls use. So the operator's
// room is fed from the socket we have, and the SIP leg, the conference and the
// second carrier leg all go away.
//
// THE WIRE IS THE SIDECAR'S EXISTING ONE, deliberately unchanged: binary frames
// of raw signed 16-bit little-endian mono PCM, room audio inbound, call audio
// outbound, and the text word "interrupt" as the only control message. Reusing
// deploy/sidecar/room_agent.py as-is means the fixed-clock writer and the echo
// gate it already got right are not reimplemented here — see the browser-call
// path, where writing those twice cost three separate audio bugs.

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"achatbot/pkg/consts"
	"achatbot/pkg/telnyx"
)

// relayRate is the PCM rate on the sidecar wire. Matches the browser path so
// one script serves both.
const relayRate = consts.DefaultRate

// liveRelay is one operator's audio bridge for one call.
type liveRelay struct {
	callID string

	mu   sync.Mutex
	ws   *websocket.Conn
	done bool
}

var (
	relayMu sync.Mutex
	// relays are keyed by SESSION id, because that is what the sidecar is
	// launched with and what it reconnects as.
	relays = map[string]*liveRelay{}
)

// registerRelay reserves a slot for a session about to have a sidecar attached.
func registerRelay(sessionID, callID string) *liveRelay {
	r := &liveRelay{callID: callID}
	relayMu.Lock()
	relays[sessionID] = r
	relayMu.Unlock()
	return r
}

// endRelay tears a relay down and detaches its tap.
func endRelay(sessionID string) {
	relayMu.Lock()
	r := relays[sessionID]
	delete(relays, sessionID)
	relayMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.done = true
	ws := r.ws
	r.ws = nil
	r.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
	stopSidecar(sessionID)
}

// sendToRoom forwards one chunk of caller audio to the operator. Dropped
// silently when the sidecar has not connected yet or has gone.
func (r *liveRelay) sendToRoom(pcm []byte) {
	r.mu.Lock()
	ws := r.ws
	done := r.done
	r.mu.Unlock()
	if ws == nil || done || len(pcm) == 0 {
		return
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		// One writer, one socket: a failed write means the sidecar is gone.
		r.mu.Lock()
		r.done = true
		r.mu.Unlock()
	}
}

// handleRelayMedia accepts the listen-in sidecar's connection.
//
// Mirror image of /room/media: there, the room is the CALLER and the pipeline
// speaks to it. Here the room is an OPERATOR listening to a call that already
// has both parties on it.
func handleRelayMedia(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	relayMu.Lock()
	relay := relays[sessionID]
	relayMu.Unlock()
	if relay == nil {
		http.Error(w, "unknown relay", http.StatusNotFound)
		return
	}

	ws, err := roomUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("relay: upgrade failed for session=%s: %v", sessionID, err)
		return
	}
	defer ws.Close()

	relay.mu.Lock()
	relay.ws = ws
	relay.mu.Unlock()
	log.Printf("relay: listen-in sidecar connected for session=%s", sessionID)

	// Caller -> operator. The tap runs on the pipeline's own read goroutine, so
	// it does nothing but resample and hand the bytes to the socket.
	if hooked := calls.setInboundTap(relay.callID, func(pcm8 []byte, rate int) {
		relay.sendToRoom(telnyx.ResamplePCM16(pcm8, rate, relayRate))
	}); !hooked {
		log.Printf("relay: could not tap call audio for session=%s — the call may have ended", sessionID)
	}
	defer calls.setInboundTap(relay.callID, nil)

	// ONE WAY, BY CONSTRUCTION. Anything the sidecar sends is read and thrown
	// away — there is no code path from this socket into the call.
	//
	// This is the whole point of the listen relay. Bridging a listener over SIP
	// puts the call in a conference, the conference mix comes back on our media
	// fork, and the agent transcribes itself: 55 turns in 37 seconds, every one
	// its own TTS, on a call where nobody was speaking. A relay that cannot
	// write to the call cannot build that loop, whatever the sidecar does.
	//
	// Draining rather than ignoring: an unread socket eventually blocks the
	// sidecar on a full send buffer.
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			log.Printf("relay: sidecar for session=%s disconnected: %v", sessionID, err)
			return
		}
	}
}
