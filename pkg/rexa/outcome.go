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

	// A machine answered. This is the case with no pipeline behind it — the
	// report is emitted from the carrier lifecycle, because there is no
	// session teardown to hang it off.
	case isMachineAMD(o.AMDVerdict):
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

// isMachineAMD reports whether an AMD verdict means no human is listening.
//
// Mirrors the dialler's own predicate. `not_sure` is treated as human because
// Telnyx documents it that way, and recording a real conversation as a
// voicemail is worse than the reverse.
func isMachineAMD(verdict string) bool {
	switch verdict {
	case "machine", "silence", "fax_detected":
		return true
	default:
		return false
	}
}
