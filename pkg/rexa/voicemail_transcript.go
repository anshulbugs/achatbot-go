package rexa

import "strings"

// Second-line answering-machine detection, from what the caller actually said.
//
// WHY THIS EXISTS. The carrier's AMD is the first line and it is not reliable
// enough on its own. MEASURED over 95 dialled calls on 25 Aug 2026: at least 57
// reached voicemail, the carrier called only 12 of them `machine`, and 38 of
// the misses came back as `human_business` — a documented "a person at a
// business answered" verdict, returned for a recorded greeting. Every one of
// those ran a full pipeline: the agent held a conversation with a machine, held
// a GPU slot for the length of it, never left the voicemail message, and the
// call was recorded as a human answer, which inflates answer_rate as well.
//
// isMachineAMD stays exactly as it is. It mirrors what the carrier documents,
// and rewriting `human_business` to mean machine would turn every receptionist
// into a voicemail. This looks at the transcript instead, which is evidence
// about this call rather than a label about it.
//
// THE TWO ERRORS ARE NOT EQUAL, and the phrase list is built around that. A
// miss costs one wasted call — the situation we are already in. A false
// positive takes the agent off a call a real person is on, mid-sentence. So a
// phrase only earns its place here if a person answering their own phone would
// essentially never say it. That is why the list contains "at the tone" and not
// "not available", and "please leave your message" and not "leave a message":
// a receptionist really does say "he's not available, can I take a message?",
// and that exact sentence is the one the carrier labels human_business.

// voicemailPhrases are matched against a normalised transcript. Each one is
// drawn from a real greeting observed in the logs, or is the standard carrier
// wording for the same thing.
var voicemailPhrases = []string{
	// The tone or beep instruction. The single most reliable signal there is:
	// it exists only to tell a caller when to start recording.
	"at the tone",
	"after the tone",
	"at the beep",
	"after the beep",
	"wait for the tone",
	"wait for the beep",
	"following the tone",

	// Recording instructions.
	"record your message",
	"recording your message",
	"when you have finished recording",
	"when youve finished recording",
	"please leave your message",
	"please leave your name and number",
	"leave your message after",

	// The mailbox itself, named.
	"voicemail box",
	"voice mail box",
	"voicemail system",
	"voice messaging system",
	"automated voice message",
	"automated voice messaging",
	"has not been set up yet",

	// Carrier announcements that replace a mailbox.
	"person you are trying to reach",
	"person youre trying to reach",
	"party you are trying to reach",
	"subscriber you have dialed",
	"subscriber you have dialled",
	"number you have dialed",
	"number you have dialled",
	"no longer in service",
	"has been forwarded to",

	// Call screening. The single biggest gap in the first list: it had "record
	// your message" but not "record your name", and screening services were a
	// third of everything that escaped.
	//
	// "reason for calling" and "stay on the line" appear in the SAME greetings
	// and are deliberately absent: a receptionist says both ("may I ask the
	// reason for calling?", "stay on the line while I transfer you"), and the
	// phrases below already catch those calls without that risk.
	"record your name",
	"see if this person is available",
	"see if the person is available",

	// Unavailability, stated by the person themselves -- which only a recording
	// can do, since someone who says it is by definition on the call.
	"cant take your call",
	"can not take your call",
	"unable to take your call",
	"unable to pick up",
	"i am unavailable",
	"return your call",

	// Requests to leave something. "please leave a message" is the machine
	// form; a receptionist offers instead -- "can I take a message?" -- which
	// is why the bare "leave a message" is not here.
	"please leave a message",
	"please leave me a message",
	"leave your name",

	// Menu prompts. A person does not offer you keypad options.
	"press 1",
	"press one",
	"press 2",
	"press two",
	"dial by name",
	"list of extensions",
	"press pound",
	"press the pound key",
	"press one to leave",
	"play the choices again",
	"to page this person",
}

// LooksLikeVoicemailGreeting reports whether an ASR transcript is an answering
// machine or carrier announcement rather than a person speaking.
//
// Deliberately conservative: see the note above on why the two error directions
// are weighted differently. Callers should also bound WHEN this is consulted —
// a greeting arrives in the first seconds, and a phrase appearing later in a
// real conversation ("leave your message after the beep", quoted by a human) is
// not evidence of anything.
func LooksLikeVoicemailGreeting(text string) bool {
	if text == "" {
		return false
	}
	norm := normaliseTranscript(text)
	if norm == "" {
		return false
	}
	for _, phrase := range voicemailPhrases {
		if strings.Contains(norm, phrase) {
			return true
		}
	}
	return false
}

// normaliseTranscript lowercases, drops apostrophes so "you're" and "youre"
// compare equal, turns every other non-alphanumeric run into a single space,
// and pads the ends so a phrase can be matched without worrying about word
// boundaries at the edges.
//
// ASR output is unpunctuated as often as not and its capitalisation is
// arbitrary, so matching raw text would depend on how the recogniser happened
// to render this particular greeting.
func normaliseTranscript(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 2)
	b.WriteByte(' ')
	lastSpace := true
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '’':
			// Dropped, not spaced: "you're" must become "youre".
			lastSpace = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	if !lastSpace {
		b.WriteByte(' ')
	}
	return b.String()
}
