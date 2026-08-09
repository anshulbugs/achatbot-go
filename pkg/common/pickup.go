package common

import "strings"

// Filtering the reflex "hello" a caller says when they pick up.
//
// The agent's greeting is already playing when the phone is answered, so the
// caller's first sound is almost always an acknowledgement rather than an
// answer: "hello", "hi", "yeah hello". Treating it as a turn makes the agent
// stop and reply to it, which produces the stilted exchange where a caller says
// hello and is told "thanks for taking my call" before anything has happened.
//
// Only the OPENING one is filtered. Once a real turn has happened, "hello" may
// well be the caller checking whether the line is still there — and answering
// that matters.
//
// The list is deliberately short. "Yes", "yeah" and "sure" are NOT on it: the
// greeting usually ends with a question like "do you have a quick moment?", and
// those are answers to it. Dropping them would leave the caller having agreed
// to something and heard nothing back, which is far worse than one redundant
// reply.
var pickupTokens = map[string]bool{
	"hi": true, "hello": true, "hey": true, "hiya": true, "hallo": true,
	"yo": true, "there": true, "howdy": true,
	// Fillers that ride along with a greeting and mean nothing on their own.
	"oh": true, "um": true, "uh": true, "er": true, "ah": true, "hm": true, "hmm": true,
}

// pickupPhrases are complete openers that carry no information, matched after
// normalisation (lowercased, punctuation and apostrophes stripped).
//
// Phrases rather than words because the useful ones are multi-word and their
// parts are not interchangeable: "how are you" is a pleasantry, "how do I stop
// this" is not, and both start "how".
var pickupPhrases = map[string]bool{
	"whats up": true, "wassup": true, "sup": true, "whats going on": true,
	"how are you": true, "how are you doing": true, "how you doing": true,
	"how are ya": true, "hows it going": true, "how is it going": true,
	"hows things": true, "you alright": true, "alright": true,
	"good morning": true, "good afternoon": true, "good evening": true,
	"good day": true, "morning": true, "afternoon": true, "evening": true,
	"yes hello": true, "yeah hello": true, "yes hi": true, "yeah hi": true,
	"hello yes": true, "hello hi": true, "hi hi": true,
}

// maxPickupWords bounds what can be dismissed as a pickup noise. Anything
// longer is a sentence, whatever words it happens to contain.
const maxPickupWords = 4

// normalisePickup lowercases and strips punctuation so "What”'s up?" and
// "whats up" are the same phrase. Apostrophes go entirely rather than being
// mapped, because speech-to-text is inconsistent about them.
func normalisePickup(text string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '	':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			// Punctuation and apostrophes are dropped, not turned into spaces,
			// so "what'''s" becomes "whats" rather than "what s".
		}
	}
	return strings.TrimSpace(b.String())
}

// IsPickupNoise reports whether text is nothing but a greeting reflex.
//
// Requires at least one real greeting word, so a stray "oh" or a mis-transcribed
// filler on its own is not mistaken for one — silence is what the caller gets if
// this returns true, and it should only ever be returned for something that
// genuinely carries no information.
func IsPickupNoise(text string) bool {
	if pickupPhrases[normalisePickup(text)] {
		return true
	}
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 || len(fields) > maxPickupWords {
		return false
	}
	greeting := false
	for _, f := range fields {
		word := strings.Trim(f, ".,!?;:'\"")
		if word == "" {
			continue
		}
		if !pickupTokens[word] {
			return false
		}
		switch word {
		case "hi", "hello", "hey", "hiya", "hallo", "yo", "howdy":
			greeting = true
		}
	}
	return greeting
}
