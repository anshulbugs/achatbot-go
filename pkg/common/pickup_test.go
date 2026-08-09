package common

import "testing"

func TestIsPickupNoise(t *testing.T) {
	filtered := []string{
		"Hello.", "hi", "Hey!", "Hello?", "Hi there", "hello hello",
		"Oh, hello", "um hi", "Hello there.", "HELLO",
		"What's up?", "whats up", "Sup", "How are you?", "How are you doing?",
		"How's it going", "Good morning", "Good afternoon.", "Evening",
		"Yes hello", "Yeah, hi", "You alright?",
	}
	for _, s := range filtered {
		if !IsPickupNoise(s) {
			t.Errorf("IsPickupNoise(%q) = false, want true", s)
		}
	}

	// Everything here must reach the model. The greeting usually ends with a
	// question, so an answer to it is the single most important thing the
	// caller says — dropping one would leave them having agreed to something
	// and heard nothing back.
	kept := []string{
		"Yes", "Yeah", "Sure", "Yes I do", "Speaking", "Who is this?",
		"Hello, who's calling?", "Hi, what is this about?", "Not interested",
		"Hello I am driving right now", "yes hello I have a minute",
		"", "oh", "um",
		// All start like a pickup phrase and none of them are one.
		"How do I stop these calls", "What is this about", "Whats the offer",
		"Good morning, who am I speaking to", "How much does it cost",
	}
	for _, s := range kept {
		if IsPickupNoise(s) {
			t.Errorf("IsPickupNoise(%q) = true, want false — this needs an answer", s)
		}
	}
}

// Only the OPENING turn is filtered.
//
// Later in a call "hello?" means something quite different — the caller is
// checking whether the line is still alive, usually because the agent has gone
// quiet. Staying silent then is the worst possible answer, so the filter
// switches itself off the moment anything with content arrives.
func TestOnlyTheOpeningTurnIsFiltered(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	s.SetFilterPickupNoise(true)

	if !s.ShouldSkipCallerTurn("Hello.") {
		t.Fatal("the opening hello was not filtered")
	}
	if !s.ShouldSkipCallerTurn("Hi there") {
		t.Fatal("a second reflex before any content was not filtered")
	}
	if s.ShouldSkipCallerTurn("Yes, I have a minute") {
		t.Fatal("a real answer was filtered")
	}
	// From here on the caller has spoken, so nothing is dropped.
	if s.ShouldSkipCallerTurn("Hello?") {
		t.Fatal("a mid-call 'hello?' was filtered — the caller is checking the " +
			"line is still there and silence is the one answer that makes it worse")
	}
	if s.ShouldSkipCallerTurn("hi") {
		t.Fatal("a mid-call greeting was filtered")
	}
}

// Without a spoken greeting there is nothing for the caller to be
// acknowledging, so their first word is a genuine turn.
func TestFilterIsOffByDefault(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	if s.ShouldSkipCallerTurn("Hello.") {
		t.Fatal("filtered with no greeting spoken")
	}
}
