package rexa

import (
	"strings"
	"testing"
)

// Verbatim from the campaign running on 25 Aug 2026, 22:57 onwards. Indented
// headers, and the action as a verb -- the form the first version of this code
// did not recognise, which is why it rewrote nothing on 27 live prompts.
const verbForm = "    ### Step 2: Hang Up\r\n    Action: hangUp\r\n\r\n" +
	"    ### Step 13: Send SMS [SMS Candidate]\r\n    Action: sendSms\r\n\r\n" +
	"    ### Step 12: Transfer Call\r\n    Action: transferCall\r\n"

// Verbatim from the campaign of session 01a03a9b, which writes steps as prose.
const proseForm = `### Step 19: Send SMS [Recruiter]
Say: "I'm sending you a text message with the details right now." and then send the SMS.

### Step 20: Send SMS [Candidate SMS]
Send the SMS silently.

### Step 26: Send Email [Email template]
Send the email.

### Step 27: Hang Up
Say: "Thank you for your time. Have a great day! Goodbye."
`

// THE REGRESSION THIS FILE EXISTS FOR. end_call was advertised on 20 live calls
// and invoked on none, because every Hang Up step went unrewritten: the headers
// were indented and the rule was anchored to column zero.
func TestRewriteHandlesTheIndentedVerbForm(t *testing.T) {
	out := RewriteUnsupportedActions(verbForm, true)

	if strings.Contains(out, "Action: hangUp") {
		t.Error("hangUp left as-is; the model is told to hang up with no way to do it")
	}
	if !strings.Contains(out, "Invoke the end_call tool now") {
		t.Error("the hangUp step was not pointed at end_call")
	}
	if strings.Contains(out, "Action: sendSms") {
		t.Error("sendSms left as-is; it has no tool and the step cannot complete")
	}
	if !strings.Contains(out, "Invoke the call_transfer tool now") {
		t.Error("transferCall was not pointed at the tool that performs it")
	}
	// Indentation is structure in these prompts; losing it reflows the step.
	if !strings.Contains(out, "    Invoke the end_call tool now") {
		t.Error("leading indentation was not preserved")
	}
}

// The other campaign's wording must keep working.
func TestRewriteStillHandlesTheProseForm(t *testing.T) {
	out := RewriteUnsupportedActions(proseForm, true)
	if strings.Contains(out, "sending you a text message with the details right now") {
		t.Error("the present-tense promise survived; it is what the model repeats")
	}
	if strings.Contains(out, "and then send the SMS") {
		t.Error("an action with no tool behind it survived")
	}
	for _, want := range []string{
		"as soon as we finish this call",
		"The text is sent after the call ends",
		"The email is sent after the call ends",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing replacement %q", want)
		}
	}
}

// The guidance must be appended even when no rule fires. Prompts are authored
// per campaign, so an exact-match rule list is always one format behind -- and
// when it matched nothing, the model was told nothing.
func TestGuidanceIsAppendedEvenWhenNothingMatched(t *testing.T) {
	unknown := "### Step 9: Wrap Up\r\nAction: doSomethingWeHaveNeverSeen\r\n"
	out := RewriteUnsupportedActions(unknown, true)
	if !strings.Contains(out, "you MUST invoke the end_call tool") {
		t.Error("an unrecognised prompt got no guidance at all -- the original bug")
	}
	if !strings.HasPrefix(out, unknown) {
		t.Error("the prompt body was altered; only an append was expected")
	}
}

// A browser session has no carrier leg and no end_call, and must not be told to
// invoke a tool it cannot see.
func TestBrowserSessionsAreNotToldToInvokeEndCall(t *testing.T) {
	out := RewriteUnsupportedActions(verbForm, false)
	if strings.Contains(out, "invoke the end_call tool") {
		t.Error("a session without the tool was told to use it")
	}
	if !strings.Contains(out, "You cannot hang up") {
		t.Error("no fallback instruction for a session that cannot hang up")
	}
	if !strings.Contains(out, "Ask the caller to hang up") {
		t.Error("the hangUp step was not given a usable alternative")
	}
}

// Both line endings appear in real prompts; a rule anchored with [ \t]*$ alone
// silently fails on CRLF.
func TestRewriteHandlesBothLineEndings(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		in := "  Action: hangUp" + nl
		if out := RewriteUnsupportedActions(in, true); strings.Contains(out, "Action: hangUp") {
			t.Errorf("hangUp survived with %q line ending", nl)
		}
	}
}

func TestRewriteLeavesAnEmptyPromptAlone(t *testing.T) {
	if out := RewriteUnsupportedActions("", true); out != "" {
		t.Error("empty prompt must stay empty")
	}
}
