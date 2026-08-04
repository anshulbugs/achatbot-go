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
	"achatbot/pkg/telnyx"
)

// callParams holds everything a single outbound call needs, set when the call
// is placed and looked up when Telnyx events and the media stream arrive.
type callParams struct {
	To           string `json:"to"`
	Hello        string `json:"hello"`
	SystemPrompt string `json:"system_prompt"`
	PromptSuffix string `json:"prompt_suffix"`
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
}

// demoVoiceSet is the curated shortlist of the best-sounding Kokoro English
// voices to audition on a demo call.
var demoVoiceSet = []int{2, 6, 9, 16, 14, 18, 21, 26} // Bella, Nicole, Sarah, Michael, Fenrir, Puck, Emma, George

// callRegistry maps a Telnyx call_control_id to its params for the life of a call.
type callRegistry struct {
	mu sync.Mutex
	m  map[string]*callParams
}

func newCallRegistry() *callRegistry { return &callRegistry{m: map[string]*callParams{}} }

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
	callControlID, err := telnyxClient.Dial(r.Context(), p.To, webhookURL, "", amdMode())
	if err != nil {
		log.Printf("telnyx dial err: %v", err)
		http.Error(w, "dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	p.amdCh, p.beepCh = make(chan string, 2), make(chan string, 2)
	calls.put(callControlID, &p)
	log.Printf("telnyx: dialing %s call_control_id=%s", p.To, callControlID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"call_control_id": callControlID, "status": "dialing"})
}

// handleTelnyxWebhook receives Call Control events for our outbound calls.
func handleTelnyxWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	w.WriteHeader(http.StatusOK) // always ack promptly
	if telnyxClient == nil {
		return
	}
	ev, err := telnyx.ParseWebhook(body)
	if err != nil {
		log.Printf("telnyx webhook parse err: %v", err)
		return
	}
	id := ev.Data.Payload.CallControlID
	log.Printf("telnyx event: %s call=%s", ev.Data.EventType, id)

	switch ev.Data.EventType {
	case "call.initiated":
		// Inbound leg (someone dialed one of our numbers). Register it with the
		// server defaults and answer it, so the agent picks up. Outbound legs are
		// already registered by handleCall and are ignored here.
		if ev.Data.Payload.Direction != "incoming" || calls.get(id) != nil {
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
			if err := telnyxClient.Answer(context.Background(), id); err != nil {
				log.Printf("telnyx answer err: %v", err)
				calls.del(id)
			}
		}()

	case "call.answered":
		if calls.get(id) == nil {
			return
		}
		if cfg.Server.RecordCalls {
			go func() {
				if err := telnyxClient.RecordStart(context.Background(), id); err != nil {
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
					if err := telnyxClient.Hangup(context.Background(), id); err != nil {
						log.Printf("telnyx hangup err: %v", err)
					}
				}
			}()
		}
		// Fork the call's audio to our media bridge (wss on the public tunnel).
		streamURL := wsURL(telnyxClient.PublicURL()) + "/telnyx/media?cc=" + id
		go func() {
			if err := telnyxClient.StreamingStart(context.Background(), id, streamURL); err != nil {
				log.Printf("telnyx streaming_start err: %v", err)
			}
		}()
	case "call.machine.detection.ended", "call.machine.premium.detection.ended":
		// human / not_sure keep the pipeline: Telnyx documents not_sure as
		// "treat as human", and hanging up on a real person is far worse than
		// spending a pipeline slot on a machine.
		res := ev.Data.Payload.Result
		log.Printf("telnyx amd verdict=%s call=%s", res, id)
		calls.signalAMD(id, res)
	case "call.machine.greeting.ended", "call.machine.premium.greeting.ended":
		// beep_detected is the real cue; "ended" (greeting_end mode) and
		// "not_sure" (30s beep timeout) both mean it is safe to start talking.
		calls.signalBeep(id, ev.Data.Payload.Result)
	case "call.hangup":
		calls.del(id)
	}
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
// amdMode returns the answering-machine-detection mode to request.
//
// Plain "detect" only reports human/machine -- it never emits the greeting
// event that carries beep_detected. Leaving a message needs that cue, so a
// configured message upgrades the mode rather than silently waiting for a beep
// that never arrives and holding the line to the call cap.
func amdMode() string {
	m := cfg.Server.VoicemailDetection
	if m == "" {
		m = "disabled"
	}
	if cfg.Server.VoicemailMessage != "" && m == "detect" {
		return "detect_beep"
	}
	return m
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
func (a *announceCache) get(text string, voiceID int, speed float32) []byte {
	if text == "" {
		return nil
	}
	key := fmt.Sprintf("%d|%.2f|%s", voiceID, speed, text)
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

// playAnnouncement streams cached PCM over the media socket at roughly real
// time, stopping early if stop closes. Pacing matters: blasting the whole
// message means Telnyx buffers it, and "stop the greeting" then only takes
// effect after a clear, losing the immediacy the caller hears.
func playAnnouncement(tw *achatbot_processors.WebsocketTransportWriter, pcm []byte, rate int, stop <-chan struct{}) bool {
	const chunkMS = 100
	chunk := rate * 2 * chunkMS / 1000
	if chunk <= 0 || len(pcm) == 0 {
		return true
	}
	tick := time.NewTicker(chunkMS * time.Millisecond)
	defer tick.Stop()
	for off := 0; off < len(pcm); off += chunk {
		select {
		case <-stop:
			return false // interrupted (machine detected)
		default:
		}
		end := off + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := tw.SendPayload(frames.NewAudioRawFrame(pcm[off:end], rate, 1, 2)); err != nil {
			return false
		}
		select {
		case <-stop:
			return false
		case <-tick.C:
		}
	}
	return true
}

// runVoicemailCall handles a call that answered as a machine, without ever
// acquiring a pipeline slot: stop the greeting, wait for the beep, play the
// pre-rendered message, hang up.
func runVoicemailCall(id string, tw *achatbot_processors.WebsocketTransportWriter, ser *telnyx.Serializer,
	rate int, p *callParams, beep <-chan string) {

	// Flush whatever of the greeting Telnyx still has buffered, so the message
	// does not trail the interrupted hello.
	if b, err := ser.Serialize(&frames.StartInterruptionFrame{}); err == nil && len(b) > 0 {
		_ = tw.SendPayload(frames.NewAudioRawFrame(nil, rate, 1, 2)) // no-op keeps the writer warm
	}

	msg := cfg.Server.VoicemailMessage
	if msg == "" {
		log.Printf("telnyx amd: no voicemail message configured, hanging up call=%s", id)
		_ = telnyxClient.Hangup(context.Background(), id)
		return
	}
	pcm := announcements.get(msg, p.VoiceID, p.Speed)
	if len(pcm) == 0 {
		_ = telnyxClient.Hangup(context.Background(), id)
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
	if err := telnyxClient.Hangup(context.Background(), id); err != nil {
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
	amdOn := amdMode() != "disabled" && len(demoVoices) == 0
	if amdOn && helloText != "" {
		pcm := announcements.get(helloText, p.VoiceID, p.Speed)
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
					if verdict == "machine" {
						stopOnce.Do(func() { close(stop) }) // cut the greeting mid-word
					}
				case <-stop:
				}
			}()
			ttsRate, _, _ := ttsSampleInfo()
			finished := playAnnouncement(tw, pcm, ttsRate, stop)
			stopOnce.Do(func() { close(stop) })

			if verdict == "" {
				// Detection may still be in flight; give it the rest of its
				// window rather than starting a pipeline we are about to drop.
				select {
				case verdict = <-p.amdCh:
				case <-time.After(3 * time.Second):
				}
			}
			if verdict == "machine" {
				log.Printf("telnyx amd: machine on call=%s (greeting cut=%t) -- no pipeline will be used", id, !finished)
				runVoicemailCall(id, tw, ser, ttsRate, p, p.beepCh)
				log.Printf("telnyx media stream ended call=%s (voicemail, 0 pool slots)", id)
				return
			}
			// Human: the greeting has already played, so the session must not
			// repeat it.
			helloText = ""
		}
	}

	runVoiceSession(conn, ser, sessionConfig{
		clientID:           "telnyx_" + id,
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
