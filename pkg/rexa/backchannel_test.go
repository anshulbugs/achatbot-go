package rexa

import "testing"

// The case this exists for: the caller says "hello" while the greeting is still
// playing. Reported 25 Aug 2026 — it came back from ASR as "Yeah", the model
// read it as agreement to the question the greeting ends on, and answered
// "That's great to hear" to a person who had not heard the question yet.
func TestIsShortAcknowledgement_DroppedWhileTheBotIsTalking(t *testing.T) {
	acks := []string{
		"hello", "Hello?", "Hello.", "HELLO",
		"hi", "Hi.", "hey",
		"yeah", "Yeah.", "yep", "yup", "ya",
		"yes", "Yes.",
		"ok", "okay", "Okay.",
		"hmm", "mhm", "mm", "uh huh", "uh-huh", "mm hmm",
		"sorry", "pardon", "what", "huh",
		"right", "sure",
		"yeah yeah", "hello hello", "ok ok",
		"  hello  ", // whitespace must not defeat it
	}
	for _, s := range acks {
		if !IsShortAcknowledgement(s) {
			t.Errorf("should have been treated as backchannel: %q", s)
		}
	}
}

// ANYTHING WITH CONTENT MUST SURVIVE. Dropping these would silence a caller who
// is trying to stop the call, which is far worse than answering a stray "yeah".
func TestIsShortAcknowledgement_RealSpeechIsNeverDropped(t *testing.T) {
	real := []string{
		"stop",
		"stop calling me",
		"do not call",
		"don't call me again",
		"no",
		"no thanks",
		"not interested",
		"who is this",
		"what is this about",
		"yes I am interested in the role",
		"yeah I have five years of experience",
		"hello, who am I speaking to?",
		"hi, yes, I got your message",
		"remove me from your list",
		"",
	}
	for _, s := range real {
		if IsShortAcknowledgement(s) {
			t.Errorf("would have swallowed real speech: %q", s)
		}
	}
}
