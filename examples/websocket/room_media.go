package main

// Media bridge for the Daily sidecar.
//
// WHY THIS EXISTS AT ALL. A browser call reaches the agent through a Daily
// room, and Daily has no Go SDK — so something has to sit in the room on our
// behalf. The first version used Telnyx to dial the room's SIP endpoint, which
// works and sounds like a phone call, because that is exactly what it is:
// G.711 µ-law at 8 kHz. A browser user comparing it to our own test page hears
// the difference immediately.
//
// So a small Python sidecar joins the room natively (deploy/sidecar/) and pipes
// audio here instead. That keeps 48 kHz end to end, and drops the carrier leg
// every browser call used to burn.
//
// THE WIRE IS DELIBERATELY DUMB. Both ends are ours, so there is no protobuf
// and no JSON: binary WebSocket frames of raw signed 16-bit little-endian mono
// PCM, 16 kHz inbound and whatever the pipeline produces outbound. The sidecar
// resamples on its side, where numpy makes it a one-liner. A text frame is a
// control message, currently only "interrupt".

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"

	"achatbot/pkg/consts"
	"achatbot/pkg/rexa"
)

// roomSerializer moves raw PCM between the sidecar and the pipeline.
//
// It does none of what the Telnyx serializer does — no µ-law, no echo
// suppression, no clarity filter — because none of it applies. Daily's client
// does acoustic echo cancellation in the browser, which is the same reason a
// laptop on speakerphone works in any video call, so the half-duplex gating
// that a phone line needs would only cut the caller off mid-sentence here.
type roomSerializer struct {
	mu sync.Mutex

	// NO OUTBOUND MUTE HERE, deliberately — this is the second attempt.
	//
	// The phone path drops outbound audio for 400ms after an interruption, to
	// swallow the interrupted turn's tail. Copying that here clipped the
	// opening of EVERY reply instead: interruption frames fire as the caller
	// stops speaking, which is a few hundred milliseconds before the agent
	// starts, so the fence was still closed when the new turn's first audio
	// arrived. The phone path gets away with it because its reply latency is
	// ~950ms; this path is faster, and the fence ate the difference.
	//
	// The tail is dropped where it can be done precisely instead: the sidecar
	// clears its own playback buffer on the interrupt message, which is the
	// only place that knows what has been queued but not yet spoken.
	lastInterrupt time.Time
	awaitingReply bool
	// onSpoken reports audio actually sent, so an interrupted turn's transcript
	// can be trimmed to what the caller heard.
	onSpoken func(time.Duration)

	// greetingUntil is when the pre-rendered greeting finishes playing.
	//
	// The greeting is handed to the sidecar as one blob and lives in its
	// playback buffer — and an interrupt message CLEARS that buffer. The
	// pipeline emits interruption frames freely in the first seconds of a
	// session, so the greeting was being cut off wherever it had reached: the
	// caller heard the opening and then silence, at a consistent point.
	//
	// Interrupts are suppressed until it has played. The caller cannot
	// meaningfully barge in on a greeting they have not heard yet, and if they
	// talk over it the turn that follows still picks them up.
	greetingUntil time.Time
}

// SetSpokenHook registers a callback fired with the duration of outbound audio.
func (s *roomSerializer) SetSpokenHook(fn func(time.Duration)) {
	s.mu.Lock()
	s.onSpoken = fn
	s.mu.Unlock()
}

// HoldInterrupts suppresses interrupt messages for d, while the pre-rendered
// greeting plays.
func (s *roomSerializer) HoldInterrupts(d time.Duration) {
	s.mu.Lock()
	s.greetingUntil = time.Now().Add(d)
	s.mu.Unlock()
}

// SupportsInterruption reports that barge-in is encoded on the wire, so the
// transport hands us the frame instead of falling back to a client control
// message the sidecar would not understand.
func (*roomSerializer) SupportsInterruption() bool { return true }

// Deserialize turns inbound PCM into a pipeline frame.
//
// Anything not a clean 16-bit sample pair is dropped rather than errored: a
// short read at the end of a stream is normal, and killing the read loop over
// one odd byte would end the call.
func (s *roomSerializer) Deserialize(data []byte) (frames.Frame, error) {
	if len(data) < 2 {
		return nil, nil
	}
	if len(data)%2 == 1 {
		data = data[:len(data)-1]
	}
	return frames.NewAudioRawFrame(data, consts.DefaultRate, 1, 2), nil
}

// Serialize turns an outbound frame into bytes for the sidecar.
//
// Audio goes out as-is: the sidecar knows the rate from the first frame and
// resamples to Daily's 48 kHz itself. An interruption becomes a text frame,
// which is how the sidecar knows to drop audio it has buffered but not yet
// played — without it the caller barges in and still hears the tail of the
// reply they just cut off.
func (s *roomSerializer) Serialize(frame frames.Frame) ([]byte, error) {
	switch f := frame.(type) {
	case *frames.AudioRawFrame:
		s.mu.Lock()
		if s.awaitingReply {
			s.awaitingReply = false
			log.Printf("room: reply latency ~%dms",
				time.Since(s.lastInterrupt).Milliseconds())
		}
		hook := s.onSpoken
		s.mu.Unlock()
		if hook != nil {
			// consts.DefaultRate mono s16: two bytes a sample.
			hook(time.Duration(len(f.Audio)/2) * time.Second / time.Duration(f.SampleRate))
		}
		return f.Audio, nil
	case *frames.StartInterruptionFrame:
		s.mu.Lock()
		holding := time.Now().Before(s.greetingUntil)
		s.lastInterrupt = time.Now()
		s.awaitingReply = true
		s.mu.Unlock()
		if holding {
			// Forwarding this would flush the greeting out of the sidecar's
			// buffer mid-sentence.
			return nil, nil
		}
		return []byte("interrupt"), nil
	default:
		return nil, nil
	}
}

// roomConn adapts a websocket to the transport's connection interface,
// sending audio as binary and control messages as text.
//
// The split matters: the sidecar decides what to do with a payload by frame
// type alone, so a control message arriving as binary would be played as a
// burst of noise.
type roomConn struct {
	*websocket.Conn
	mu sync.Mutex
}

func (c *roomConn) ReadMessage() (consts.MessageType, []byte, error) {
	_, data, err := c.Conn.ReadMessage()
	return consts.BinaryMessage, data, err
}

func (c *roomConn) WriteMessage(_ consts.MessageType, data []byte) error {
	kind := websocket.BinaryMessage
	if isRoomControl(data) {
		kind = websocket.TextMessage
	}
	// Gorilla forbids concurrent writers, and audio frames and interruption
	// control messages come from different goroutines.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.WriteMessage(kind, data)
}

// isRoomControl distinguishes a control word from audio.
//
// Audio is always an even number of bytes and effectively never equals a short
// ASCII keyword, so comparing against the known words is safe and avoids a
// second channel just to carry one string.
func isRoomControl(data []byte) bool {
	return len(data) < 16 && strings.EqualFold(string(data), "interrupt")
}

func (c *roomConn) Close() error { return c.Conn.Close() }

// roomUpgrader accepts the sidecar's connection. Buffers are larger than the
// browser's because 48 kHz audio arrives in bigger chunks.
var roomUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  16384,
	WriteBufferSize: 16384,
}

// handleRoomMedia runs a voice session for a browser call.
//
// The sidecar connects with ?session=<session_id>, which is how this finds the
// dispatch that created the room — its prompt, its voice, and the platform
// context the end-of-call report needs.
func handleRoomMedia(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}
	p := calls.get(sessionID)
	if p == nil {
		// The room outlived its dispatch, or the sidecar reconnected after the
		// call ended. Refusing is right: without the dispatch there is no prompt
		// and no session to report against.
		log.Printf("room: no dispatch for session=%s, refusing media", sessionID)
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	conn, err := roomUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("room: upgrade failed for session=%s: %v", sessionID, err)
		return
	}
	defer conn.Close()

	log.Printf("room: sidecar connected for session=%s", sessionID)
	markOnGPU(sessionID)
	// Play the greeting from the announcement cache, which was filled while the
	// room was being created and the sidecar was joining. A cache hit is a map
	// lookup, so the caller hears the first word as soon as they are in.
	ser := &roomSerializer{}
	if p.Hello != "" {
		if pcm := announcements.get(p.Hello, p.VoiceID, p.Speed, p.Volume); len(pcm) > 0 {
			rate, _, _ := ttsSampleInfo()
			// Suppress interrupts for as long as the greeting plays, plus a
			// little: the sidecar holds it until the caller is in the room, so
			// playback starts later than this send does.
			dur := time.Duration(len(pcm)/2) * time.Second / time.Duration(rate)
			ser.HoldInterrupts(dur + 3*time.Second)
			log.Printf("room: greeting %d bytes (%.1fs) queued for session=%s",
				len(pcm), dur.Seconds(), sessionID)
			go func() {
				// Straight down the wire: the sidecar paces playback on its own
				// fixed clock, so there is nothing to pace here.
				_ = conn.WriteMessage(websocket.BinaryMessage, pcm)
			}()
		}
	}
	if p.platform != nil {
		p.platform.startedAt = time.Now()
	}
	if p.platform != nil {
		// The browser is in the room and audio is flowing, which is the browser
		// equivalent of a human picking up.
		p.platform.live.Event("human_detected", nil)
	}

	var chatObserver func(map[string]any)
	var agentObserver func(string)
	if p.platform != nil && p.platform.transcript != nil {
		p.platform.transcript.SeedGreeting(p.Hello)
		chatObserver = p.platform.transcript.ObserveChatHistory()
		agentObserver = p.platform.transcript.ObserveAgentTurns()
	}
	if obs := sentimentObserver(sessionID, p.platform); obs != nil {
		chatObserver = chainObservers(chatObserver, obs)
	}

	runVoiceSession(&roomConn{Conn: conn}, ser, sessionConfig{
		clientID: "room_" + sessionID,
		// Played out of band above; without this the model greets again.
		spokenGreeting:    p.Hello,
		callID:            sessionID,
		call:              p,
		chatObserver:      chatObserver,
		agentTurnObserver: agentObserver,
		systemPrompt:      p.SystemPrompt,
		voiceID:           p.VoiceID,
		speed:             p.Speed,
		volume:            p.Volume,
		llmModel:          p.LLMModel,
		// Empty on purpose — the greeting is played below from the copy already
		// rendered at dispatch. Left set, runVoiceSession re-synthesizes it at
		// session start, and the caller waits through a TTS render of the whole
		// thing before hearing a word: a real dispatch had a 14.8-SECOND
		// greeting, so that alone was most of a twenty-second wait.
		hello:        "",
		addWavHeader: false,
		// Barge-in works properly here: Daily's client cancels echo before the
		// audio ever reaches us, so the caller's voice is the caller's voice.
		allowInterruptions: true,
		// Matches the phone path. 60ms was chosen to save WebSocket writes and
		// it bought nothing worth having: every frame is 60ms of audio the
		// caller waits for before the first word, on a path where the sidecar
		// and Daily each add their own buffering on top.
		audioOutFrameMS: 40,
	})
	log.Printf("room: session=%s media ended", sessionID)

	// The sidecar leaves on its own when the room empties, but a media session
	// that ended for any other reason — the pipeline stopping, the agent
	// hanging up — would otherwise leave it sitting in the room, billing.
	stopSidecar(sessionID)
	if p.platform != nil {
		reportSessionFailedOrEnded(p.platform)
	}
	endLiveRoom(p.platform)
	releaseCall(sessionID)
	calls.del(sessionID)
}

// reportSessionFailedOrEnded emits the end-of-call report for a browser call.
//
// Browser calls have no carrier lifecycle, so nothing else will ever report
// them: there is no call.hangup to hang the reporter off, and a session the
// platform never hears about is marked failed half an hour later with no cause.
func reportSessionFailedOrEnded(rc *rexaCall) {
	if rexaPoster == nil || rc == nil {
		return
	}
	// Claim it the same way the phone path does, so a media session that ends
	// twice cannot send two reports.
	if rc.reported {
		return
	}
	rc.reported = true

	ended := time.Now()
	report := rexa.EndOfCallReport{
		SessionID:  rc.sessionID,
		TenantID:   rc.tenantID,
		CallStatus: rexa.CallStatusCompleted,
		EndReason:  rexa.EndReasonCallerHungUp,
		EndedAt:    rexa.ISOTime(ended),
	}
	if !rc.startedAt.IsZero() {
		report.StartedAt = rexa.ISOTime(rc.startedAt)
		report.DurationSeconds = int(ended.Sub(rc.startedAt).Seconds())
	}
	if rc.transcript != nil {
		report.Messages = rc.transcript.Turns()
	}
	log.Printf("rexa: reporting browser session=%s turns=%d", rc.sessionID, len(report.Messages))
	go func() {
		if err := rexaPoster.PostEndOfCall(context.Background(), rc.webhookURL, report); err != nil {
			log.Printf("rexa: browser end-of-call report FAILED for session=%s: %v", rc.sessionID, err)
		}
	}()
}
