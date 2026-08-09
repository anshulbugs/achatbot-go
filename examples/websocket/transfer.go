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
			"description": "Transfer this call to a human. Call this ONLY when the " +
				"person explicitly asks to speak to a human, a real person, an agent, " +
				"or a manager. Tell them you are connecting them BEFORE calling this. " +
				"Do not call it to answer a question you could answer yourself.",
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
		rc.live.Status(rexa.LiveStatusTransferred)
	}
	// Release the pipeline: the carrier owns the conversation now and our
	// media fork is not part of that bridge, so holding a GPU slot for it
	// would consume capacity nobody is using.
	markVoicemail(t.callID)
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
