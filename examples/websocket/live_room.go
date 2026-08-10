package main

// Live listening: a Daily room per watched call, bridged to the phone leg so an
// operator can hear the conversation and speak into it.
//
// GATED ON REDIS, DELIBERATELY. Daily bills per participant-minute and every
// bridged room also runs a second Telnyx leg for its whole duration, so doing
// this for every call would be a large bill for a feature almost nobody opens.
// The dispatch's Redis details are the platform's own signal that something is
// watching this call, so they are what turns it on. A dispatch without them
// costs nothing extra and behaves exactly as before.

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"achatbot/pkg/daily"
)

// dailyClient is nil unless DAILY_API_KEY is set, which disables live rooms
// entirely. Every method on it is nil-safe.
var dailyClient *daily.Client

// roomTTL bounds a room's life.
//
// Comfortably longer than max_call_secs so a long call never loses its room
// mid-conversation, and short enough that a room left behind by a crash stops
// billing the same hour. Rooms are also deleted explicitly when the call ends.
const roomTTL = 2 * time.Hour

// webrtcRoomTTL bounds a browser room. Same reasoning as roomTTL, and the same
// value: the ceiling on a room's life should not depend on who is calling.
const webrtcRoomTTL = 2 * time.Hour

func initDaily(apiKey string) {
	dailyClient = daily.New(apiKey)
	if dailyClient != nil {
		log.Printf("rexa: live listening enabled (Daily rooms for dispatches carrying redis details)")
	}
}

// The Daily sidecar: a Python process that joins a room and pipes its audio to
// /room/media. See deploy/sidecar/room_agent.py.
//
// Python because Daily has no Go SDK. The alternative — having Telnyx dial the
// room's SIP endpoint — works and sounds like a phone call, because it is one:
// G.711 at 8 kHz. This keeps browser calls wideband and drops the carrier leg
// they used to burn.
var (
	sidecarPython string
	sidecarScript string

	sidecarMu sync.Mutex
	// sidecars tracks one process per session so a call that ends early does
	// not leave a process sitting in an empty room. The script leaves on its
	// own when the room empties; this is the belt to that braces.
	sidecars = map[string]*exec.Cmd{}
)

// initSidecar locates the interpreter and script. Both must exist or browser
// calls are refused up front rather than returning a room nobody joins.
func initSidecar(python, script string) {
	if python == "" || script == "" {
		return
	}
	if _, err := os.Stat(python); err != nil {
		log.Printf("rexa: sidecar python not found at %s — browser calls disabled", python)
		return
	}
	if _, err := os.Stat(script); err != nil {
		log.Printf("rexa: sidecar script not found at %s — browser calls disabled", script)
		return
	}
	sidecarPython, sidecarScript = python, script
	log.Printf("rexa: browser rooms enabled (sidecar %s)", script)
}

// sidecarReady reports whether browser calls can be served.
func sidecarReady() bool { return sidecarPython != "" && sidecarScript != "" }

// startSidecar launches the room agent for one session.
//
// agentWS is where it sends the room's audio. Always loopback: the sidecar runs
// beside the agent, so routing its audio out through the public tunnel and back
// would add latency and a dependency on the tunnel being up.
func startSidecar(sessionID, roomURL, token, agentWS string) error {
	cmd := exec.Command(sidecarPython, sidecarScript,
		"--room-url", roomURL,
		"--token", token,
		"--session", sessionID,
		"--agent-ws", agentWS)
	// Its logs are the only view into what happened inside the room, so they go
	// to ours rather than to a file nobody reads.
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		return err
	}
	sidecarMu.Lock()
	sidecars[sessionID] = cmd
	sidecarMu.Unlock()

	go func() {
		err := cmd.Wait()
		sidecarMu.Lock()
		delete(sidecars, sessionID)
		sidecarMu.Unlock()
		if err != nil && !strings.Contains(err.Error(), "signal: terminated") {
			log.Printf("rexa: sidecar for session=%s exited: %v", sessionID, err)
		}
	}()
	return nil
}

// stopSidecar ends the room agent for a session, if one is running.
func stopSidecar(sessionID string) {
	sidecarMu.Lock()
	cmd := sidecars[sessionID]
	delete(sidecars, sessionID)
	sidecarMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		// SIGTERM, not Kill: the script leaves the room cleanly on it, and a
		// participant that vanishes without leaving takes Daily a while to time
		// out — during which the room still bills.
		_ = cmd.Process.Signal(os.Interrupt)
	}
}

// startLiveRoom creates a room for this call and publishes the join link.
//
// Returns the room name so the caller can delete it, and "" when live listening
// is off for this call — which is the common case.
//
// Runs BEFORE the dial so the link is in Redis while the phone is still
// ringing. An operator watching a wallboard wants to jump on the interesting
// call as it connects, and a link that appears ten seconds into the
// conversation has already missed the part they wanted.
func startLiveRoom(ctx context.Context, rc *rexaCall, redisConfigured bool) string {
	if dailyClient == nil || rc == nil || !redisConfigured {
		return ""
	}
	// No recording: this room exists so an operator can listen to a PHONE call,
	// and Telnyx is already recording that call end to end. A second copy would
	// bill twice and give the platform two recordings of one conversation.
	room, err := dailyClient.CreateRoom(ctx, roomTTL, false)
	if err != nil || room == nil {
		// Never fail the call over the listening feature. The call is the
		// product; this is a window onto it.
		log.Printf("rexa: session=%s live room creation failed (call continues): %v", rc.sessionID, err)
		return ""
	}
	rc.roomName = room.Name
	rc.roomSIP = room.SIPURI
	rc.joinURL = room.JoinURL
	// The token, NOT just the tokenised URL.
	//
	// This published room.JoinURL with an empty token. The URL does carry the
	// token in its query string, but the consumer hands `room_url` and `token`
	// to the Daily client as separate fields, and an empty token against a
	// PRIVATE room is refused with "You are not invited to this meeting" — the
	// exact error an operator saw when they pressed Join.
	rc.live.JoinDaily(room.URL, room.JoinURL, room.Token)
	if room.Token == "" {
		log.Printf("rexa: session=%s live room %s has NO meeting token — an operator pressing Join will be refused",
			rc.sessionID, room.Name)
	}
	log.Printf("rexa: session=%s live room %s ready", rc.sessionID, room.Name)
	return room.Name
}

// bridgeLiveRoom puts the answered call and the room's SIP endpoint into one
// conference, so audio flows between them.
//
// Conferencing rather than transferring: a transfer hands the leg away and ends
// the agent's conversation, which is the opposite of listening in on it.
//
// Called after the call is answered. Before that there is nothing to listen to,
// and a SIP leg dialled during the ring would bill for the whole ring period on
// a call that may never be picked up.
func bridgeLiveRoom(id string, rc *rexaCall) {
	if rc == nil || rc.roomSIP == "" || rc.client == nil || rc.bridged {
		return
	}
	rc.bridged = true

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The conference is named for the session so it is greppable across
		// Telnyx's dashboard, our logs and the platform's.
		confID, err := rc.client.ConferenceCreate(ctx, "live-"+rc.sessionID, id)
		if err != nil {
			log.Printf("rexa: session=%s conference create failed (call unaffected): %v", rc.sessionID, err)
			return
		}
		sipLeg, err := rc.client.DialSIP(ctx, rc.roomSIP, "", "")
		if err != nil {
			log.Printf("rexa: session=%s SIP dial to room failed (call unaffected): %v", rc.sessionID, err)
			return
		}
		// Muted: an operator dropping in must not accidentally speak over a
		// live call. They unmute in the Daily UI when they mean to.
		if err := rc.client.ConferenceJoin(ctx, confID, sipLeg, true); err != nil {
			log.Printf("rexa: session=%s room leg failed to join conference: %v", rc.sessionID, err)
			return
		}
		log.Printf("rexa: session=%s live room bridged (conference=%s)", rc.sessionID, confID)
	}()
}

// endLiveRoom deletes the room when the call is over.
//
// Rooms carry an expiry so this is tidiness rather than correctness — but a
// room that outlives its call is a link that still opens, and an operator who
// clicks it hears silence and reports the feature as broken.
func endLiveRoom(rc *rexaCall) {
	if dailyClient == nil || rc == nil || rc.roomName == "" {
		return
	}
	name := rc.roomName
	rc.roomName = ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := dailyClient.DeleteRoom(ctx, name); err != nil {
			log.Printf("rexa: live room %s delete failed (it will expire on its own): %v", name, err)
		}
	}()
}

// liveJoinURLFor is used by the sentiment event, which carries the join link so
// an operator alerted mid-call can act on it in one click rather than going to
// look the call up.
func liveJoinURLFor(rc *rexaCall) string {
	if rc == nil {
		return ""
	}
	return rc.joinURL
}

// watchForListener bridges the call into its room the moment an operator
// arrives, and gives up when the call ends.
//
// WHY POLL AT ALL. bridgeLiveRoom cannot simply run when the call is answered:
// `allow_sip_only_in_room` is false on this Daily domain, so a SIP participant
// that would be alone in the room is rejected with SIP 480 — a failure that
// reads as a network fault rather than as the policy it is. The phone leg has
// to arrive AFTER the human, so something has to notice the human arriving.
// Daily can push that as a webhook, but registering one needs a stable callback
// URL and ours is a tunnel that renames itself on every restart.
//
// The cost is one presence check per interval per watched call, and it stops at
// the first listener or when the call leaves the registry — so a call nobody
// opens pays for the length of the call and nothing after it.
func watchForListener(id string, rc *rexaCall) {
	if dailyClient == nil || rc == nil || rc.roomName == "" || rc.roomSIP == "" {
		return
	}
	room, session := rc.roomName, rc.sessionID
	go func() {
		// Slow enough to be cheap across a full campaign, fast enough that an
		// operator who clicks Join is not left in silence wondering whether it
		// worked.
		const interval = 5 * time.Second
		ctx := context.Background()
		for {
			time.Sleep(interval)
			// The call is the clock. When it is gone from the registry it has
			// hung up, and there is nothing left to listen to.
			if calls.get(id) == nil {
				return
			}
			n, err := dailyClient.Presence(ctx, room)
			if err != nil {
				// Transient: keep watching. A presence check that fails once
				// is not a reason to deny the operator the feature for the
				// rest of the call.
				continue
			}
			if n == 0 {
				continue
			}
			log.Printf("rexa: session=%s listener joined room %s — bridging the call in", session, room)
			bridgeLiveRoom(id, rc)
			return
		}
	}()
}
