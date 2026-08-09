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

// maxPickupWords bounds what can be dismissed as a pickup noise. Anything
// longer is a sentence, whatever words it happens to contain.
const maxPickupWords = 4

// IsPickupNoise reports whether text is nothing but a greeting reflex.
//
// Requires at least one real greeting word, so a stray "oh" or a mis-transcribed
// filler on its own is not mistaken for one — silence is what the caller gets if
// this returns true, and it should only ever be returned for something that
// genuinely carries no information.
func IsPickupNoise(text string) bool {
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
