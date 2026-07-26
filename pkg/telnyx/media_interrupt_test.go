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
