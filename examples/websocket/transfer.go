package main

// Live call transfer.
//
// The model asks for a transfer by calling the `call_transfer` tool; we hand
// the leg to the configured destination via the carrier and step out of the
// conversation.
//
// The tool is bound PER SESSION rather than to the global function registry. A
// tool invocation carries only its arguments — no session, no call id — so a
// process-wide handler could not tell which of the calls in flight it was
// meant to transfer. Binding it per call makes the implementation a closure
// over that call's own state, which is the only version of this that is safe
// with sixty calls running at once.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"achatbot/pkg/common"
	"achatbot/pkg/rexa"
	"achatbot/pkg/telnyx"
)

// transferTimeoutSecs bounds how long the carrier rings the transfer
// destination before giving up. Kept short: the caller is holding the line
// listening to silence for the whole of it.
const transferTimeoutSecs = 25

// transferTool is the per-call `call_transfer` implementation.
//
// Implements common.IFunction so it can be registered on a session.
type transferTool struct {
	callID string
	// to is the destination, from the dispatch's transfer_number.
	to string
	// from is the caller ID presented to the destination — see fromNumberFor.
	from string
	// displayName rides alongside the number so the destination can tell who
	// is being transferred even when the number itself is filtered.
	displayName string
	client      *telnyx.Client

	// platform context for the transfer_initiated callback. Empty on demo
	// calls, which transfer without reporting anywhere.
	sessionID  string
	tenantID   string
	webhookURL string

	// done guards against a model that calls the tool twice: the second
	// transfer would target a leg the carrier has already handed off.
	done bool
}

// GetToolCall returns the OpenAI tool schema advertised to the model.
//
// The description matters more than usual here. The platform appends a note to
// the system prompt telling the model this call "has transfer functionality",
// so the model already believes it can transfer — this is what makes that
// belief true, and what stops it transferring for the wrong reasons.
func (t *transferTool) GetToolCall() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "call_transfer",
			// "Tell them you are connecting them BEFORE calling this" used to be
			// the second sentence, and it is the reason a real caller never got
			// transferred: gemma said "Certainly, I can connect you with X right
			// away", then "I'm just getting you connected now" — and stopped
			// there, having done the part the description asked for first and
			// treated the announcement as the action. The tool was registered,
			// advertised, and never invoked, twice in one call.
			//
			// So the description now says the sentence is not the transfer.
			"description": "Transfer this call to a human. INVOKE THIS TOOL as soon as " +
				"the person asks to speak to a human, a real person, an agent or a " +
				"manager, including when they just say 'transfer me' or agree to be " +
				"put through. Saying that you are connecting or transferring them does " +
				"NOT transfer the call — only invoking this tool does, and a caller " +
				"told they are being connected who then is not has been abandoned on " +
				"the line. Never say you are connecting, transferring, or putting " +
				"someone through unless you invoke this tool in the same turn. Do not " +
				"call it to answer a question you could answer yourself.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{
						"type": "string",
						"description": "Brief reason the caller asked to be transferred, " +
							"in a few words.",
					},
				},
				"required": []string{"reason"},
			},
		},
	}
}

// GetOllamaAPIToolCall returns the same schema in Ollama's shape.
func (t *transferTool) GetOllamaAPIToolCall() map[string]any { return t.GetToolCall() }

// Execute performs the transfer.
//
// The string it returns goes back to the model as the tool result, so it has
// to be something the model can act on: on failure the caller is still on the
// line and needs to be told, and the model is the only thing that can tell
// them.
func (t *transferTool) Execute(args map[string]any) (string, error) {
	if t.done {
		return "The call has already been transferred.", nil
	}
	if t.to == "" {
		// Should not happen: the tool is only registered when a destination
		// exists. Answer in the model's own terms rather than erroring, so it
		// tells the caller instead of stalling.
		return "Transfer is not available on this call. Apologise and continue helping the caller.", nil
	}
	t.done = true

	reason, _ := args["reason"].(string)
	log.Printf("transfer: call=%s to=%s reason=%q", t.callID, t.to, reason)

	// The transfer event is emitted BEFORE the carrier is asked, deliberately:
	// the platform uses it to warn the receiving human that a call is about to
	// arrive, which is only useful ahead of time. The cost is that a transfer
	// that then fails leaves an event claiming one happened — see the failure
	// branch below, which logs loudly so that is at least visible here.
	t.postTransferInitiated()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := t.client.Transfer(ctx, t.callID, t.to, t.from, transferTimeoutSecs); err != nil {
		log.Printf("transfer: call=%s FAILED after transfer_initiated was already sent: %v",
			t.callID, err)
		t.done = false // let the model try again if the caller asks
		return "The transfer could not be connected. Apologise, tell the caller you " +
			"could not reach a human right now, and offer to help them yourself.", nil
	}

	log.Printf("transfer: call=%s handed to %s", t.callID, t.to)
	// Only on success. `transfer_initiated` fires on attempt by design, but a
	// live view showing "transferred" for a call still sitting with the agent
	// would be simply wrong.
	if rc := calls.platformOf(t.callID); rc != nil {
		rc.live.Event(rexa.EventTransferred, map[string]any{"transfer_number": t.to})
	}
	// Release the pipeline: the carrier owns the conversation now and our
	// media fork is not part of that bridge, so holding a GPU slot for it
	// would consume capacity nobody is using.
	markVoicemail(t.callID)

	// LEAVE THE CALL. Accounting for the slot was never enough on its own —
	// the media session stayed open, the pipeline kept transcribing, and the
	// agent carried on answering questions for another thirty seconds after
	// handing the caller over. Telling the model "stop speaking" does not stop
	// it: that is a request, and the next thing the caller says starts another
	// turn regardless.
	//
	// Ending the socket is what actually removes the agent from the
	// conversation. Deferred by a beat so this tool's own result still reaches
	// the model and the turn unwinds through its normal path rather than
	// through a read error.
	go func() {
		time.Sleep(750 * time.Millisecond)
		if calls.stopMediaFor(t.callID) {
			log.Printf("transfer: call=%s handed over — agent has left the call", t.callID)
		}
	}()
	return "Transferred. Stop speaking; the call is being handed over now.", nil
}

// postTransferInitiated notifies the platform. No-op on demo calls.
func (t *transferTool) postTransferInitiated() {
	if rexaPoster == nil || t.webhookURL == "" || t.sessionID == "" {
		return
	}
	ev := rexa.NewTransferInitiated(t.sessionID, t.tenantID, t.to, time.Now())
	go func() {
		if err := rexaPoster.PostTransferInitiated(context.Background(), t.webhookURL, ev); err != nil {
			log.Printf("transfer: transfer_initiated callback FAILED for session=%s: %v",
				t.sessionID, err)
		}
	}()
}

// fromNumberFor picks the caller ID presented to the transfer destination.
//
// Default is the CONTACT's number, so the person receiving the transfer sees
// whose call it is — which is the point of transferring rather than
// cold-dialling them.
//
// The trade-off is real and configurable for that reason. A `from` that is not
// a number on the carrier account gets low or no STIR/SHAKEN attestation, and
// US carriers increasingly mark those "Spam Likely" or drop them outright, so
// this is the setting to suspect first if transfers connect unreliably. Set
// `server.transfer_caller_id: tenant` to present our own number instead;
// `from_display_name` carries the contact's name either way, which survives
// when the number does not.
func fromNumberFor(contactNumber, tenantNumber string) string {
	if cfg.Server.TransferCallerID == "tenant" {
		return tenantNumber
	}
	return contactNumber
}

// registerTransferTool binds `call_transfer` to a session when the call has a
// transfer destination.
//
// Registering nothing when there is no destination is deliberate: a model that
// cannot see the tool cannot promise a transfer it will not get. The platform
// separately tells the prompt whether transfer is available, and the two must
// agree or the caller is misled.
func registerTransferTool(session *common.Session, p *callParams, callID string) {
	if p == nil || p.TransferNumber == "" || p.tc() == nil {
		return
	}
	t := &transferTool{
		callID:      callID,
		to:          p.TransferNumber,
		from:        fromNumberFor(p.To, p.tc().FromNumber()),
		displayName: p.DisplayName,
		client:      p.tc(),
	}
	if p.platform != nil {
		t.sessionID = p.platform.sessionID
		t.tenantID = p.platform.tenantID
		t.webhookURL = p.platform.webhookURL
	}
	session.RegisterFunc("call_transfer", t)
	log.Printf("transfer: enabled for call=%s -> %s (caller id %s)",
		callID, t.to, describeCallerID(t.from, p.To))
}

func describeCallerID(from, contact string) string {
	if from == contact {
		return fmt.Sprintf("contact %s", from)
	}
	return fmt.Sprintf("tenant %s", from)
}

// transferPromises are the ways the agent tells a caller it is handing them
// over. Matched against the agent's own words, lowercased.
//
// Deliberately narrow: each one is a STATEMENT that the handover is happening,
// not an offer to arrange one. "I can connect you if you like" is a question
// and must not move the call.
var transferPromises = []string{
	"transfer you",
	"transferring you",
	"connect you with",
	"connecting you",
	"connect you to",
	"put you through",
	"get you over to",
	"hand you over",
	"getting you connected",
}

// transferDenials are phrasings that CONTAIN a promise phrase but mean the
// opposite. Checked first, because "I can't transfer you" contains
// "transfer you" and acting on it would put a caller through after being told
// they could not be.
var transferDenials = []string{
	"cannot transfer", "can not transfer", "can't transfer",
	"cannot connect", "can't connect",
	"unable to transfer", "unable to connect",
	"not able to transfer", "not able to connect",
	"no one available", "nobody available",
	"do not have transfer", "don't have transfer",
}

// transferOffers are phrasings that ASK whether to transfer rather than state
// that one is happening. Sampling the model's no-tools replies turned up
// "Would you like me to transfer you now, or did you have another question?"
// and "I can certainly connect you with a member of our sales team if you'd
// like. Before I do, ..." — both contain a promise phrase, and transferring on
// either would move a caller in the middle of being asked whether they wanted
// it.
var transferOffers = []string{
	"would you like", "do you want", "shall i", "should i",
	"if you'd like", "if you would like", "if you prefer",
	"before i do", "before i transfer", "before i connect",
	"just to confirm", "is that okay", "is that alright",
}

// promisesTransfer reports whether an agent turn told the caller they are being
// handed to a human.
func promisesTransfer(text string) bool {
	s := strings.ToLower(text)
	for _, d := range transferDenials {
		if strings.Contains(s, d) {
			return false
		}
	}
	for _, o := range transferOffers {
		if strings.Contains(s, o) {
			return false
		}
	}
	for _, p := range transferPromises {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// transferOnPromise returns an agent-turn observer that performs the transfer
// the model said it was performing.
//
// WHY THIS IS NEEDED. gemma-4 will not reliably emit the tool call. Replaying a
// real call's history against the model shows it answering "I completely
// understand that you'd prefer to speak with a person. Let me transfer you
// right now..." with finish_reason=stop and zero tool calls — while the SAME
// request with tool_choice forced returns a correct call_transfer. The model
// decides to transfer and then declines to say so in the one way that does
// anything. Sharpening the tool description was tried first and did not fix it.
//
// So the trigger is the model's own statement to the caller. That is a
// heuristic, and it is the RIGHT heuristic: by the time those words have been
// spoken, the caller has been promised a human, and the only question left is
// whether we keep the promise. Doing nothing is the one option that is
// certainly wrong.
//
// Fires at most once. The tool itself is also idempotent — it reports "already
// transferred" on a second call — but the caller should never see two attempts
// from one promise.
func transferOnPromise(session *common.Session, callID string) func(string) {
	var once sync.Once
	return func(text string) {
		if !promisesTransfer(text) {
			return
		}
		fn := session.Func("call_transfer")
		if fn == nil {
			return
		}
		once.Do(func() {
			log.Printf("transfer: agent promised a handover without calling the tool on call=%s — transferring anyway (%q)",
				callID, firstChars(text, 120))
			result, err := fn.Execute(map[string]any{
				"reason": "the agent told the caller it was connecting them to a person",
			})
			if err != nil {
				log.Printf("transfer: promise repair FAILED on call=%s: %v", callID, err)
				return
			}
			log.Printf("transfer: promise repair on call=%s -> %s", callID, result)
		})
	}
}
