package processors

import "testing"

// fakeASR returns canned transcripts so the processor can be exercised without
// the GPU service.
type fakeASR struct{ says []string }

func (f *fakeASR) Transcribe(audio []byte) string {
	if len(f.says) == 0 {
		return ""
	}
	s := f.says[0]
	f.says = f.says[1:]
	return s
}
func (f *fakeASR) Warmup()        {}
func (f *fakeASR) Name() string   { return "fake" }
func (f *fakeASR) Reset() error   { return nil }
func (f *fakeASR) Release() error { return nil }

// The first transcript on a call is the one callers report as garbled, so it
// has to be findable. Counting every ASR result -- including the empty ones --
// is deliberate: an opening that came back empty is itself the symptom, and
// numbering only the transcripts that survived would hide it.
func TestASRProcessorNumbersEveryResultFromTheFirst(t *testing.T) {
	p := NewASRProcessor(&fakeASR{says: []string{"", "Hello?", "uh", "I'm listening"}}).
		WithCallID("v3:abc")

	if p.callID != "v3:abc" {
		t.Fatalf("call id not held: %q", p.callID)
	}

	var delivered []string
	p.WithOnTranscript(func(text string) { delivered = append(delivered, text) })

	for i := 0; i < 4; i++ {
		p.emit(make([]byte, 32000)) // 1s of 16 kHz 16-bit mono
	}

	if p.turns != 4 {
		t.Errorf("counted %d results, want 4 -- an empty or filler opening must still be numbered", p.turns)
	}
	// Empty and filler-only results must not reach the conversation: they are
	// pauses, and answering them chops the caller's sentence into fragments.
	want := []string{"Hello?", "I'm listening"}
	if len(delivered) != len(want) {
		t.Fatalf("delivered %v, want %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Errorf("delivered[%d] = %q, want %q", i, delivered[i], want[i])
		}
	}
}

// A browser session has no call id, and must not break for want of one.
func TestASRProcessorWorksWithoutACallID(t *testing.T) {
	p := NewASRProcessor(&fakeASR{says: []string{"hello"}})
	var got string
	p.WithOnTranscript(func(text string) { got = text })
	p.emit(make([]byte, 32000))
	if got != "hello" {
		t.Errorf("transcript = %q, want %q", got, "hello")
	}
}
