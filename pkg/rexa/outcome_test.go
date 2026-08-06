package rexa

import "testing"

func TestOutcomeReport(t *testing.T) {
	cases := []struct {
		name       string
		out        Outcome
		wantStatus string
		wantReason string
	}{
		// The whole point of the AMD work: a machine-answered call still
		// reports, even though no pipeline ever ran.
		{"premium machine", Outcome{AMDVerdict: "machine", Answered: true},
			CallStatusVoicemail, EndReasonVoicemail},
		{"premium silence", Outcome{AMDVerdict: "silence", Answered: true},
			CallStatusVoicemail, EndReasonVoicemail},
		{"premium fax", Outcome{AMDVerdict: "fax_detected", Answered: true},
			CallStatusVoicemail, EndReasonVoicemail},

		// Premium's human variants must NOT be mistaken for machines.
		{"human_residence", Outcome{AMDVerdict: "human_residence", Answered: true},
			CallStatusCompleted, EndReasonCalleeHungUp},
		{"human_business", Outcome{AMDVerdict: "human_business", Answered: true},
			CallStatusCompleted, EndReasonCalleeHungUp},
		{"not_sure is human", Outcome{AMDVerdict: "not_sure", Answered: true},
			CallStatusCompleted, EndReasonCalleeHungUp},

		// Unanswered outcomes are ordinary campaign results, not failures.
		{"busy", Outcome{HangupCause: "user_busy"}, CallStatusBusy, EndReasonBusy},
		{"rejected reads as busy", Outcome{HangupCause: "call_rejected"},
			CallStatusBusy, EndReasonBusy},
		{"rang out", Outcome{HangupCause: "timeout"}, CallStatusNoAnswer, EndReasonNoAnswer},
		{"no answer", Outcome{HangupCause: "no_answer"}, CallStatusNoAnswer, EndReasonNoAnswer},
		{"unknown cause, unanswered", Outcome{HangupCause: "weird_cause"},
			CallStatusFailed, EndReasonError},
		{"no cause at all", Outcome{}, CallStatusNoAnswer, EndReasonNoAnswer},

		// Direction-aware hangup attribution.
		{"outbound human hangup", Outcome{Answered: true, Direction: "outbound",
			HangupCause: "normal_clearing"}, CallStatusCompleted, EndReasonCalleeHungUp},
		{"inbound human hangup", Outcome{Answered: true, Direction: "inbound",
			HangupCause: "normal_clearing"}, CallStatusCompleted, EndReasonCallerHungUp},

		{"agent ended", Outcome{Answered: true, AgentEnded: true, Direction: "outbound"},
			CallStatusCompleted, EndReasonAgentHungUp},

		// Never placed at all — distinct from nobody answering.
		{"dispatch failed", Outcome{DispatchFailed: true},
			CallStatusFailed, EndReasonProviderFail},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, reason := c.out.Report()
			if status != c.wantStatus || reason != c.wantReason {
				t.Errorf("= (%q, %q), want (%q, %q)", status, reason, c.wantStatus, c.wantReason)
			}
		})
	}
}

// call_status is a closed enum on the platform: an unrecognised value fails
// validation and the whole report is dropped, transcript included. No input
// combination may produce anything outside the five valid values.
func TestReportAlwaysYieldsAValidCallStatus(t *testing.T) {
	valid := map[string]bool{
		CallStatusCompleted: true, CallStatusFailed: true, CallStatusNoAnswer: true,
		CallStatusVoicemail: true, CallStatusBusy: true,
	}
	verdicts := []string{"", "machine", "human", "human_residence", "silence",
		"fax_detected", "not_sure", "garbage"}
	causes := []string{"", "normal_clearing", "user_busy", "timeout", "call_rejected", "nonsense"}
	dirs := []string{"", "inbound", "outbound", "sideways"}

	for _, v := range verdicts {
		for _, c := range causes {
			for _, d := range dirs {
				for _, answered := range []bool{true, false} {
					for _, agentEnded := range []bool{true, false} {
						o := Outcome{AMDVerdict: v, HangupCause: c, Direction: d,
							Answered: answered, AgentEnded: agentEnded}
						status, reason := o.Report()
						if !valid[status] {
							t.Fatalf("%+v produced invalid call_status %q", o, status)
						}
						if reason == "" {
							t.Fatalf("%+v produced an empty end_reason", o)
						}
					}
				}
			}
		}
	}
}

// A machine verdict must win over an agent-initiated hangup: we always hang up
// after leaving a voicemail, and reporting that as agent_hung_up would hide
// every voicemail in the campaign.
func TestVoicemailBeatsAgentHangup(t *testing.T) {
	o := Outcome{AMDVerdict: "machine", Answered: true, AgentEnded: true}
	status, reason := o.Report()
	if status != CallStatusVoicemail || reason != EndReasonVoicemail {
		t.Errorf("= (%q, %q), want voicemail", status, reason)
	}
}

// A dispatch that never left the ground must not be reported as a voicemail
// even if a stale verdict is somehow attached.
func TestDispatchFailureBeatsEverything(t *testing.T) {
	o := Outcome{DispatchFailed: true, AMDVerdict: "machine", Answered: true}
	if status, _ := o.Report(); status != CallStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}
