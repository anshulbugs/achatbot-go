package rexa

import "testing"

// Every "machine" case below is a real transcript taken from rexa-4399.log on
// 25 Aug 2026, from a call the carrier's AMD labelled human_business and which
// therefore ran a full pipeline against an answering machine.
func TestLooksLikeVoicemailGreeting_RealMissedCalls(t *testing.T) {
	machine := []string{
		"At the tone, please record your message. When you've finished recording, you may hang up.",
		"I'm sorry this person is not available. If you would like to leave a message, wait for the tone.",
		"The person you're trying to reach is not available. At the tone, please record your message.",
		"Has a voicemail box that has not been set up yet. Please try your call again later. Goodbye.",
		"Voicemail box that has not been set up yet. Please try your call again later. Goodbye.",
		"Seven, seven, nine, seven, one, three, three, one, zero, zero is not available right now. Please record your message after the tone.",
		"Play the choices again, press Pound.",
		"Your call has been forwarded to an automated voice message system.",
		"The Google subscriber you have dialed is not available.",
		"Please leave your message for", // truncated mid-greeting, still unmistakable
	}
	for _, s := range machine {
		if !LooksLikeVoicemailGreeting(s) {
			t.Errorf("missed a real voicemail greeting: %q", s)
		}
	}
}

// THE EXPENSIVE MISTAKE IS THE OTHER DIRECTION. A false positive takes the
// agent off a call a person is actually on, so anything a human plausibly says
// in the first seconds of answering has to survive this — including the
// receptionist phrasing that the carrier labels human_business and that sounds
// closest to a machine.
func TestLooksLikeVoicemailGreeting_HumansAreNotHungUpOn(t *testing.T) {
	humans := []string{
		"Hello?",
		"Yeah, hello.",
		"Hi, who's this?",
		"Sorry, I'm not available right now, can you call me back later?",
		"He's not available at the moment, can I take a message?",
		"She's in a meeting. Would you like to leave a message with me?",
		"I can leave a message for him if you want.",
		"Yeah I'm driving, what's this about?",
		"This is Lynda speaking.",
		"Can you hear me? Hello?",
		"I'm not interested, thanks.",
		"Who is calling please?",
		"Just a second, let me get a pen.",
		"", // an empty transcript must never trip anything
	}
	for _, s := range humans {
		if LooksLikeVoicemailGreeting(s) {
			t.Errorf("would have hung up on a person: %q", s)
		}
	}
}

func TestLooksLikeVoicemailGreeting_IsCaseAndPunctuationInsensitive(t *testing.T) {
	variants := []string{
		"AT THE TONE, PLEASE RECORD YOUR MESSAGE.",
		"at the tone please record your message",
		"...At The Tone, Please Record Your Message!",
	}
	for _, s := range variants {
		if !LooksLikeVoicemailGreeting(s) {
			t.Errorf("case or punctuation defeated the match: %q", s)
		}
	}
}
