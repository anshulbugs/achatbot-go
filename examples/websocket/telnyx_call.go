package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"

	"achatbot/pkg/consts"
	"achatbot/pkg/modules/speech/tts"
	"achatbot/pkg/params"
	achatbot_processors "achatbot/pkg/processors"
	"achatbot/pkg/rexa"
	"achatbot/pkg/sentiment"
	"achatbot/pkg/telnyx"
)

// callParams holds everything a single outbound call needs, set when the call
// is placed and looked up when Telnyx events and the media stream arrive.
type callParams struct {
	To           string `json:"to"`
	Hello        string `json:"hello"`
	SystemPrompt string `json:"system_prompt"`
	PromptSuffix string `json:"prompt_suffix"`
	// VoicemailMessage is spoken after the beep when this callee's line
	// answers as a machine. Per call so a campaign can address each person by
	// name; falls back to the server default when empty.
	VoicemailMessage string `json:"voicemail_message"`
	// stopMedia ends the media session for this call, releasing its GPU pool
	// slots. Set by the media handler, called by answering-machine detection.
	stopMedia func()
	// amdCh carries the answering-machine verdict (human/machine/not_sure) and
	// beepCh the greeting-ended cue. Detection happens on the webhook goroutine
	// while the audio is driven from the media handler, so they meet here.
	amdCh    chan string
	beepCh   chan string
	VoiceID  int     `json:"voice"`
	Speed    float32 `json:"speed"`
	Volume   float32 `json:"volume"`
	LLMModel string  `json:"llm"`
	Demo     bool    `json:"demo"`   // play a curated set of voices, one after another
	Voices   []int   `json:"voices"` // explicit voice ids to demo (overrides the default set)

	// TransferNumber is where a "put me through to a human" request goes.
	// Empty means transfer is unavailable, and the call_transfer tool is not
	// registered at all — a model that cannot see the tool cannot promise a
	// transfer it will not get.
	TransferNumber string `json:"transfer_number"`
	// DisplayName is the contact's name, presented alongside the caller ID on
	// a transfer so the receiving human knows who is being put through even if
	// the number is filtered.
	DisplayName string `json:"display_name"`

	// ─── Platform-contract state ────────────────────────────────────
	//
	// Everything below is set only for calls dispatched by the platform
	// (pkg/rexa) and stays zero for the local demo server's /api/call. The
	// demo path is chosen by `rexa == nil`, so a demo call takes exactly the
	// code path it did before the contract existed.

	// rexa carries the platform's per-call context: session/tenant ids, the
	// callback URL, and the client built from that dispatch's own credentials.
	// nil on demo calls.
	platform *rexaCall
}

// rexaCall is the platform-dispatched half of a call's state.
//
// It is a separate struct rather than more fields on callParams so the
// boundary stays obvious: if you are reading p.platform, you are on the contract
// path, and if it is nil you are not.
type rexaCall struct {
	sessionID  string
	tenantID   string
	webhookURL string
	direction  string // "outbound" | "inbound"

	// client is built from THIS dispatch's telecom credentials. The platform
	// is multi-tenant and hands over per-call BYO credentials, so using the
	// process-global client here would place one tenant's call on another
	// tenant's carrier account.
	client *telnyx.Client

	// transcript accumulates every turn. It cannot be reconstructed from the
	// pipeline's ChatHistory, which trims to a rolling window mid-call.
	transcript *rexa.Transcript

	// sentimentWebhook is a DIFFERENT url from webhookURL, and empty unless the
	// tenant opted in. Mid-call alerts go there; the end-of-call report does
	// not.
	sentimentWebhook string
	sentiment        sentiment.Tracker

	// live publishes call state to the tenant's own Redis. nil when the
	// dispatch carried no Redis details, and every method is nil-safe.
	live *rexa.LivePublisher

	// Live-listening room. Empty unless the dispatch carried Redis details:
	// that is the platform's signal that something is watching this call, and
	// a room per call regardless would be a large Daily bill for a feature
	// almost nobody opens.
	roomName string
	roomSIP  string
	joinURL  string
	// bridged guards against dialling a second SIP leg into the room if the
	// answered event ever arrives twice.
	bridged bool

	// startedAt anchors both the report's duration and the transcript's turn
	// timings. Set when the call is answered, not when it was dispatched.
	startedAt time.Time

	// answered, amdVerdict and hangupCause accumulate the signals the outcome
	// mapper needs. They are written from the webhook goroutine and read when
	// the report is built, so they go through the registry's mutex.
	answered    bool
	amdVerdict  string
	hangupCause string
	agentEnded  bool

	// reported guards the report against double-emission. Both call.hangup and
	// a dispatch failure can reach the reporter, and the platform dedupes on
	// session_id anyway, but sending twice wastes a retry ladder and muddies
	// the logs.
	reported bool
}

// demoVoiceSet is the curated shortlist of the best-sounding Kokoro English
// voices to audition on a demo call.
var demoVoiceSet = []int{2, 6, 9, 16, 14, 18, 21, 26} // Bella, Nicole, Sarah, Michael, Fenrir, Puck, Emma, George

// callRegistry maps a Telnyx call_control_id to its params for the life of a call.
type callRegistry struct {
	mu sync.Mutex
	m  map[string]*callParams
	// ended keeps finished calls reachable briefly, for carrier events that
	// arrive after the hangup. See remember.
	ended map[string]endedCall
}

func newCallRegistry() *callRegistry { return &callRegistry{m: map[string]*callParams{}} }

// tc returns the Telnyx client that owns this call.
//
// Platform-dispatched calls carry their own client, built from the tenant
// credentials on that dispatch. Everything else — the demo server's /api/call,
// inbound legs the demo answers — falls back to the process-global client
// built from TELNYX_* environment variables, which is exactly the client those
// paths used before the contract existed.
//
// Returns nil only when there is no client at all (telephony unconfigured),
// which callers already handle.
func (p *callParams) tc() *telnyx.Client {
	if p != nil && p.platform != nil && p.platform.client != nil {
		return p.platform.client
	}
	return telnyxClient
}

// markAnswered records the pickup time, which anchors both the report duration
// and the transcript's turn timings.
func (r *callRegistry) markAnswered(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[id]; p != nil && p.platform != nil && !p.platform.answered {
		p.platform.answered = true
		p.platform.startedAt = at
	}
}

// recordAMD stores the answering-machine verdict for the end-of-call report.
// Distinct from signalAMD, which wakes the media handler: the verdict is
// needed by BOTH, and a call that never started a pipeline still has to
// report the machine it reached.
func (r *callRegistry) recordAMD(id, verdict string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[id]; p != nil && p.platform != nil {
		p.platform.amdVerdict = verdict
		if isMachineVerdict(verdict) {
			// A watcher should see a voicemail drop as its own event, not as a
			// call that went quiet: no pipeline ever runs, so nothing else on
			// this path would report anything.
			p.platform.live.Event(rexa.EventMachineDetected, nil)
		}
	}
}

// recordHangup stores the carrier's hangup cause, which distinguishes busy
// from rang-out from failed.
func (r *callRegistry) recordHangup(id, cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[id]; p != nil && p.platform != nil {
		p.platform.hangupCause = cause
	}
}

// markAgentEnded notes that we hung up deliberately rather than the far end.
func (r *callRegistry) markAgentEnded(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[id]; p != nil && p.platform != nil {
		p.platform.agentEnded = true
	}
}

// platformOf returns the call's contract state without claiming it, for events
// that are not once-per-call. nil for demo calls.
//
// Falls back to the recently-ended set, because some carrier events arrive
// AFTER the hangup that removed the call. call.recording.saved is the one that
// matters: it landed one second after call.hangup on a real call, found
// nothing, and the recording was silently never reported.
func (r *callRegistry) platformOf(id string) *rexaCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[id]; p != nil {
		return p.platform
	}
	if e, ok := r.ended[id]; ok {
		return e.rc
	}
	return nil
}

// endedTTL is how long a finished call stays reachable for late carrier events.
//
// Telnyx finalises a recording tens of seconds after the call ends, and the
// event can arrive either side of call.hangup depending on how the call
// finished. Five minutes covers the observed lag with room to spare while
// keeping the map small — it holds three strings per entry, not a call.
const endedTTL = 5 * time.Minute

type endedCall struct {
	rc *rexaCall
	at time.Time
}

// remember keeps a finished call's contract context for late events, and drops
// anything past the TTL while it is here. Sweeping on write means no timer and
// no goroutine for a map that only grows when calls end.
func (r *callRegistry) remember(id string, rc *rexaCall) {
	if rc == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended == nil {
		r.ended = map[string]endedCall{}
	}
	cutoff := time.Now().Add(-endedTTL)
	for k, e := range r.ended {
		if e.at.Before(cutoff) {
			delete(r.ended, k)
		}
	}
	r.ended[id] = endedCall{rc: rc, at: time.Now()}
}

// claimReport returns the call's contract state exactly once, so the report is
// emitted a single time however many paths race to send it. Returns nil for
// demo calls and for a second caller.
func (r *callRegistry) claimReport(id string) *rexaCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.m[id]
	if p == nil || p.platform == nil || p.platform.reported {
		return nil
	}
	p.platform.reported = true
	return p.platform
}

func (r *callRegistry) put(id string, p *callParams) {
	r.mu.Lock()
	r.m[id] = p
	r.mu.Unlock()
}
func (r *callRegistry) get(id string) *callParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}
func (r *callRegistry) del(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

// setStopMedia records how to tear down a call's media session. Answering-machine
// detection arrives on the webhook goroutine, but the pipeline lives in the media
// handler, so the webhook needs a handle to end it.
func (r *callRegistry) setStopMedia(id string, stop func()) {
	r.mu.Lock()
	if p := r.m[id]; p != nil {
		p.stopMedia = stop
	}
	r.mu.Unlock()
}

// signalAMD delivers the answering-machine verdict to the media handler. The
// channel is buffered and the send is non-blocking, so a duplicate or late
// webhook can never wedge the webhook goroutine.
func (r *callRegistry) signalAMD(id, result string) {
	r.mu.Lock()
	ch := (chan string)(nil)
	if p := r.m[id]; p != nil {
		ch = p.amdCh
	}
	r.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
	}
}

// signalBeep delivers the greeting-ended cue to a voicemail call.
func (r *callRegistry) signalBeep(id, result string) {
	r.mu.Lock()
	ch := (chan string)(nil)
	if p := r.m[id]; p != nil {
		ch = p.beepCh
	}
	r.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
	}
}

// stopMediaFor ends a call's media session exactly once and reports whether it
// did. Closing the socket unblocks the pipeline task, which lets runVoiceSession's
// deferred Put calls hand the VAD/ASR/TTS slots back.
func (r *callRegistry) stopMediaFor(id string) bool {
	r.mu.Lock()
	p := r.m[id]
	var stop func()
	if p != nil && p.stopMedia != nil {
		stop, p.stopMedia = p.stopMedia, nil
	}
	r.mu.Unlock()
	if stop == nil {
		return false
	}
	stop()
	return true
}

var (
	telnyxClient *telnyx.Client
	calls        = newCallRegistry()
)

// handleCall places an outbound call. It expects a JSON body with at least
// "to"; hello/system_prompt/voice/speed/llm are optional and default to the
// server config.
func handleCall(w http.ResponseWriter, r *http.Request) {
	writeCORS(w)
	if r.Method == http.MethodOptions {
		return
	}
	if telnyxClient == nil {
		http.Error(w, "telephony not configured (set TELNYX_API_KEY)", http.StatusServiceUnavailable)
		return
	}
	if telnyxClient.PublicURL() == "" {
		http.Error(w, "TELNYX_PUBLIC_URL not set — the server needs its public URL to receive webhooks", http.StatusServiceUnavailable)
		return
	}

	var p callParams
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	p.To = strings.TrimSpace(p.To)
	if !strings.HasPrefix(p.To, "+") || len(p.To) < 8 {
		http.Error(w, "\"to\" must be an E.164 number like +15551234567", http.StatusBadRequest)
		return
	}
	if p.Hello == "" {
		p.Hello = "Hello! This is your voice assistant. How can I help you today?"
	}
	// Prefer appending per-caller text to the shared base: RadixAttention caches
	// shared prefixes, so a suffix keeps the fleet-wide prompt cached while a full
	// replacement does not. Measured 31.5 vs 10.8 req/s at 60 concurrent.
	p.SystemPrompt = resolvePrompt(cfg.Server.SystemPrompt, p.SystemPrompt, p.PromptSuffix)
	if !isValidVoiceID(p.VoiceID) {
		p.VoiceID = cfg.TTS.SpeakerID
	}
	if p.Speed <= 0.2 || p.Speed > 3 {
		p.Speed = cfg.TTS.Speed
	}
	if p.Volume <= 0.2 || p.Volume > 3 {
		p.Volume = cfg.TTS.Gain
	}
	if p.LLMModel == "" {
		p.LLMModel = cfg.LLM.Model
	}

	webhookURL := telnyxClient.PublicURL() + "/telnyx/webhook"
	if p.VoicemailMessage == "" {
		p.VoicemailMessage = cfg.Server.VoicemailMessage
	}
	// Render both announcements before the line is even ringing. Doing it here
	// rather than mid-call means the greeting is ready the instant the callee
	// answers, and a call that turns out to be a machine never waits on -- or
	// pays for -- a TTS slot. Cached by text, so a campaign renders each
	// distinct wording once however many numbers share it.
	prerenderAnnouncements(&p)

	callControlID, err := telnyxClient.Dial(r.Context(), p.To, webhookURL, "", amdModeFor(&p))
	if err != nil {
		log.Printf("telnyx dial err: %v", err)
		http.Error(w, "dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	p.amdCh, p.beepCh = make(chan string, 2), make(chan string, 2)
	calls.put(callControlID, &p)
	// "source=demo" is load-bearing. A platform dispatch logs
	// "rexa: session=... dialing" instead, and telling the two apart after the
	// fact is the first question asked when a call behaves unexpectedly — a
	// demo call carries no session, so the contract machinery correctly does
	// nothing for it and there is no recording or report to look for.
	log.Printf("telnyx: dialing %s call_control_id=%s source=demo(/api/call)", p.To, callControlID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"call_control_id": callControlID, "status": "dialing"})
}

// handleTelnyxWebhook receives Call Control events for our outbound calls.
func handleTelnyxWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	w.WriteHeader(http.StatusOK) // always ack promptly
	// Platform-dispatched calls carry their own client, so events for them
	// must be processed even when the process-global client is absent
	// (TELNYX_API_KEY unset). Bail out only when neither path is active.
	if telnyxClient == nil && rexaPoster == nil {
		return
	}
	ev, err := telnyx.ParseWebhook(body)
	if err != nil {
		log.Printf("telnyx webhook parse err: %v", err)
		return
	}
	id := ev.Data.Payload.CallControlID
	// The hangup cause is the first thing anyone asks for when a call dies
	// early, and logging it here beats reconstructing it from the end-of-call
	// report. The SIP code is the only useful signal on a SIP URI dial.
	if cause := ev.Data.Payload.HangupCause; cause != "" {
		log.Printf("telnyx event: %s call=%s cause=%s sip=%s", ev.Data.EventType, id,
			cause, ev.Data.Payload.SIPHangupCause)
	} else {
		log.Printf("telnyx event: %s call=%s", ev.Data.EventType, id)
	}

	switch ev.Data.EventType {
	case "call.initiated":
		// Inbound leg (someone dialed one of our numbers). Register it with the
		// server defaults and answer it, so the agent picks up. Outbound legs are
		// already registered by handleCall and are ignored here.
		if ev.Data.Payload.Direction != "incoming" || calls.get(id) != nil {
			return
		}
		// Auto-answering with server defaults is DEMO behaviour and requires
		// the process-global client. Under the platform contract the platform
		// decides which inbound calls we answer, and tells us via /incoming
		// with the tenant's own credentials — so an unregistered inbound leg
		// here is not ours to pick up. (Without this guard a contract-only
		// deployment, which has no global client, would also nil-panic below.)
		if telnyxClient == nil {
			log.Printf("telnyx: ignoring unregistered inbound call=%s (no demo client configured)", id)
			return
		}
		p := &callParams{
			To:           ev.Data.Payload.From,
			Hello:        cfg.Server.InboundHello,
			SystemPrompt: cfg.Server.SystemPrompt,
			VoiceID:      cfg.TTS.SpeakerID,
			Speed:        cfg.TTS.Speed,
			Volume:       cfg.TTS.Gain,
			LLMModel:     cfg.LLM.Model,
		}
		if p.Hello == "" {
			p.Hello = "Hello! Thanks for calling. How can I help you today?"
		}
		p.amdCh, p.beepCh = make(chan string, 2), make(chan string, 2)
		calls.put(id, p)
		log.Printf("telnyx: inbound call from %s call_control_id=%s", ev.Data.Payload.From, id)
		go func() {
			if err := p.tc().Answer(context.Background(), id); err != nil {
				log.Printf("telnyx answer err: %v", err)
				calls.del(id)
			}
		}()

	case "call.answered":
		p := calls.get(id)
		if p == nil {
			return
		}
		calls.markAnswered(id, time.Now())
		// Ring time = dial to answer, for the reservation-cost estimate.
		markAnswered(id)
		if cfg.Server.RecordCalls {
			go func() {
				if err := p.tc().RecordStart(context.Background(), id); err != nil {
					log.Printf("telnyx record_start err: %v", err)
				} else {
					log.Printf("telnyx: recording started call=%s", id)
				}
			}()
		}
		// Safety net: two agents talking to each other never hang up on their own,
		// so bound every call. 0 disables.
		if secs := cfg.Server.MaxCallSecs; secs > 0 {
			go func() {
				time.Sleep(time.Duration(secs) * time.Second)
				if calls.get(id) != nil {
					log.Printf("telnyx: max duration %ds reached, hanging up call=%s", secs, id)
					// We are ending this, not the callee — the report must say so.
					calls.markAgentEnded(id)
					if err := p.tc().Hangup(context.Background(), id); err != nil {
						log.Printf("telnyx hangup err: %v", err)
					}
				}
			}()
		}
		// Fork the call's audio to our media bridge (wss on the public tunnel).
		streamURL := wsURL(p.tc().PublicURL()) + "/telnyx/media?cc=" + id
		go func() {
			if err := p.tc().StreamingStart(context.Background(), id, streamURL); err != nil {
				log.Printf("telnyx streaming_start err: %v", err)
			}
		}()
	case "call.machine.detection.ended", "call.machine.premium.detection.ended":
		// human / not_sure keep the pipeline: Telnyx documents not_sure as
		// "treat as human", and hanging up on a real person is far worse than
		// spending a pipeline slot on a machine.
		res := ev.Data.Payload.Result
		log.Printf("telnyx amd verdict=%s call=%s", res, id)
		// Record before signalling. The media handler may tear the call down
		// the instant it reads the verdict, and the report needs it too — a
		// voicemail is precisely the case where no pipeline ever runs.
		calls.recordAMD(id, res)
		calls.signalAMD(id, res)
	case "call.machine.greeting.ended", "call.machine.premium.greeting.ended":
		// beep_detected is the real cue; "ended" (greeting_end mode) and
		// "not_sure" (30s beep timeout) both mean it is safe to start talking.
		calls.signalBeep(id, ev.Data.Payload.Result)
	case "call.recording.saved":
		// Emitted separately from the end-of-call report, and deliberately
		// after it: the carrier finalises a recording tens of seconds after the
		// call ends, and holding the report back for it would delay every
		// disposition the platform acts on.
		reportRecordingSaved(id, body)
	case "call.hangup":
		// The report is emitted from HERE, on the carrier's lifecycle, rather
		// than from pipeline teardown. Voicemail, no-answer and busy calls all
		// need a report and none of them ever start a pipeline, so hanging the
		// reporter off the session would silently drop exactly the outcomes
		// the platform most needs to hear about.
		calls.recordHangup(id, ev.Data.Payload.HangupCause)
		// Keep the contract context reachable before the entry goes: the
		// recording event arrives after this and would otherwise find nothing.
		if rc := calls.platformOf(id); rc != nil {
			calls.remember(id, rc)
		}
		reportCallEnded(id)
		// Release capacity here, on the carrier's lifecycle, for the same
		// reason the report is emitted here: a no-answer or a busy never
		// reaches a pipeline, so nothing downstream would ever give the slot
		// back.
		releaseCall(id)
		calls.del(id)
	}
}

// reportCallEnded posts the end-of-call report for a platform-dispatched call.
// No-op for demo calls, and for any call already reported.
//
// Runs in the background: the retry ladder can take a quarter of an hour, and
// the Telnyx webhook goroutine must not be held open for it.
func reportCallEnded(id string) {
	rc := calls.claimReport(id)
	if rc == nil {
		return // demo call, or already reported
	}
	if rexaPoster == nil {
		log.Printf("rexa: no poster configured; dropping report for session=%s", rc.sessionID)
		return
	}

	ended := time.Now()
	status, reason := rexa.Outcome{
		AMDVerdict:  rc.amdVerdict,
		HangupCause: rc.hangupCause,
		Direction:   rc.direction,
		Answered:    rc.answered,
		AgentEnded:  rc.agentEnded,
	}.Report()

	report := rexa.EndOfCallReport{
		SessionID:  rc.sessionID,
		TenantID:   rc.tenantID,
		CallStatus: status,
		EndReason:  reason,
		EndedAt:    rexa.ISOTime(ended),
		CCID:       id,
	}
	// Timestamps and duration only make sense once the call was answered; on a
	// no-answer there is no conversation to have lasted any length of time,
	// and inventing a zero duration would misreport it as a completed call of
	// no length.
	if rc.answered && !rc.startedAt.IsZero() {
		report.StartedAt = rexa.ISOTime(rc.startedAt)
		report.DurationSeconds = int(ended.Sub(rc.startedAt).Seconds())
	}
	if rc.transcript != nil {
		report.Messages = rc.transcript.Turns()
	}
	// Terminal event here rather than on pipeline teardown, for the same reason
	// the report is emitted here: a no-answer or a busy never reaches a
	// pipeline, and the consumer's tailer waits on this event to stop reading.
	// end_reason "voicemail" is load-bearing on their side -- it routes the call
	// into the voicemail bucket instead of counting it as completed.
	if status == "failed" {
		rc.live.Failed(reason)
	} else {
		rc.live.Ended(reason)
	}
	endLiveRoom(rc)

	log.Printf("rexa: reporting session=%s status=%s reason=%s turns=%d",
		rc.sessionID, status, reason, len(report.Messages))
	go func() {
		if err := rexaPoster.PostEndOfCall(context.Background(), rc.webhookURL, report); err != nil {
			log.Printf("rexa: end-of-call report FAILED for session=%s: %v", rc.sessionID, err)
		}
	}()
}

// reportRecordingSaved forwards Telnyx's recording payload to the platform,
// with only the session and tenant ids added.
//
// A PASS-THROUGH, deliberately. Re-mapping the carrier's fields into our own
// struct means every field Telnyx adds is dropped until someone notices, and
// every field it renames breaks quietly. The platform's schema is a loose
// passthrough for the same reason, so the useful thing we can do is not get in
// the way.
//
// In particular the URLs are forwarded exactly as received — including Telnyx's
// `s3://` form. We do not rewrite them, re-host them, or infer a status from
// whether they are present.
//
// Read from the registry rather than claimed: this is not once-per-call state.
// It can also arrive after call.hangup has deleted the entry, in which case
// there is nobody to report to and we drop it.
func reportRecordingSaved(id string, body []byte) {
	rc := calls.platformOf(id)
	if rc == nil || rexaPoster == nil {
		return
	}
	var env struct {
		Data struct {
			Payload map[string]any `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data.Payload == nil {
		log.Printf("rexa: recording payload unreadable for session=%s: %v", rc.sessionID, err)
		return
	}

	evt := rexa.NewRecordingSaved(env.Data.Payload, rc.sessionID, rc.tenantID)
	log.Printf("rexa: recording saved session=%s recording=%v",
		rc.sessionID, env.Data.Payload["recording_id"])
	go func() {
		if err := rexaPoster.PostRecordingSaved(context.Background(), rc.webhookURL, evt); err != nil {
			log.Printf("rexa: recording report FAILED for session=%s: %v", rc.sessionID, err)
		}
	}()
}

// wsURL converts an http(s) base URL to its ws(s) equivalent.
func wsURL(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

var telnyxUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// handleTelnyxMedia is the media bridge: Telnyx forks call audio to this
// WebSocket (base64 µ-law/8 kHz). It runs the full voice pipeline over a
// Telnyx serializer, so the caller talks to the same VAD->ASR->LLM->TTS agent
// as the browser client.
// isMachineVerdict reports whether an AMD result means "no human is listening".
//
// The standard modes (detect, detect_beep, detect_words, greeting_end) only ever
// answer human/machine/not_sure, but `premium` widens the vocabulary to
// human_residence, human_business, machine, silence, fax_detected and not_sure.
// Testing `== "machine"` therefore silently routes a silent voicemail or a fax
// tone down the human path and burns a full pipeline on it for the whole call.
//
// not_sure stays on the human path deliberately: Telnyx documents it as
// "treat as human", and hanging up on a real person is far worse than spending
// a pipeline slot on a machine.
func isMachineVerdict(result string) bool {
	switch result {
	case "machine", "silence", "fax_detected":
		return true
	default:
		return false
	}
}

// amdMode returns the answering-machine-detection mode to request.
//
// Plain "detect" only reports human/machine -- it never emits the greeting
// event that carries beep_detected. Leaving a message needs that cue, so a
// configured message upgrades the mode rather than silently waiting for a beep
// that never arrives and holding the line to the call cap.
func amdModeFor(p *callParams) string {
	m := cfg.Server.VoicemailDetection
	if m == "" {
		m = "disabled"
	}
	if p != nil && p.VoicemailMessage != "" && m == "detect" {
		return "detect_beep"
	}
	return m
}

// prerenderAnnouncements synthesizes this call's greeting and voicemail
// message up front so neither costs a TTS slot once the call is live. Both
// are cached by text/voice/speed, so unique-per-callee wording is supported at
// the cost of one render each, while shared wording renders once per campaign.
func prerenderAnnouncements(p *callParams) {
	// AMD off means no voicemail path, so on a PHONE call there is nothing to
	// pre-render — the greeting is spoken by the pipeline as its first turn.
	// A browser call has no AMD at all and still wants its greeting ready
	// before the caller arrives, which is what the second condition covers.
	if amdModeFor(p) == "disabled" && p.platform == nil {
		return
	}
	for _, text := range []string{p.Hello, p.VoicemailMessage} {
		if text != "" {
			announcements.get(text, p.VoiceID, p.Speed, p.Volume)
		}
	}
}

// announceCache holds pre-synthesized PCM for the fixed announcements: the
// greeting and the voicemail message.
//
// These are the same words on every call of a campaign, so synthesizing them
// per call burns a TTS pool slot to produce audio we already have. Rendering
// once and replaying the bytes means a voicemail call never touches the GPU at
// all, and a human call only pays for the conversation itself.
type announceCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

var announcements = &announceCache{m: map[string][]byte{}}

// get returns PCM for text in the given voice, synthesizing on first use. The
// key includes voice and speed because the same words in a different voice are
// different audio.
// get returns PCM for text, synthesizing on first use.
//
// gain is part of the key AND applied to the provider. Leaving it out was a
// real bug: the greeting was rendered at whatever gain the pooled instance
// happened to carry from the last session, so it played quieter than every
// other word on the call while the configured gain was applied faithfully
// everywhere else.
func (a *announceCache) get(text string, voiceID int, speed, gain float32) []byte {
	if text == "" {
		return nil
	}
	key := fmt.Sprintf("%d|%.2f|%.2f|%s", voiceID, speed, gain, text)
	a.mu.Lock()
	pcm, ok := a.m[key]
	a.mu.Unlock()
	if ok {
		return pcm
	}
	info, err := ttsPool.Get()
	if err != nil {
		log.Printf("announce: tts pool err: %v", err)
		return nil
	}
	defer ttsPool.Put(info)
	prov := info.GetInstance().(tts.VoiceProvider)
	prov.SetVoice(voiceID, speed)
	if gain > 0 {
		prov.SetGain(gain)
	}
	pcm = prov.Synthesize(text)
	a.mu.Lock()
	a.m[key] = pcm
	a.mu.Unlock()
	log.Printf("announce: cached %d bytes for %q (voice=%d speed=%.2f)", len(pcm), truncate(text, 40), voiceID, speed)
	return pcm
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// playAnnouncement streams cached PCM over the media socket, keeping a lead of
// buffered audio ahead of real time, and stops early if stop closes.
//
// Pacing at exactly real time does not work: sending 100ms of audio every 100ms
// leaves Telnyx no cushion, so the first scheduling hiccup on our side arrives
// late, its buffer underruns and the caller hears a hole in the middle of a
// word. That is what "...learn a little about you [gap] anything you're curious
// about" was -- the audio was all sent, just not early enough.
//
// Blasting the whole message instead would buffer fine but makes stopping it
// meaningless, since Telnyx would already hold the rest. So we run a lead:
// send until we are announceLead ahead of the wall clock, then only top up.
// That absorbs jitter while keeping at most announceLead of audio committed,
// which is how quickly a machine verdict can still cut the greeting.
// Both too little AND too much lead produce the same symptom, which is what
// made this hard to pin down.
//
// Too little: sending 100ms of audio every 100ms leaves Telnyx no cushion, one
// scheduling hiccup arrives late, its buffer underruns and the caller hears a
// hole in the middle of a word.
//
// Too much: Telnyx buffers a bounded amount of inbound media. Pushed 3 seconds
// ahead it dropped the excess, and playback resumed only when our sender caught
// up — a pause in the middle of the greeting that then continued mid-word. Our
// own sender never fell behind on those calls (nothing logged), which is what
// ruled out the underrun explanation and pointed here.
//
// 600ms absorbs an ordinary stall while staying well inside what the carrier
// will hold. It also bounds how much greeting is already committed when a
// machine verdict arrives, which is the other thing this number controls.
const announceLead = 600 * time.Millisecond

func playAnnouncement(tw *achatbot_processors.WebsocketTransportWriter, pcm []byte, rate int, stop <-chan struct{}) bool {
	const chunkMS = 100
	chunk := rate * 2 * chunkMS / 1000
	if chunk <= 0 || len(pcm) == 0 {
		return true
	}
	started := time.Now()
	sent := 0
	fellBehind := false
	for off := 0; off < len(pcm); off += chunk {
		select {
		case <-stop:
			log.Printf("announce: stopped after %d/%d bytes in %v (interrupted)", sent, len(pcm), time.Since(started).Round(time.Millisecond))
			return false
		default:
		}
		end := off + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		// Telephony sets AudioOutAddWavHeader false, so this is byte-identical to
		// SendPayload today. Routed through SendAudioFrame anyway so the header
		// decision stays with the transport, and enabling headers here later
		// cannot silently produce undecodable audio.
		if err := tw.SendAudioFrame(frames.NewAudioRawFrame(pcm[off:end], rate, 1, 2)); err != nil {
			log.Printf("announce: send failed after %d/%d bytes in %v: %v", sent, len(pcm), time.Since(started).Round(time.Millisecond), err)
			return false
		}
		sent = end

		// Wait only while we are further ahead than the lead allows.
		audioSent := time.Duration(sent/2) * time.Second / time.Duration(rate)
		ahead := audioSent - time.Since(started)
		// Falling behind real time is the gap the caller hears, and it is
		// otherwise invisible — the audio all arrives, just too late. Logged
		// once per announcement so the next report of "it broke up" can be
		// confirmed or ruled out from the log instead of guessed at.
		if ahead < 0 && !fellBehind {
			fellBehind = true
			log.Printf("announce: FELL BEHIND real time by %v after %d/%d bytes — "+
				"the caller will hear a gap here", -ahead.Round(time.Millisecond), sent, len(pcm))
		}
		if ahead > announceLead {
			select {
			case <-stop:
				log.Printf("announce: stopped after %d/%d bytes in %v (interrupted)", sent, len(pcm), time.Since(started).Round(time.Millisecond))
				return false
			case <-time.After(ahead - announceLead):
			}
		}
	}
	log.Printf("announce: completed %d bytes (%.2fs audio) in %v", len(pcm), float64(len(pcm))/2/float64(rate), time.Since(started).Round(time.Millisecond))
	return true
}

// runVoicemailCall handles a call that answered as a machine, without ever
// acquiring a pipeline slot: stop the greeting, wait for the beep, play the
// pre-rendered message, hang up.
func runVoicemailCall(id string, tw *achatbot_processors.WebsocketTransportWriter, ser *telnyx.Serializer,
	rate int, p *callParams, beep <-chan string) {

	// Release this call's GPU capacity: the greeting was cut, the pipeline
	// never started, and the message plays from the announcement cache, so it
	// holds no pool slots. It stays counted against the TOTAL in-flight
	// ceiling, because a voicemail still costs a carrier channel, a media
	// stream and the CPU to play audio down it.
	markVoicemail(id)

	// Flush whatever of the greeting Telnyx still has buffered, so the message
	// does not trail the interrupted hello.
	if b, err := ser.Serialize(&frames.StartInterruptionFrame{}); err == nil && len(b) > 0 {
		_ = tw.SendPayload(frames.NewAudioRawFrame(nil, rate, 1, 2)) // no-op keeps the writer warm
	}

	msg := p.VoicemailMessage
	if msg == "" {
		log.Printf("telnyx amd: no voicemail message configured, hanging up call=%s", id)
		_ = p.tc().Hangup(context.Background(), id)
		return
	}
	pcm := announcements.get(msg, p.VoiceID, p.Speed, p.Volume)
	if len(pcm) == 0 {
		_ = p.tc().Hangup(context.Background(), id)
		return
	}

	// Wait for the beep. Telnyx times its own beep detection out at 30s and
	// reports not_sure, so this bound only covers the event never arriving.
	select {
	case res := <-beep:
		log.Printf("telnyx amd: beep signal (%s) call=%s", res, id)
	case <-time.After(35 * time.Second):
		log.Printf("telnyx amd: no beep event within 35s, speaking anyway call=%s", id)
	}

	log.Printf("telnyx amd: playing voicemail message call=%s (%d bytes, no gpu)", id, len(pcm))
	playAnnouncement(tw, pcm, rate, make(chan struct{}))
	// Let the tail drain out of Telnyx's buffer before tearing the call down.
	time.Sleep(time.Duration(len(pcm)/2/rate)*time.Second/4 + 700*time.Millisecond)
	if err := p.tc().Hangup(context.Background(), id); err != nil {
		log.Printf("telnyx amd hangup err call=%s: %v", id, err)
	}
}

func handleTelnyxMedia(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("cc")
	p := calls.get(id)
	if p == nil {
		http.Error(w, "unknown call", http.StatusNotFound)
		return
	}
	ws, err := telnyxUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("telnyx media upgrade err: %v", err)
		return
	}
	defer ws.Close()
	log.Printf("telnyx media stream connected call=%s", id)
	// Give answering-machine detection a way to end this session early.
	var closeOnce sync.Once
	calls.setStopMedia(id, func() { closeOnce.Do(func() { ws.Close() }) })

	ser := telnyx.NewSerializer(consts.DefaultRate)
	ser.SetClarity(cfg.Server.ClarityFilter)
	ser.SetLatencyHook(func(d time.Duration) {
		log.Printf("telnyx response latency ~%dms call=%s", d.Milliseconds(), id)
	})
	// Voice-demo call: audition either the caller-supplied voice ids or the
	// curated shortlist. Suppress the idle re-prompt so it can't cut in.
	var demoVoices []int
	idlePrompt := cfg.Server.IdlePromptText
	idleSecs := cfg.Server.IdlePromptSecs
	if len(p.Voices) > 0 {
		demoVoices = p.Voices
	} else if p.Demo {
		demoVoices = demoVoiceSet
	}
	if len(demoVoices) > 0 {
		idlePrompt = ""
		idleSecs = 0
	}

	conn := telnyx.NewConn(ws)

	// Greeting first, from cache, before any pool slot is taken. Every call in a
	// campaign says the same words, so synthesizing per call would burn a TTS
	// slot to produce audio we already have -- and a call that turns out to be a
	// machine would have paid for a pipeline it never used.
	helloText := p.Hello
	amdOn := amdModeFor(p) != "disabled" && len(demoVoices) == 0
	if amdOn && helloText != "" {
		pcm := announcements.get(helloText, p.VoiceID, p.Speed, p.Volume)
		if len(pcm) > 0 {
			tw := achatbot_processors.NewWebsocketTransportWriter(conn, &params.WebsocketServerParams{
				AudioCameraParams: params.NewAudioCameraParams(),
				Serializer:        ser,
			})
			stop := make(chan struct{})
			var stopOnce sync.Once
			verdict := ""
			go func() {
				select {
				case verdict = <-p.amdCh:
					if isMachineVerdict(verdict) {
						stopOnce.Do(func() { close(stop) }) // cut the greeting mid-word
					}
				case <-stop:
				}
			}()
			ttsRate, _, _ := ttsSampleInfo()
			announceStart := time.Now()
			log.Printf("announce: playing greeting call=%s (%d bytes @%dHz)", id, len(pcm), ttsRate)
			finished := playAnnouncement(tw, pcm, ttsRate, stop)
			stopOnce.Do(func() { close(stop) })

			// Do NOT block waiting for a verdict here. Detection resolves within
			// a few seconds of answer, so on any greeting of normal length it has
			// already arrived; waiting again at the end just adds dead air to
			// every human call, and adds it unconditionally when detection is not
			// firing at all. Silence by now means treat the callee as human and
			// get the pipeline running immediately.
			select {
			case verdict = <-p.amdCh:
			default:
			}
			if isMachineVerdict(verdict) {
				log.Printf("telnyx amd: machine verdict=%q on call=%s (greeting cut=%t) -- no pipeline will be used", verdict, id, !finished)
				runVoicemailCall(id, tw, ser, ttsRate, p, p.beepCh)
				log.Printf("telnyx media stream ended call=%s (voicemail, 0 pool slots)", id)
				return
			}
			// Telnyx is still holding whatever we sent ahead of real time.
			// Suppress "clear" for that long: the pipeline starts now and its
			// first interruption frame would otherwise flush the unplayed tail
			// of the greeting mid-sentence.
			if finished {
				spoken := time.Duration(len(pcm)/2) * time.Second / time.Duration(ttsRate)
				if outstanding := spoken - time.Since(announceStart); outstanding > 0 {
					ser.HoldInterrupts(outstanding)
					log.Printf("announce: %s of greeting still buffered at Telnyx; holding interrupts",
						outstanding.Round(time.Millisecond))
				}
			}
			log.Printf("announce: greeting done call=%s (finished=%t verdict=%q) -> starting pipeline", id, finished, verdict)
			// Human: the greeting has already played, so the session must not
			// repeat it.
			helloText = ""
		}
	}

	// Platform calls record a transcript; demo calls do not.
	var chatObserver func(map[string]any)
	var agentObserver func(string)
	if p.platform != nil && p.platform.transcript != nil {
		// The greeting was spoken straight from TTS and never reached the
		// model, so nothing else will ever put it in the transcript.
		p.platform.transcript.SeedGreeting(p.Hello)
		chatObserver = p.platform.transcript.ObserveChatHistory()
		agentObserver = p.platform.transcript.ObserveAgentTurns()
	}
	// Sentiment rides the same observer: it needs exactly what the transcript
	// needs — every turn as it happens — and adding a second observation point
	// in the pipeline would be a second thing to keep in sync.
	if obs := sentimentObserver(id, p.platform); obs != nil {
		chatObserver = chainObservers(chatObserver, obs)
	}
	if p.platform != nil {
		// The AI now has the call. This is what moves the consumer out of
		// "dialing" and into a live conversation.
		p.platform.live.Event(rexa.EventHumanDetected, nil)
	}

	runVoiceSession(conn, ser, sessionConfig{
		clientID: "telnyx_" + id,
		// The greeting was played from the announcement cache above, so the
		// model must be told it happened or it greets the caller a second time.
		spokenGreeting:     p.Hello,
		callID:             id,
		call:               p,
		chatObserver:       chatObserver,
		agentTurnObserver:  agentObserver,
		systemPrompt:       p.SystemPrompt,
		voiceID:            p.VoiceID,
		speed:              p.Speed,
		volume:             p.Volume,
		llmModel:           p.LLMModel,
		addWavHeader:       false,
		hello:              helloText,
		demoVoices:         demoVoices,
		allowInterruptions: true, // adaptive echo gate lets real barge-in through
		idlePrompt:         idlePrompt,
		idleSecs:           idleSecs,
		// Small frames so the caller hears the first synthesized clause
		// ~160ms sooner than 200ms batching; Telnyx repaces to 20ms RTP.
		audioOutFrameMS: 40,
	})
	log.Printf("telnyx media stream ended call=%s", id)
}
