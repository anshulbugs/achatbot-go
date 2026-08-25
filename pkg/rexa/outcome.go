package rexa

// Outcome is everything we know about how a call ended. It converts the
// carrier's and the detector's vocabularies into the two fields the platform
// stores.
//
// This deliberately takes raw signals rather than a pre-digested verdict, so
// the mapping lives in one tested place instead of being scattered across the
// webhook handler, the AMD branch and the media teardown — the three places
// that each learn a different part of how a call ended.
type Outcome struct {
	// AMDVerdict is the answering-machine result, empty when detection was
	// disabled or never resolved. Both the standard vocabulary (human,
	// machine, not_sure) and premium's (human_residence, human_business,
	// machine, silence, fax_detected, not_sure) are understood.
	AMDVerdict string
	// HangupCause is Telnyx's cause on call.hangup, e.g. normal_clearing,
	// user_busy, timeout, call_rejected. Empty when the call never got that
	// far (a dial that failed outright).
	HangupCause string
	// Direction is "inbound" or "outbound"; anything else is treated as
	// outbound, which is the only direction the platform dispatches today.
	Direction string
	// Answered is true once the callee picked up. It is the dividing line
	// between "we never reached anyone" and "a conversation happened".
	Answered bool
	// AgentEnded is true when we hung up deliberately — the max-call-duration
	// cap, or finishing a voicemail message.
	AgentEnded bool
	// DispatchFailed is true when the call could not be placed at all
	// (credentials rejected, carrier unreachable).
	DispatchFailed bool
	// VoicemailDetected is true when something OTHER than the carrier's AMD
	// concluded a machine answered -- today, the transcript guard reading a
	// recorded greeting off the caller's own words.
	//
	// It has to be a separate signal because such a call carries the verdict
	// the carrier actually returned, which is precisely the mislabel the guard
	// exists to correct: human_business. Deriving voicemail from AMDVerdict
	// alone reported those calls as ordinary completed conversations while our
	// own counter called them voicemail.
	VoicemailDetected bool
}

// Report returns the call_status and end_reason for this outcome.
//
// call_status is a closed enum on the platform side: an unrecognised value
// fails validation and the ENTIRE report is dropped, taking the transcript
// with it. So every branch here must yield one of the five CallStatus
// constants — there is no safe "unknown".
func (o Outcome) Report() (callStatus, endReason string) {
	switch {
	// Never placed. Distinct from no_answer: nobody's phone ever rang.
	case o.DispatchFailed:
		return CallStatusFailed, EndReasonProviderFail

	// A machine answered — either the carrier said so, or the transcript guard
	// did. Ordered ABOVE the answered/AgentEnded branches on purpose: a call
	// the guard acted on was answered and was ended by us, so leaving it any
	// lower reports it as an ordinary completed conversation.
	case IsMachineAMD(o.AMDVerdict) || o.VoicemailDetected:
		return CallStatusVoicemail, EndReasonVoicemail

	// Never answered. The cause distinguishes busy from ringing out; both are
	// ordinary campaign outcomes rather than failures, and the platform bills
	// and reports them differently.
	case !o.Answered:
		switch o.HangupCause {
		case "user_busy", "busy":
			return CallStatusBusy, EndReasonBusy
		case "no_answer", "timeout", "originator_cancel", "no_user_response":
			return CallStatusNoAnswer, EndReasonNoAnswer
		case "call_rejected", "rejected":
			// Rejected is a deliberate decline, which reads as busy to a
			// campaign far better than it reads as a failure.
			return CallStatusBusy, EndReasonBusy
		case "":
			return CallStatusNoAnswer, EndReasonNoAnswer
		default:
			return CallStatusFailed, EndReasonError
		}

	// Answered, and we ended it.
	case o.AgentEnded:
		return CallStatusCompleted, EndReasonAgentHungUp
	}

	// Answered and the far end ended it. Attribution is direction-aware: on an
	// outbound call the human is the callee we dialled; on an inbound call the
	// human is the caller who dialled us. The platform re-derives this from
	// the session direction anyway, so getting it wrong is not fatal — but it
	// costs nothing to be right.
	if o.Direction == "inbound" {
		return CallStatusCompleted, EndReasonCallerHungUp
	}
	return CallStatusCompleted, EndReasonCalleeHungUp
}

// IsMachineAMD reports whether an AMD verdict means no human is listening.
//
// THIS IS THE ONE PREDICATE. The dialler used to carry its own copy, and the
// two drifted: the dialler dropped `silence` from the machine set — for good
// reasons, below — and the reporter kept it. Roughly eleven calls per campaign
// run therefore held a full conversation with a person and were filed as
// voicemail. Exported so `isMachineVerdict` in the dialler delegates here
// rather than restating it; routing and reporting must not be able to disagree
// about what answered the phone.
//
// `not_sure` is human. Telnyx documents it that way, and recording a real
// conversation as a voicemail is worse than the reverse.
//
// `silence` is human, and it used to be a machine. It means detection heard
// nothing to judge, which is most often a person who picked up and waited —
// exactly what people do when a call opens with a pause. Treating it as a
// machine is also the unrecoverable direction: if a silent answer really was a
// mailbox, `call.machine.greeting.ended` follows, that event is only ever
// emitted for a machine, and it overwrites the verdict with `machine`, so the
// call still reports as voicemail. Nothing rescues a person filed as one.
func IsMachineAMD(verdict string) bool {
	switch verdict {
	case "machine", "fax_detected":
		return true
	default:
		return false
	}
}
