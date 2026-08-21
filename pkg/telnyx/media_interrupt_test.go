package telnyx

import (
	"testing"
	"time"

	"github.com/weedge/pipeline-go/pkg/frames"

	"achatbot/pkg/consts"
)

func audioFrame() *frames.AudioRawFrame {
	return frames.NewAudioRawFrame(make([]byte, 320), consts.DefaultRate, 1, 2)
}

// An interruption must drop the interrupted turn's tail but must never latch:
// a previous implementation lifted the mute only on a frame this pipeline never
// emits, which silenced every call after the first barge-in.
func TestInterruptionMuteExpires(t *testing.T) {
	s := NewSerializer(consts.DefaultRate)

	out, err := s.Serialize(audioFrame())
	if err != nil || len(out) == 0 {
		t.Fatalf("audio before interruption must be sent, got %d bytes err=%v", len(out), err)
	}

	if _, err := s.Serialize(&frames.StartInterruptionFrame{}); err != nil {
		t.Fatalf("interruption: %v", err)
	}

	out, err = s.Serialize(audioFrame())
	if err != nil {
		t.Fatalf("muted audio: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("audio during mute window must be dropped, got %d bytes", len(out))
	}

	time.Sleep(interruptMute + 50*time.Millisecond)

	out, err = s.Serialize(audioFrame())
	if err != nil {
		t.Fatalf("post-mute audio: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("mute latched: audio still dropped after the window expired")
	}
}

// tone builds an 8 kHz PCM chunk at a given amplitude.
func tone(amp float64, samples int) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := int16(amp)
		if i%2 == 0 {
			v = -v
		}
		b[2*i] = byte(uint16(v))
		b[2*i+1] = byte(uint16(v) >> 8)
	}
	return b
}

// A barge-in must reach ASR with its onset intact. The echo gate can only
// recognise speech once it clears the echo floor, so the quiet ramp-up of the
// first word is gated; without a pre-roll those samples are lost and the
// utterance arrives truncated ("Where do you live?" -> "Barely leave.").
func TestBargeInKeepsWordOnset(t *testing.T) {
	s := NewSerializer(consts.DefaultRate)
	const chunk = 160 // 20ms at 8kHz

	// Bot is speaking, so the gate is active.
	s.noteOutbound(8000)

	// Quiet echo establishes the floor.
	for i := 0; i < 5; i++ {
		if got := s.keepInbound(tone(200, chunk)); got != nil {
			t.Fatalf("echo-level audio must be gated, got %d bytes", len(got))
		}
	}
	// The caller's word ramps up: still under the barge threshold, so gated,
	// but it is real speech and must be retained.
	onset := 0
	for _, amp := range []float64{300, 420, 500} {
		if got := s.keepInbound(tone(amp, chunk)); got != nil {
			t.Fatalf("onset should still be gated, got %d bytes", len(got))
		}
		onset += chunk * 2
	}
	// Now clearly above the floor: barge-in fires.
	got := s.keepInbound(tone(4000, chunk))
	if got == nil {
		t.Fatal("loud barge-in must pass the gate")
	}
	if len(got) <= chunk*2 {
		t.Fatalf("barge-in dropped the word onset: got %d bytes, want the trigger chunk plus preceding audio", len(got))
	}
}

// Once the caller has barged in, the whole sentence must reach ASR. Normal
// speech dips below the echo floor between words and on unvoiced consonants;
// filtering frame by frame punched holes through the middle of the utterance,
// so overlapping speech came back as nonsense ("They did not feel the same for
// you.") while sentences begun in silence transcribed correctly.
func TestGateStaysOpenThroughMidSentenceDips(t *testing.T) {
	s := NewSerializer(consts.DefaultRate)
	const chunk = 160

	s.noteOutbound(16000) // bot speaking for 2s
	for i := 0; i < 5; i++ {
		s.keepInbound(tone(200, chunk)) // establish echo floor
	}
	if got := s.keepInbound(tone(4000, chunk)); got == nil {
		t.Fatal("barge-in must open the gate")
	}
	// A dip between words: quiet enough to look like echo, but the caller is
	// still mid-sentence and these samples carry the consonants.
	for i := 0; i < 6; i++ {
		if got := s.keepInbound(tone(180, chunk)); got == nil {
			t.Fatalf("frame %d of a mid-sentence dip was dropped: the utterance reaches ASR with holes", i)
		}
	}
	// Loud again, same sentence continuing.
	if got := s.keepInbound(tone(3500, chunk)); got == nil {
		t.Fatal("continued speech must pass")
	}
}

// A greeting is bot speech, and the echo gate has to know it.
//
// REGRESSION. The announcement path writes the greeting to the socket in one
// message rather than frame by frame, so it never called noteOutbound and
// playbackEndsAt stayed in the past. For the whole length of a greeting the
// gate reported the bot idle and passed every inbound byte straight to ASR —
// so a caller talking over the greeting was transcribed and answered as though
// they had heard all of it.
func TestGreetingGatesInboundLikeAnyOtherBotSpeech(t *testing.T) {
	s := NewSerializer(consts.DefaultRate)
	const chunk = 160 // 20ms at 8kHz

	// Before the announcement the bot is idle and inbound passes, which is what
	// makes the bug below possible in the first place.
	if got := s.keepInbound(tone(300, chunk)); got == nil {
		t.Fatal("inbound should pass while the bot is genuinely silent")
	}

	// Now a 5-second greeting goes out through the announcement path.
	s.NoteAnnouncement(5 * time.Second)

	// Quiet talking-over must be gated, exactly as during normal bot speech.
	for i := 0; i < 5; i++ {
		if got := s.keepInbound(tone(300, chunk)); got != nil {
			t.Fatalf("inbound at %d passed while the greeting was still playing — "+
				"this is the bug: the caller gets transcribed mid-greeting", i)
		}
	}
}

// The clock must not go backwards: a short announcement after a long one still
// leaves the longer one's tail protected.
func TestNoteAnnouncementExtendsRatherThanReplaces(t *testing.T) {
	s := NewSerializer(consts.DefaultRate)
	s.NoteAnnouncement(5 * time.Second)
	s.NoteAnnouncement(0)                // no-op
	s.NoteAnnouncement(-1 * time.Second) // no-op
	if got := s.keepInbound(tone(300, 160)); got != nil {
		t.Error("a zero or negative announcement must not clear the existing hold")
	}
}
