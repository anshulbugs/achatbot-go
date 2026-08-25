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

// Greetings that escaped the first phrase list, taken from 719 transcripts on
// the 25 Aug campaign. Call screening was the single biggest gap: the list had
// "record your message" but not "record your name", and that one word let a
// third of the misses through.
func TestLooksLikeVoicemailGreeting_SecondRoundFromTheLogs(t *testing.T) {
	machine := []string{
		"If you record your name and reason for calling, I'll see if this person is available.",
		"Record your name and reason for calling. I'll see if this person is available.",
		"Hi, this is Ron. Give me your name and number, and I'll return your call. Thanks.",
		"You've reached Adelia. I'm unable to pick up the phone, but please leave your name, number, and a brief message.",
		"John, I can't take your call. Please leave a message.",
		"Can't take your call now.",
		"Please press 1 to connect with our sales department. Please press 2 to connect with our recruitment department.",
		"Press 1 to dial by name. Press 2 for a list of extensions.",
		"Eric, if you would like me to get back to you, please leave a message. Thanks.",
		"Robert Nebel. I am unavailable at the moment, so if you could please leave me a message.",
	}
	for _, s := range machine {
		if !LooksLikeVoicemailGreeting(s) {
			t.Errorf("still missing a real greeting: %q", s)
		}
	}
}

// THREE CANDIDATES WERE DELIBERATELY LEFT OUT, and these lock that decision in.
//
// Each appeared in the logs often enough to be tempting -- "you've reached" 9
// times, "reason for calling" 37, "stay on the line" 28 -- and each is also
// something a live person at a business genuinely says. Since human_business is
// exactly the population the guard acts on, matching them would hang up on the
// receptionists we most need to keep. The screening greetings they appear in
// are caught by "see if this person is available" anyway, so excluding them
// costs coverage nothing.
func TestLooksLikeVoicemailGreeting_ReceptionistPhrasesAreLeftAlone(t *testing.T) {
	humans := []string{
		"You've reached Acme Staffing, this is Bob, how can I help you?",
		"You have reached the front desk.",
		"Please stay on the line while I transfer you.",
		"Stay on the line, I'll put you through.",
		"May I ask the reason for calling?",
		"Sure, what's the reason for calling today?",
	}
	for _, s := range humans {
		if LooksLikeVoicemailGreeting(s) {
			t.Errorf("would have hung up on a receptionist: %q", s)
		}
	}
}
