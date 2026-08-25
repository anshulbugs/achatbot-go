package rexa

import "strings"

// Backchannel detection: the noises a listener makes while somebody else is
// talking, which are not a turn and must not be answered as one.
//
// WHY THIS EXISTS. A caller said "hello" while the greeting was still playing.
// It reached ASR clipped — the echo gate only opens once inbound audio clears
// the echo floor, so the onset of a word spoken over the bot is already gone —
// and came back as "Yeah". The greeting ends on "Would you be interested…", the
// model is told the greeting was delivered and replied to, so it read "Yeah" as
// agreement and answered "That's great to hear". The caller had not heard the
// question at that point and had not agreed to anything.
//
// Two separate failures met there. The transcript was wrong, which is the echo
// gate's problem and is bounded by preRoll. And a one-word reply arriving over
// the bot's own speech was treated as a considered answer, which is this one.
//
// THE ASYMMETRY, AGAIN. Dropping a real sentence would silence a caller trying
// to end the call — "stop calling me" must always get through. So this matches
// only utterances that are ENTIRELY backchannel tokens: every word has to be in
// the list, and the list holds nothing that carries meaning on its own. "yeah"
// is dropped; "yeah I have five years" is not, because it says something.

// backchannelWords are the tokens that can make up a non-turn. Kept short and
// closed on purpose: every addition is a new way to swallow a real answer.
var backchannelWords = map[string]bool{
	"hello": true, "hallo": true, "hullo": true,
	"hi": true, "hey": true,
	"yeah": true, "yea": true, "yep": true, "yup": true, "ya": true,
	"yes": true,
	"ok":  true, "okay": true, "kay": true,
	"hmm": true, "hm": true, "mhm": true, "mm": true, "mmhmm": true,
	"uh": true, "huh": true, "uhhuh": true, "mmm": true,
	"sorry": true, "pardon": true, "what": true,
	"right": true, "sure": true,
	"a": true, "ah": true, "oh": true,
}

// maxBackchannelWords bounds how long an utterance can be and still be nothing.
// Two covers the real cases ("uh huh", "yeah yeah", "hello hello"); three starts
// admitting short sentences that mean something.
const maxBackchannelWords = 2

// IsShortAcknowledgement reports whether a transcript is pure backchannel — a
// listening noise rather than a turn.
//
// Callers should consult this ONLY while the bot is actually speaking. The same
// "yeah" said into silence, after a question, is an answer and must be treated
// as one.
func IsShortAcknowledgement(text string) bool {
	fields := strings.Fields(normaliseTranscript(text))
	if len(fields) == 0 || len(fields) > maxBackchannelWords {
		return false
	}
	for _, w := range fields {
		if !backchannelWords[w] {
			return false
		}
	}
	return true
}
