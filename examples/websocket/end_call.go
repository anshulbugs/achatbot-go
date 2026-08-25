package main

// Ending the call.
//
// The model asks to end the call with the `end_call` tool; we let it finish its
// closing line, then hang up at the carrier.
//
// WHY THIS EXISTS. Campaign prompts are step machines, and their flows end in
// "Hang Up" steps -- six of them in the prompt this was written against. There
// was no way to hang up, so those steps could not complete: nothing recorded
// them as done, and the model re-entered the step every time the caller spoke.
// One real call carried SEVEN copies of "Thank you for your time. Have a great
// day! Goodbye." in its history, and ran five minutes past the first one.
//
// Bound per session, for the same reason as call_transfer: a tool invocation
// carries only its arguments, so a process-wide handler could not tell which of
// the calls in flight it was meant to end.

import (
	"context"
	"log"
	"sync"
	"time"

	"achatbot/pkg/common"
	"achatbot/pkg/telnyx"
)

const (
	// endCallGrace is how long after the agent's audio finishes playing we wait
	// before hanging up. The carrier is still draining what we sent, and
	// cutting the leg early truncates the goodbye -- the exact failure that
	// makes a call sound broken to the person on it.
	endCallGrace = 900 * time.Millisecond
	// endCallMaxWait bounds the wait for the closing line. If speech never
	// stops we still hang up: a tool the model invoked and that then does
	// nothing is the defect this file exists to remove.
	endCallMaxWait = 25 * time.Second
	// endCallSettle is how long the bot must be quiet before we treat the
	// closing line as finished, so a gap between sentences is not mistaken for
	// the end of the turn.
	endCallSettle = 400 * time.Millisecond
)

// endCallTool is the per-call `end_call` implementation.
type endCallTool struct {
	callID string
	client *telnyx.Client
	// ser reports when the agent's audio has actually finished playing.
	ser *telnyx.Serializer

	mu   sync.Mutex
	done bool
}

// GetToolCall returns the schema advertised to the model.
//
// THE DESCRIPTION IS LOAD-BEARING, and the wording is taken from what went
// wrong with call_transfer: gemma said "I'm just getting you connected now" and
// stopped, having treated the announcement as the action. A goodbye is far more
// likely to be treated that way, because saying goodbye genuinely IS how a
// person ends a call. So the description says plainly that the sentence is not
// the hangup.
func (t *endCallTool) GetToolCall() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "end_call",
			"description": "Hang up this phone call. INVOKE THIS TOOL whenever the " +
				"conversation is finished: you have reached a Hang Up step, the person " +
				"has said goodbye, they have asked you to stop calling, or there is " +
				"nothing further to discuss. Say your closing line and invoke this tool " +
				"in the SAME turn -- saying goodbye does NOT end the call, only " +
				"invoking this tool does, and a call that is not hung up stays open " +
				"with the person still on the line. Never say goodbye without invoking " +
				"it. Do not invoke it while the person is still asking things, and " +
				"never invoke it twice.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{
						"type": "string",
						"description": "Why the call is ending, in a few words " +
							"(for example: candidate not interested, do not call, " +
							"screening complete, caller said goodbye).",
					},
				},
				"required": []string{"reason"},
			},
		},
	}
}

// GetOllamaAPIToolCall returns the same schema in Ollama's shape.
func (t *endCallTool) GetOllamaAPIToolCall() map[string]any { return t.GetToolCall() }

// Execute schedules the hangup and returns immediately.
//
// It does NOT hang up inline. The closing line is still being synthesised and
// played when this runs -- the model emits its text and its tool call in one
// turn -- so hanging up here would cut the goodbye off mid-word. The wait
// happens on its own goroutine so the pipeline is not blocked holding the turn
// open for it.
//
// The string returned goes back to the model as the tool result, and its job is
// to stop the model producing anything further: whatever it says now is spoken
// over a line that is about to close.
func (t *endCallTool) Execute(args map[string]any) (string, error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return "The call is already ending. Say nothing further.", nil
	}
	t.done = true
	t.mu.Unlock()

	reason, _ := args["reason"].(string)
	log.Printf("end_call: call=%s reason=%q -- hanging up once the closing line has played",
		t.callID, reason)

	// Attribute the hangup to us BEFORE it happens. The report is emitted from
	// the carrier's call.hangup webhook, which can arrive before this goroutine
	// finishes, and a call we ended that reports as callee_hung_up is a lie the
	// campaign acts on.
	calls.markAgentEnded(t.callID)

	go t.hangUpWhenQuiet()
	return "The call is being ended now. Say nothing further.", nil
}

// hangUpWhenQuiet waits for the agent's closing line to finish playing, then
// hangs up at the carrier.
func (t *endCallTool) hangUpWhenQuiet() {
	deadline := time.Now().Add(endCallMaxWait)
	var quietSince time.Time
	for time.Now().Before(deadline) {
		now := time.Now()
		// PlaybackEnd is the carrier-playout clock, so this waits for what the
		// CALLER hears rather than for what we have finished sending. They are
		// seconds apart: a whole turn is handed to Telnyx in one message and
		// then played out in real time.
		end := time.Time{}
		if t.ser != nil {
			end = t.ser.PlaybackEnd()
		}
		if now.Before(end) {
			quietSince = time.Time{}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if quietSince.IsZero() {
			quietSince = now
		}
		// Require a settled gap: TTS arrives sentence by sentence, so playback
		// briefly runs dry between them and that is not the end of the turn.
		if now.Sub(quietSince) >= endCallSettle {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(endCallGrace)

	if t.client == nil {
		log.Printf("end_call: call=%s has no carrier client; cannot hang up", t.callID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := t.client.Hangup(ctx, t.callID); err != nil {
		// Worth shouting about: the model has told the caller the call is over
		// and the line is still open.
		log.Printf("end_call: HANGUP FAILED on call=%s, the line is still open: %v", t.callID, err)
		return
	}
	log.Printf("end_call: call=%s hung up", t.callID)
}

// registerEndCallTool binds `end_call` to a session.
//
// Registered only for carrier calls: a browser demo has no leg to hang up, and
// a model that cannot see the tool cannot promise something that will not
// happen -- the same rule call_transfer follows.
func registerEndCallTool(session *common.Session, p *callParams, callID string, ser *telnyx.Serializer) {
	if p == nil || callID == "" || p.tc() == nil {
		return
	}
	session.RegisterFunc("end_call", &endCallTool{callID: callID, client: p.tc(), ser: ser})
	log.Printf("end_call: enabled for call=%s", callID)
}
