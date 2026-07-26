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
