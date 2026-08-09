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

func initDaily(apiKey string) {
	dailyClient = daily.New(apiKey)
	if dailyClient != nil {
		log.Printf("rexa: live listening enabled (Daily rooms for dispatches carrying redis details)")
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
	room, err := dailyClient.CreateRoom(ctx, roomTTL)
	if err != nil || room == nil {
		// Never fail the call over the listening feature. The call is the
		// product; this is a window onto it.
		log.Printf("rexa: session=%s live room creation failed (call continues): %v", rc.sessionID, err)
		return ""
	}
	rc.roomName = room.Name
	rc.roomSIP = room.SIPURI
	rc.live.JoinURL(room.JoinURL)
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
