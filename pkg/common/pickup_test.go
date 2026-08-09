package common

import "testing"

func TestIsPickupNoise(t *testing.T) {
	filtered := []string{
		"Hello.", "hi", "Hey!", "Hello?", "Hi there", "hello hello",
		"Oh, hello", "um hi", "Hello there.", "HELLO",
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
	}
	for _, s := range kept {
		if IsPickupNoise(s) {
			t.Errorf("IsPickupNoise(%q) = true, want false — this needs an answer", s)
		}
	}
}
