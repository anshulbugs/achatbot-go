package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"achatbot/pkg/consts"
	"achatbot/pkg/telnyx"
)

// callParams holds everything a single outbound call needs, set when the call
// is placed and looked up when Telnyx events and the media stream arrive.
type callParams struct {
	To           string  `json:"to"`
	Hello        string  `json:"hello"`
	SystemPrompt string  `json:"system_prompt"`
	PromptSuffix string  `json:"prompt_suffix"`
	VoiceID      int     `json:"voice"`
	Speed        float32 `json:"speed"`
	Volume       float32 `json:"volume"`
	LLMModel     string  `json:"llm"`
	Demo         bool    `json:"demo"`   // play a curated set of voices, one after another
	Voices       []int   `json:"voices"` // explicit voice ids to demo (overrides the default set)
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
	callControlID, err := telnyxClient.Dial(r.Context(), p.To, webhookURL, "")
	if err != nil {
		log.Printf("telnyx dial err: %v", err)
		http.Error(w, "dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
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
			if err := telnyxClient.StreamingStart(context.Background(), id, streamURL, cfg.Server.StreamCodec, cfg.Server.StreamSampleRate); err != nil {
				log.Printf("telnyx streaming_start err: %v", err)
			}
		}()
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

	ser := telnyx.NewSerializer(consts.DefaultRate)
	// Match the serializer to whatever encoding we asked Telnyx for; getting
	// these out of step turns every frame into noise.
	ser.SetWireFormat(strings.EqualFold(cfg.Server.StreamCodec, "L16"), cfg.Server.StreamSampleRate)
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

	runVoiceSession(telnyx.NewConn(ws), ser, sessionConfig{
		clientID:           "telnyx_" + id,
		systemPrompt:       p.SystemPrompt,
		voiceID:            p.VoiceID,
		speed:              p.Speed,
		volume:             p.Volume,
		llmModel:           p.LLMModel,
		addWavHeader:       false,
		hello:              p.Hello,
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
