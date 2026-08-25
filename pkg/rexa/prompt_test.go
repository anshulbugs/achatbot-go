package rexa

import "strings"

import "testing"

// The step bodies here are copied verbatim from the campaign prompt of the call
// that exposed this: session 01a03a9b, 25 Aug 2026.
const realSteps = `### Step 19: Send SMS [Recruiter]
Say: "I'm sending you a text message with the details right now." and then send the SMS.

Then continue with -> [Step 20: Send SMS [Candidate SMS]]

### Step 20: Send SMS [Candidate SMS]
Send the SMS silently.

### Step 26: Send Email [Email template]
Send the email.

### Step 27: Hang Up
Say: "Thank you for your time. Have a great day! Goodbye. Please feel free to cut the call if you have no other questions."
`

func TestRewriteRemovesTheSentenceThatLooped(t *testing.T) {
	out := RewriteUnsupportedActions(realSteps)

	// The exact sentence the agent said seven times in a row.
	if strings.Contains(out, "sending you a text message with the details right now") {
		t.Error("the present-tense promise survived; it is what the model repeats")
	}
	if strings.Contains(out, "and then send the SMS") {
		t.Error("an action with no tool behind it survived")
	}
	for _, want := range []string{
		"as soon as we finish this call", // the honest, terminal replacement
		"The text is sent after the call ends",
		"The email is sent after the call ends",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing replacement %q", want)
		}
	}
}

// The closing words are correct as written -- they ask the caller to do the one
// thing the agent cannot. Only the repetition is a defect.
func TestRewriteKeepsTheClosingLineButStopsItRepeating(t *testing.T) {
	out := RewriteUnsupportedActions(realSteps)
	if !strings.Contains(out, "Please feel free to cut the call if you have no other questions") {
		t.Fatal("the polite hand-off to the caller must survive")
	}
	if !strings.Contains(out, "Do not say goodbye a second time") {
		t.Error("the Hang Up step gained no stop condition, so it can loop again")
	}
}

// Appending is what makes this cache-safe, so it must happen exactly once even
// if a prompt is somehow rewritten twice.
func TestRewriteIsIdempotent(t *testing.T) {
	once := RewriteUnsupportedActions(realSteps)
	twice := RewriteUnsupportedActions(once)
	if once != twice {
		t.Error("rewriting twice changed the prompt again; the note would stack")
	}
	if n := strings.Count(twice, "You have no way to hang up"); n != 1 {
		t.Errorf("guidance note appears %d times, want 1", n)
	}
}

// A prompt with no unsupported action must come back byte-identical: an
// untouched prompt is an untouched KV-cache prefix.
func TestRewriteLeavesACleanPromptAlone(t *testing.T) {
	clean := "## Global Prompt\nBe brief.\n\n### Step 1: Greet\nSay hello.\n"
	if out := RewriteUnsupportedActions(clean); out != clean {
		t.Errorf("a clean prompt was modified:\n%q", out)
	}
	if out := RewriteUnsupportedActions(""); out != "" {
		t.Error("empty prompt must stay empty")
	}
}

// The apostrophe arrives as U+2019 from anything authored in a word processor.
func TestRewriteHandlesTypographicApostrophes(t *testing.T) {
	in := "Say: \"I’m sending you a text message with the details right now.\" and then send the SMS."
	out := RewriteUnsupportedActions(in)
	// Check the STEP, not the whole output: the appended guidance quotes the
	// offending phrase back at the model on purpose.
	step := strings.SplitN(out, "\n\n## Ending the call", 2)[0]
	if strings.Contains(step, "right now") || strings.Contains(step, "send the SMS") {
		t.Errorf("smart-quote variant not matched: %q", step)
	}
}
