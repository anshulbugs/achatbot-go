package main

// Mid-call sentiment: classify each caller turn and tell the platform the
// moment the answer changes, so a human can be alerted while the call is still
// live.
//
// The whole feature is worthless late. That shapes every decision here: the
// classifier runs off the reply path, on a model that is not the conversation
// model, with a short timeout, a short retry ladder, and a hard rule that it
// never delays a caller.

import (
	"context"
	"log"
	"time"

	"achatbot/pkg/rexa"
	"achatbot/pkg/sentiment"
)

// sentimentClassifier is the process-wide classifier, or nil when sentiment is
// not configured. Built once — it holds only an HTTP client.
var sentimentClassifier *sentiment.Classifier

// initSentiment builds the classifier from config. baseURL is an
// OpenAI-compatible endpoint for a SMALL model; pointing it at the
// conversation model would spend the resource that decides fleet capacity.
func initSentiment(baseURL, model string) {
	if baseURL == "" || model == "" {
		return
	}
	sentimentClassifier = sentiment.New(baseURL, model)
	log.Printf("rexa: sentiment classifier enabled model=%s endpoint=%s", model, baseURL)
}

// chainObservers runs two chat observers in order, skipping nils. Returns nil
// when both are nil, so the caller can keep passing nil for demo sessions.
func chainObservers(a, b func(map[string]any)) func(map[string]any) {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(m map[string]any) {
		a(m)
		b(m)
	}
}

// sentimentObserver returns a chat observer that classifies caller turns, or
// nil when this call has not opted in.
//
// Three conditions must all hold: a classifier is configured on this agent, the
// dispatch asked for sentiment analysis, and it supplied somewhere to send the
// result. Missing any one of them means doing nothing at all rather than
// classifying into the void — the classification is the expensive part.
func sentimentObserver(callID string, rc *rexaCall) func(map[string]any) {
	if sentimentClassifier == nil || rc == nil || rc.sentimentWebhook == "" {
		return nil
	}

	// priorAgent is the last thing we said, kept so a bare "no, a person" is
	// read as an answer rather than a fragment. Only ever touched on the
	// pipeline's own observer goroutine, which is single-threaded per call.
	var priorAgent string

	return func(m map[string]any) {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if content == "" {
			return
		}
		if role == "assistant" {
			priorAgent = content
			return
		}
		if role != "user" {
			return
		}

		// Off the reply path. The caller is already waiting on the
		// conversation model; a classifier that ran inline would add its
		// latency to every single turn to catch something that happens on a
		// handful of them.
		agentSaid := priorAgent
		go classifyTurn(callID, rc, agentSaid, content)
	}
}

func classifyTurn(callID string, rc *rexaCall, agentSaid, callerSaid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	verdict := sentimentClassifier.Classify(ctx, agentSaid, callerSaid)
	// Observe under the registry lock: the tracker is per-call state and this
	// runs on a fresh goroutine per turn, so two turns in flight would race.
	if !calls.observeSentiment(callID, verdict) {
		return
	}

	log.Printf("rexa: sentiment session=%s -> %s", rc.sessionID, verdict)
	// Carries the join link too: an operator alerted mid-call should be one
	// click from listening, not one lookup away.
	rc.live.Event("sentiment_detected", map[string]any{
		"sentiment_value": verdict,
		"room_url":        rc.joinURL,
	})

	if rexaPoster == nil {
		return
	}
	evt := rexa.SentimentEvent{
		SessionID:  rc.sessionID,
		TenantID:   rc.tenantID,
		CallStatus: "in_progress",
		CCID:       callID,
		Sentiment:  verdict,
		// The platform's alert template has a {{join_url}} token; filling it
		// is what turns "the caller wants a human" into something actionable.
		DailyRoomURL: liveJoinURLFor(rc),
	}
	if err := rexaPoster.PostSentiment(ctx, rc.sentimentWebhook, evt); err != nil {
		// Logged, never retried further and never surfaced to the call. A
		// missed alert is a worse call; a blocked pipeline is a broken one.
		log.Printf("rexa: sentiment post FAILED session=%s value=%s: %v",
			rc.sessionID, verdict, err)
	}
}

// observeSentiment records a verdict against a call and reports whether it is a
// change worth sending. False for demo calls and for repeats.
func (r *callRegistry) observeSentiment(id, verdict string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.m[id]
	if p == nil || p.platform == nil {
		return false
	}
	return p.platform.sentiment.Observe(verdict)
}
