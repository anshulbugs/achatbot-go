package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"achatbot/pkg/telnyx"
)

// callParams holds everything a single outbound call needs, set when the call
// is placed and looked up when Telnyx events and the media stream arrive.
type callParams struct {
	To           string  `json:"to"`
	Hello        string  `json:"hello"`
	SystemPrompt string  `json:"system_prompt"`
	VoiceID      int     `json:"voice"`
	Speed        float32 `json:"speed"`
	LLMModel     string  `json:"llm"`
}

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
	if p.SystemPrompt == "" {
		p.SystemPrompt = cfg.Server.SystemPrompt
	}
	if !isValidVoiceID(p.VoiceID) {
		p.VoiceID = cfg.TTS.SpeakerID
	}
	if p.Speed <= 0.2 || p.Speed > 3 {
		p.Speed = cfg.TTS.Speed
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
	case "call.answered":
		p := calls.get(id)
		if p == nil {
			return
		}
		// Milestone A: prove the loop with Telnyx's built-in TTS. The media
		// bridge (real pipeline audio) replaces this next.
		go func() {
			ctx := context.Background()
			if err := telnyxClient.Speak(ctx, id, p.Hello, ""); err != nil {
				log.Printf("telnyx speak err: %v", err)
			}
		}()
	case "call.speak.ended":
		go func() {
			if err := telnyxClient.Hangup(context.Background(), id); err != nil {
				log.Printf("telnyx hangup err: %v", err)
			}
		}()
	case "call.hangup":
		calls.del(id)
	}
}
