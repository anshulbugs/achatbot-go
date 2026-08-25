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

// The opening floor must survive REPEATED phantoms. Protecting only "turn 1"
// leaves a second invented word to be answered, and the caller still has not
// spoken.
func TestOpeningFloorHoldsUntilTheCallerIsActuallyHeard(t *testing.T) {
	fake := &fakeASR{says: []string{"Yeah.", "Okay.", "Hello, who is this?"}}
	p := NewASRProcessor(fake).WithCallID("v3:abc").WithOpeningMinRMS(1000)
	var delivered []string
	p.WithOnTranscript(func(text string) { delivered = append(delivered, text) })

	p.emit(pcmAt(113, 12800))  // the measured phantom level
	p.emit(pcmAt(400, 12800))  // still not a person
	p.emit(pcmAt(1800, 12800)) // the caller, at last

	if len(delivered) != 1 || delivered[0] != "Yeah." {
		t.Fatalf("delivered %v; want only the segment loud enough to be real", delivered)
	}
	if len(fake.says) != 2 {
		t.Error("a quiet opening was sent to the ASR; the GPU call should be skipped")
	}
	if p.turns != 3 {
		t.Errorf("turns = %d, want 3 -- dropped openings must still be countable", p.turns)
	}
}

// Once the caller has been heard, the strict opening floor must stop applying:
// a quiet "no" later in the call is a real answer.
func TestOpeningFloorStopsAfterTheCallerHasSpoken(t *testing.T) {
	p := NewASRProcessor(&fakeASR{says: []string{"Hello?", "No."}}).WithOpeningMinRMS(1000)
	var delivered []string
	p.WithOnTranscript(func(text string) { delivered = append(delivered, text) })

	p.emit(pcmAt(1800, 12800)) // opening, loud
	p.emit(pcmAt(300, 12800))  // a quiet reply mid-call
	if len(delivered) != 2 || delivered[1] != "No." {
		t.Errorf("delivered %v; a quiet answer after the opening must survive", delivered)
	}
}

// The ASR invents "Yeah." from near-silence -- measured at RMS 1.0 and 5.8
// against real speech at 1425 -- so a floor must drop those segments before
// they reach the model, and must not touch anything that sounds like a person.
func TestASRProcessorDropsSegmentsTooQuietToBeSpeech(t *testing.T) {
	quiet := &fakeASR{says: []string{"Yeah."}}
	p := NewASRProcessor(quiet).WithCallID("v3:abc").WithMinRMS(60)
	var delivered []string
	p.WithOnTranscript(func(text string) { delivered = append(delivered, text) })

	p.emit(pcmAt(5, 12800)) // 0.4s at RMS 5 -- the level that hallucinates
	if len(delivered) != 0 {
		t.Errorf("near-silence reached the conversation as %v", delivered)
	}
	if len(quiet.says) != 1 {
		t.Error("near-silence was sent to the ASR; the GPU call should be skipped entirely")
	}
	// It still counts as a turn: a dropped opening is exactly what we want to
	// be able to find in the log.
	if p.turns != 1 {
		t.Errorf("turns = %d, want 1", p.turns)
	}

	p.emit(pcmAt(1400, 12800)) // real speech
	if len(delivered) != 1 || delivered[0] != "Yeah." {
		t.Errorf("real speech was dropped: %v", delivered)
	}
}

// With no floor set the processor must behave exactly as before.
func TestASRProcessorFloorIsOffByDefault(t *testing.T) {
	p := NewASRProcessor(&fakeASR{says: []string{"Yeah."}})
	var delivered []string
	p.WithOnTranscript(func(text string) { delivered = append(delivered, text) })
	p.emit(pcmAt(2, 12800))
	if len(delivered) != 1 {
		t.Errorf("default behaviour changed: %v", delivered)
	}
}

// pcmAt builds 16-bit mono PCM of n bytes at approximately the given RMS.
func pcmAt(rms float64, n int) []byte {
	b := make([]byte, n)
	v := int16(rms) // a square wave: |sample| == RMS
	for i := 0; i+1 < n; i += 2 {
		s := v
		if (i/2)%2 == 1 {
			s = -v
		}
		b[i] = byte(uint16(s))
		b[i+1] = byte(uint16(s) >> 8)
	}
	return b
}
