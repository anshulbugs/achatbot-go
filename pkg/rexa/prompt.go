package rexa

import (
	"regexp"
	"strings"
)

// RewriteUnsupportedActions removes instructions that tell the agent to perform
// an action it has no tool for, and replaces them with something it can
// actually do.
//
// WHY. A campaign prompt is a step machine, and its steps assume the agent can
// send a text, send an email, hang up and transfer. Only transfer is real: the
// one tool registered on a call is call_transfer. Counted on a live campaign
// prompt: 6 Send SMS steps, 6 Hang Up, 3 Send Email, 1 Transfer.
//
// The failure is not that the action quietly does nothing. It is that the step
// never COMPLETES. "Say X and then send the SMS" has no observable result, so
// nothing in the history marks it done, and on the next turn the model re-enters
// the same step and says the same sentence again. On a real call the agent said
// "I'm sending you a text message with the details right now" SEVEN times in a
// row while the candidate grew increasingly explicit -- "Hey, do it now.",
// "Don't send or not. Do send it or not." -- and every reply was byte-identical.
//
// So the rewrite makes the promise honest AND terminal: the agent says the text
// is coming after the call, which is true, needs no tool, and is a sentence the
// conversation can move on from.
//
// Hang Up steps are deliberately left alone. They already end with "Please feel
// free to cut the call if you have no other questions", which asks the caller to
// do the one thing the agent cannot -- so that sentence stays. But like Send SMS
// the step cannot complete, and one real call carried SEVEN copies of it: every
// time the candidate spoke, the model re-entered the step and said its goodbye
// again. So it gets a termination rule rather than a rewrite.
//
// That is a mitigation, not a cure. The durable fix is an end_call tool -- the
// carrier hangup already exists as telnyx.Client.Hangup and is called from four
// other places -- which would make all six steps do what they say.
//
// CACHE COST IS NIL, and that is not luck. Prefix caching only holds up to the
// first byte that differs between calls, and in these prompts the candidate's
// name lands at 18% -- every action step edited here sits well past it, in a
// region that was never shared. The Global Prompt header, which IS shared, is
// not touched. See the prompt-prefix-sharing note.
func RewriteUnsupportedActions(prompt string) string {
	if prompt == "" {
		return prompt
	}
	out := prompt
	for _, r := range promptRewrites {
		out = r.re.ReplaceAllString(out, r.with)
	}
	if out != prompt && !strings.Contains(out, promptActionNote) {
		out += promptActionNote
	}
	return out
}

// promptActionNote is appended once, and only when something was rewritten. It
// backstops step wording this does not recognise: the steps are authored per
// campaign, so an exact-match list will always be incomplete.
const promptActionNote = "\n\n## Ending the call, and sending things\n" +
	"You have no way to hang up. Say the closing line ONCE and then stop closing: " +
	"if the caller keeps talking, answer what they asked in one short sentence and " +
	"leave it to them to put the phone down. Never say goodbye twice in one call. " +
	"You cannot send a text or an email while the call is running. Never say one " +
	"is being sent now, and never say you are doing it \"right now\" -- say it will " +
	"be sent after the call, once, and then move on to the next step. If the caller " +
	"asks again whether you have sent it, tell them it goes out as soon as the call " +
	"ends. Do not repeat a sentence you have already said in this call.\n"

var promptRewrites = []struct {
	re   *regexp.Regexp
	with string
}{
	// The spoken promise, in the present tense. Apostrophes arrive as both ' and
	// U+2019 depending on who typed the step, so match either.
	{
		regexp.MustCompile(`(?i)I['\x{2019}]m sending you (a text message|an email|an sms) with the details right now\.`),
		"I'll send you the details by text as soon as we finish this call.",
	},
	// The action tacked onto a spoken line.
	{
		regexp.MustCompile(`(?i)\s*,?\s*and then send the (SMS|text|email)\.`),
		"",
	},
	// Whole steps whose entire body is the unsupported action.
	{
		regexp.MustCompile(`(?im)^\s*Send the (SMS|text) silently\.\s*$`),
		"Say nothing here. The text is sent after the call ends; continue to the next step.",
	},
	{
		regexp.MustCompile(`(?im)^\s*Send the email( silently)?\.\s*$`),
		"Say nothing here. The email is sent after the call ends; continue to the next step.",
	},
	{
		regexp.MustCompile(`(?im)^\s*Send the (SMS|text)\.\s*$`),
		"Say nothing here. The text is sent after the call ends; continue to the next step.",
	},
	// Terminal steps keep their wording; what they gain is a stop condition.
	{
		regexp.MustCompile(`(?m)^(### Step \d+: Hang Up)[ \t]*$`),
		// The header itself is changed, not just annotated: a rule that leaves
		// its own trigger intact appends again on every pass.
		"$1 -- say this once\n(If the caller speaks again afterwards, reply briefly and let them hang up. Do not say goodbye a second time.)",
	},
}
