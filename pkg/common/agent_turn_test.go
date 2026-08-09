package common

import (
	"testing"
	"time"
)

// Rexa kept receiving text the caller never heard. The model finishes a reply
// in a second or two while the voice takes ten, so on an interruption
// "generated" and "heard" are very different things — and only the audio
// actually sent says which is which.
func TestInterruptedTurnIsTrimmedToWhatWasSpoken(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	s.SetSpeakingRate(14) // characters per second

	var got string
	s.SetAgentTurnObserver(func(text string) { got = text })

	// The model produced all of this; the voice reached about the first second.
	s.RecordAgentChunk("Thanks for taking my call. ")
	s.RecordAgentChunk("To get a better idea of what might suit you, ")
	s.RecordAgentChunk("how long have you been with your current carrier?")
	s.RecordSpokenAudio(1200 * time.Millisecond)

	s.FlushAgentTurn(true)

	if got == "" {
		t.Fatal("nothing reported for an interrupted turn that had been speaking")
	}
	if len(got) > 40 {
		t.Fatalf("reported %d chars for 1.2s of speech: %q", len(got), got)
	}
	if got[len(got)-1] == ' ' {
		t.Errorf("trailing space left on %q", got)
	}
	t.Logf("1.2s of audio -> %q", got)
}

// A turn that ends normally is reported in full. Its audio is still playing,
// but all of it will be heard, so trimming would under-report.
func TestCompletedTurnIsReportedInFull(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	s.SetSpeakingRate(14)

	var got string
	s.SetAgentTurnObserver(func(text string) { got = text })

	full := "Thanks for taking my call. How long have you been with your current carrier?"
	s.RecordAgentChunk(full)
	s.RecordSpokenAudio(200 * time.Millisecond) // barely started playing
	s.FlushAgentTurn(false)

	if got != full {
		t.Fatalf("completed turn was trimmed to %q", got)
	}
}

// Interrupted inside the first word: nothing meaningful reached the caller, so
// nothing is reported. Inventing a fragment would read as the agent saying it.
func TestInterruptedImmediatelyReportsNothing(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	s.SetSpeakingRate(14)

	called := false
	s.SetAgentTurnObserver(func(string) { called = true })
	s.RecordAgentChunk("Absolutely, let me check that for you")
	s.RecordSpokenAudio(30 * time.Millisecond)
	s.FlushAgentTurn(true)

	if called {
		t.Fatal("reported a turn the caller never heard")
	}
}

// The accumulator resets, or one turn's audio would trim the next.
func TestSpokenDurationResetsBetweenTurns(t *testing.T) {
	size := 4
	s := NewSession("s", &size)
	s.SetSpeakingRate(14)

	var turns []string
	s.SetAgentTurnObserver(func(text string) { turns = append(turns, text) })

	s.RecordAgentChunk("First reply, spoken in full.")
	s.RecordSpokenAudio(5 * time.Second)
	s.FlushAgentTurn(false)

	// One second of speech, so this lands past the first word and the trim is
	// actually exercised rather than hitting the nothing-was-heard case.
	s.RecordAgentChunk("Second reply, cut off shortly after it began here.")
	s.RecordSpokenAudio(1 * time.Second)
	s.FlushAgentTurn(true)

	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %q", len(turns), turns)
	}
	// 1s at 14 chars/s is a handful of words. Carrying the first turn's five
	// seconds over would have reported the whole thing.
	if len(turns[1]) > 25 {
		t.Fatalf("second turn kept %d chars from 1s of speech: %q — the accumulator did not reset",
			len(turns[1]), turns[1])
	}
}
