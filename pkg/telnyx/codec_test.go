package telnyx

import (
	"math"
	"testing"
)

// TestMuLawRoundTrip checks that PCM -> µ-law -> PCM stays close to the input
// (µ-law is lossy but preserves loud samples well and small ones acceptably).
func TestMuLawRoundTrip(t *testing.T) {
	for _, in := range []int16{0, 100, -100, 1000, -1000, 8000, -8000, 20000, -20000, 32000, -32000} {
		u := encodeMuLaw(in)
		out := decodeMuLaw(u)
		diff := math.Abs(float64(in) - float64(out))
		tol := math.Abs(float64(in))*0.10 + 260 // ~µ-law quantization
		if diff > tol {
			t.Errorf("in=%d out=%d diff=%.0f tol=%.0f", in, out, diff, tol)
		}
	}
}

func TestMuLawSliceLengths(t *testing.T) {
	pcm := make([]byte, 320) // 160 samples
	mulaw := PCM16ToMuLaw(pcm)
	if len(mulaw) != 160 {
		t.Fatalf("expected 160 µ-law bytes, got %d", len(mulaw))
	}
	back := MuLawToPCM16(mulaw)
	if len(back) != 320 {
		t.Fatalf("expected 320 PCM bytes, got %d", len(back))
	}
}

// TestResampleLengths checks 8k<->16k produce the expected sample counts.
func TestResampleLengths(t *testing.T) {
	pcm8 := make([]byte, 160*2) // 160 samples @ 8k = 20ms
	up := ResamplePCM16(pcm8, 8000, 16000)
	if len(up) != 320*2 {
		t.Fatalf("upsample: expected 640 bytes, got %d", len(up))
	}
	down := ResamplePCM16(up, 16000, 8000)
	if len(down) != 160*2 {
		t.Fatalf("downsample: expected 320 bytes, got %d", len(down))
	}
	if got := ResamplePCM16(pcm8, 8000, 8000); len(got) != len(pcm8) {
		t.Fatalf("identity resample changed length: %d", len(got))
	}
}

// TestResampleTonePreserved checks a sine wave keeps roughly its amplitude
// after up/down resampling (no gross corruption).
func TestResampleTonePreserved(t *testing.T) {
	const n = 800
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(10000 * math.Sin(2*math.Pi*300*float64(i)/8000))
		pcm[2*i] = byte(s)
		pcm[2*i+1] = byte(s >> 8)
	}
	round := ResamplePCM16(ResamplePCM16(pcm, 8000, 16000), 16000, 8000)
	if len(round) != len(pcm) {
		t.Fatalf("length changed: %d vs %d", len(round), len(pcm))
	}
	var peak int16
	for i := 0; i < len(round)/2; i++ {
		s := int16(round[2*i]) | int16(round[2*i+1])<<8
		if s > peak {
			peak = s
		}
	}
	if peak < 7000 || peak > 12000 {
		t.Fatalf("tone amplitude not preserved, peak=%d", peak)
	}
}
