package rexa

import (
	"regexp"
	"strings"
)

// RewriteUnsupportedActions rewrites campaign-prompt steps that ask the agent to
// perform an action, so each one names the tool that actually performs it or
// says plainly that there is none.
//
// WHY. A campaign prompt is a step machine whose steps assume the agent can
// send a text, send an email, hang up and transfer. Only two of those are real:
// call_transfer, and end_call. The rest cannot complete, and a step that cannot
// complete is not merely a no-op -- nothing marks it done, so the model
// re-enters it on the next turn and repeats itself. One call carried SEVEN
// copies of "I'm sending you a text message with the details right now" while
// the candidate got steadily more explicit, and another seven copies of the
// goodbye line.
//
// canEndCall says whether end_call is registered on this call. Browser sessions
// have no carrier leg and get no such tool, and telling a model to invoke one it
// cannot see is how you get a model that stalls.
//
// TWO PROMPT FORMATS, BECAUSE PROMPTS ARE AUTHORED PER CAMPAIGN. One writes
// steps as prose ("Say: ... and then send the SMS."); another writes them as a
// verb ("Action: hangUp") under an indented header. The first version of this
// function knew only the prose form, so against a live campaign it rewrote
// NOTHING: 27 prompts carried Hang Up steps and none were touched, which is
// exactly why end_call was advertised on 20 calls and invoked on none. Hence
// both forms below -- and hence the guidance note is now appended
// unconditionally rather than only when a rule fires, so a format nobody has
// seen yet still gets the rules that matter.
func RewriteUnsupportedActions(prompt string, canEndCall bool) string {
	if prompt == "" {
		return prompt
	}
	out := prompt
	for _, r := range promptRewrites {
		out = r.re.ReplaceAllString(out, r.with)
	}
	out = hangUpAction.ReplaceAllString(out, hangUpReplacement(canEndCall))
	return out + guidanceNote(canEndCall)
}

// guidanceNote is the backstop for step wording no rule recognises. Appended at
// the very END, which is also what makes it free: prefix caching holds up to
// the first byte that differs between calls, and in these prompts that is the
// candidate's name a fifth of the way in, so nothing appended here was ever
// part of a shared prefix.
func guidanceNote(canEndCall bool) string {
	var b strings.Builder
	b.WriteString("\n\n## Actions, and what actually performs them\n")
	if canEndCall {
		b.WriteString("To end the call you MUST invoke the end_call tool. Saying goodbye does " +
			"not end it: the line stays open with the person still on it. Say your closing " +
			"line and invoke end_call in the SAME turn, and never say goodbye twice.\n")
	} else {
		b.WriteString("You cannot hang up. When the conversation is over, say so once and ask " +
			"the caller to put the phone down; do not repeat your goodbye.\n")
	}
	b.WriteString("To transfer to a human you MUST invoke the call_transfer tool. Saying you " +
		"are connecting them does not transfer them.\n")
	b.WriteString("You cannot send a text or an email during the call. Never say one is being " +
		"sent now -- say it will be sent once the call ends, say it once, and move on. If " +
		"asked again, say it goes out as soon as the call ends.\n")
	b.WriteString("Never repeat a sentence you have already said in this call.\n")
	return b.String()
}

// hangUpAction matches the verb form, indented or not, with either line ending.
// Anchoring to "^###..." was the mistake that made this whole function inert on
// a live campaign: the headers were indented four spaces.
var hangUpAction = regexp.MustCompile(`(?im)^([ \t]*)Action:[ \t]*hang[ _]?up[ \t\r]*$`)

func hangUpReplacement(canEndCall bool) string {
	if canEndCall {
		return "${1}Invoke the end_call tool now. Saying goodbye does not end the call."
	}
	return "${1}Ask the caller to hang up, once. You have no way to end the call yourself."
}

var promptRewrites = []struct {
	re   *regexp.Regexp
	with string
}{
	// --- verb form: "Action: sendSms" under an indented header ---
	{
		regexp.MustCompile(`(?im)^([ \t]*)Action:[ \t]*send[ _]?(sms|text)[ \t\r]*$`),
		"${1}Say the details will be texted once this call ends, then continue. " +
			"There is no tool for this: never say you are sending it now.",
	},
	{
		regexp.MustCompile(`(?im)^([ \t]*)Action:[ \t]*send[ _]?e?mail[ \t\r]*$`),
		"${1}Say the details will be emailed once this call ends, then continue. " +
			"There is no tool for this: never say you are sending it now.",
	},
	{
		regexp.MustCompile(`(?im)^([ \t]*)Action:[ \t]*transfer[ _]?call[ \t\r]*$`),
		"${1}Invoke the call_transfer tool now. Saying you are connecting them does " +
			"not transfer the call.",
	},

	// --- prose form: "Say: \"...\" and then send the SMS." ---
	{
		// Apostrophes arrive as both ' and U+2019 depending on who typed the step.
		regexp.MustCompile(`(?i)I['\x{2019}]m sending you (a text message|an email|an sms) with the details right now\.`),
		"I'll send you the details by text as soon as we finish this call.",
	},
	{
		regexp.MustCompile(`(?i)\s*,?\s*and then send the (SMS|text|email)\.`),
		"",
	},
	{
		regexp.MustCompile(`(?im)^([ \t]*)Send the (SMS|text)( silently)?\.[ \t\r]*$`),
		"${1}Say nothing here. The text is sent after the call ends; continue to the next step.",
	},
	{
		regexp.MustCompile(`(?im)^([ \t]*)Send the email( silently)?\.[ \t\r]*$`),
		"${1}Say nothing here. The email is sent after the call ends; continue to the next step.",
	},
}
