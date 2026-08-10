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
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
	// DELIBERATELY DOES NOTHING NOW.
	//
	// The room used to be created here, before the dial, so the join link was
	// in Redis while the phone rang. That turned out to be the reason barging
	// took ten seconds: the dialer stashes the link from our join_daily event
	// and then takes an "already have a room" shortcut past the very request
	// that would have told us the operator had pressed Join
	// (rexa-dialer calls.py `_bridge_daily_room`). Left with no push signal, the
	// agent could only poll Daily's presence API, which lags a join by about
	// five and a half seconds.
	//
	// The room is now made on demand in handleJoinCall, which also means a room
	// exists only for calls somebody actually opened rather than one per
	// dispatch.
	return ""
}

// bridgeLiveRoom puts the call and the room's SIP endpoint into one conference,
// so audio flows between the caller and whoever is in the room.
//
// Conferencing rather than transferring: a transfer hands the leg away and ends
// the agent's conversation, which is the opposite of listening in on it.
//
// RETRIES THE WHOLE DIAL, not just the join. Two carrier-side facts make a
// single attempt unreliable, and they pull in opposite directions:
//
//   - `allow_sip_only_in_room` is false on this Daily domain, so a SIP leg that
//     would be alone in the room is rejected with SIP 480 — and this runs the
//     moment the operator is handed the URL, a beat before their browser is
//     actually in the room.
//   - A leg that has been dialled cannot be conferenced until it has been
//     answered: 422 "Call not answered yet", which is what left one real barge
//     with a conference the SIP leg never entered.
//
// So the outer loop redials a leg that died, and the inner one waits for a leg
// that is still being answered.
func bridgeLiveRoom(id string, rc *rexaCall) {
	if rc == nil || rc.roomSIP == "" || rc.client == nil || rc.bridged {
		return
	}
	rc.bridged = true

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// The conference is named for the session so it is greppable across
		// Telnyx's dashboard, our logs and the platform's.
		confID, err := rc.client.ConferenceCreate(ctx, "live-"+rc.sessionID, id)
		if err != nil {
			log.Printf("rexa: session=%s conference create failed (call unaffected): %v", rc.sessionID, err)
			rc.bridged = false
			return
		}

		for dial := 0; dial < 12; dial++ {
			if calls.get(id) == nil {
				return // the call ended while we were setting this up
			}
			sipLeg, err := rc.client.DialSIP(ctx, rc.roomSIP, "", "")
			if err != nil {
				log.Printf("rexa: session=%s SIP dial to room failed: %v", rc.sessionID, err)
				time.Sleep(time.Second)
				continue
			}
			// UNMUTED: the agent is about to leave, and a muted operator in an
			// agentless call is silence for the caller.
			joinErr := error(nil)
			for attempt := 0; attempt < 24; attempt++ {
				joinErr = rc.client.ConferenceJoin(ctx, confID, sipLeg, false)
				if joinErr == nil {
					log.Printf("rexa: session=%s live room bridged (conference=%s)", rc.sessionID, confID)
					// The operator has the call now — step out of it. Asked for
					// explicitly: barging should behave like a transfer, with
					// the agent gone rather than talking over the person who
					// just took over.
					leaveCall(id, rc.client, "operator barged in")
					return
				}
				if !strings.Contains(joinErr.Error(), "not answered yet") {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			log.Printf("rexa: session=%s room leg not in conference yet (attempt %d): %v",
				rc.sessionID, dial+1, joinErr)
			time.Sleep(time.Second)
		}
		log.Printf("rexa: session=%s gave up bridging the live room", rc.sessionID)
		rc.bridged = false
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
	// Stop watching before the room goes: a poller still holding this entry
	// would try to bridge a call that has hung up.
	unwatchRoom(name)
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

// Rooms being watched for a listener, keyed by Daily room name.
//
// One shared map plus one poller, rather than a goroutine and an API call per
// call: see daily.PresenceAll. What an operator notices is the gap between
// pressing Join and the agent going quiet, and that gap is this interval.
var (
	watchMu      sync.Mutex
	watchedRooms = map[string]watchedRoom{}
	watchPolling bool
)

type watchedRoom struct {
	callID string
	rc     *rexaCall
}

// listenerPollInterval is how often the whole domain's presence is checked.
// One request covers every room, so this is a fixed cost no matter how many
// calls are running.
const listenerPollInterval = time.Second

// watchForListener arranges for the call to be bridged into its room as soon as
// somebody joins it.
//
// bridgeLiveRoom cannot simply run when the call is answered:
// `allow_sip_only_in_room` is false on this Daily domain, so a SIP participant
// that would be alone in the room is rejected with SIP 480 — a failure that
// reads as a network fault rather than as the policy it is. The phone leg has
// to arrive AFTER the human, so something has to notice the human arriving.
func watchForListener(id string, rc *rexaCall) {
	if dailyClient == nil || rc == nil || rc.roomName == "" || rc.roomSIP == "" {
		return
	}
	watchMu.Lock()
	watchedRooms[rc.roomName] = watchedRoom{callID: id, rc: rc}
	start := !watchPolling
	watchPolling = true
	watchMu.Unlock()
	if start {
		go pollForListeners()
	}
}

// unwatchRoom stops watching a room, on bridge or on hangup.
func unwatchRoom(name string) {
	if name == "" {
		return
	}
	watchMu.Lock()
	delete(watchedRooms, name)
	watchMu.Unlock()
}

// pollForListeners runs for the life of the process, checking domain presence
// while any room is being watched and sleeping cheaply when none is.
func pollForListeners() {
	ctx := context.Background()
	for {
		time.Sleep(listenerPollInterval)

		watchMu.Lock()
		waiting := len(watchedRooms)
		watchMu.Unlock()
		if waiting == 0 {
			continue
		}

		present, err := dailyClient.PresenceAll(ctx)
		if err != nil {
			// Transient. A presence check that fails once is not a reason to
			// deny the feature for the rest of the call.
			continue
		}

		watchMu.Lock()
		var ready []watchedRoom
		for room, w := range watchedRooms {
			if present[room] > 0 {
				ready = append(ready, w)
				delete(watchedRooms, room)
			}
		}
		watchMu.Unlock()

		for _, w := range ready {
			// The call may have ended between the poll and here.
			if calls.get(w.callID) == nil {
				continue
			}
			log.Printf("rexa: session=%s listener joined room %s — bridging the call in",
				w.rc.sessionID, w.rc.roomName)
			bridgeLiveRoom(w.callID, w.rc)
		}
	}
}

// handleJoinCall serves the dialer's Join/Barge button.
//
// THIS IS THE SIGNAL WE WERE MISSING. The dialer does not sit quietly when an
// operator presses Join — it issues
// `GET {join_call_url}?uuid=<call_uuid>` and expects a room back
// (rexa-dialer apps/api/app/routers/calls.py `_bridge_daily_room`). We never
// implemented the endpoint, so the only way to notice a listener was to poll
// Daily's presence API — and that API lags a join by about five and a half
// seconds, measured. Five seconds of detection plus a five second bridge is the
// ten seconds an operator spends listening to the agent after taking over.
//
// The request arrives on the click. Answering it removes the detection delay
// completely.
//
// The room is created HERE rather than at dispatch, which also settles the cost
// worry this feature was gated on: a room now exists only for calls somebody
// actually opened, instead of one per dispatched call whether or not anyone
// ever looks.
func handleJoinCall(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("uuid")
	if id == "" {
		http.Error(w, "uuid required", http.StatusBadRequest)
		return
	}
	p := calls.get(id)
	if p == nil || p.platform == nil {
		// 404 is the right answer and the caller expects it: it retries on
		// 0/0.5/1/2s, which covers a Join pressed while the dial is still
		// being registered.
		http.Error(w, "unknown call", http.StatusNotFound)
		return
	}
	rc := p.platform

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := ensureLiveRoom(ctx, rc); err != nil {
		log.Printf("rexa: session=%s join-call could not make a room: %v", rc.sessionID, err)
		http.Error(w, "room unavailable", http.StatusServiceUnavailable)
		return
	}

	// Answer FIRST, bridge after. The operator's browser cannot join a room it
	// has not been told about, and the SIP leg cannot enter a room with nobody
	// in it — allow_sip_only_in_room is false on this domain. So the reply is
	// what unblocks the bridge, and holding it back to bridge first would
	// deadlock the two against each other.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"room_url":       rc.roomURL,
		"daily_room_url": rc.roomURL,
		"token":          rc.roomToken,
		"daily_token":    rc.roomToken,
		"url":            rc.roomURL,
	})
	log.Printf("rexa: session=%s join-call — operator is opening room %s", rc.sessionID, rc.roomName)

	bridgeLiveRoom(id, rc)
}

// ensureLiveRoom creates this call's listen-in room if it does not have one.
func ensureLiveRoom(ctx context.Context, rc *rexaCall) error {
	if dailyClient == nil {
		return errNoDaily
	}
	if rc.roomName != "" {
		return nil
	}
	room, err := dailyClient.CreateRoom(ctx, daily.RoomOptions{TTL: roomTTL, Public: true})
	if err != nil {
		return err
	}
	if room == nil {
		return errNoDaily
	}
	rc.roomName = room.Name
	rc.roomSIP = room.SIPURI
	rc.roomURL = room.URL
	rc.roomToken = room.Token
	rc.joinURL = room.JoinURL
	// Published for the tailer's own record. It arrives after the operator has
	// already been handed the URL directly, which is the point: publishing it
	// up front is what made the dialer take its "already stashed" shortcut and
	// never call us.
	rc.live.JoinDaily(room.URL, room.JoinURL, room.Token)
	log.Printf("rexa: session=%s live room %s ready", rc.sessionID, room.Name)
	return nil
}

var errNoDaily = errors.New("daily is not configured")
