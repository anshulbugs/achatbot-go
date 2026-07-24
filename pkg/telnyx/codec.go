package telnyx

// G.711 µ-law (PCMU) codec and anti-aliased resampling for bridging Telnyx's
// 8 kHz µ-law call audio to and from the pipeline's 16-bit PCM.

import "math"

const muLawBias = 0x84
const muLawClip = 32635

// encodeMuLaw converts one 16-bit PCM sample to a µ-law byte.
func encodeMuLaw(sample int16) byte {
	sign := byte(0)
	s := int(sample)
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > muLawClip {
		s = muLawClip
	}
	s += muLawBias
	exponent := 7
	for mask := 0x4000; (s&mask) == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := (s >> (exponent + 3)) & 0x0F
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

// decodeMuLaw converts one µ-law byte to a 16-bit PCM sample.
func decodeMuLaw(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F
	sample := (int(mantissa) << 3) + muLawBias
	sample <<= exponent
	sample -= muLawBias
	if sign != 0 {
		sample = -sample
	}
	return int16(sample)
}

// MuLawToPCM16 decodes a µ-law byte slice into little-endian 16-bit PCM.
func MuLawToPCM16(mulaw []byte) []byte {
	out := make([]byte, len(mulaw)*2)
	for i, u := range mulaw {
		s := decodeMuLaw(u)
		out[2*i] = byte(s)
		out[2*i+1] = byte(s >> 8)
	}
	return out
}

// PCM16ToMuLaw encodes little-endian 16-bit PCM into µ-law.
func PCM16ToMuLaw(pcm []byte) []byte {
	n := len(pcm) / 2
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		s := int16(pcm[2*i]) | int16(pcm[2*i+1])<<8
		out[i] = encodeMuLaw(s)
	}
	return out
}

const lanczosA = 4.0

func lanczos(x float64) float64 {
	if x == 0 {
		return 1
	}
	if x <= -lanczosA || x >= lanczosA {
		return 0
	}
	px := math.Pi * x
	return lanczosA * math.Sin(px) * math.Sin(px/lanczosA) / (px * px)
}

// ResamplePCM16 resamples little-endian 16-bit mono PCM from fromRate to
// toRate using a Lanczos kernel. When downsampling, the kernel is widened by
// the rate ratio so it low-pass filters below the new Nyquist frequency; this
// anti-aliasing is what keeps downsampled speech (e.g. 24 kHz TTS to 8 kHz
// telephony) clear instead of scratchy.
func ResamplePCM16(pcm []byte, fromRate, toRate int) []byte {
	if fromRate == toRate {
		return pcm
	}
	n := len(pcm) / 2
	if n == 0 {
		return nil
	}
	in := make([]float64, n)
	for i := 0; i < n; i++ {
		in[i] = float64(int16(pcm[2*i]) | int16(pcm[2*i+1])<<8)
	}

	ratio := float64(toRate) / float64(fromRate)
	scale := 1.0
	if ratio < 1.0 {
		scale = 1.0 / ratio // widen kernel to low-pass when downsampling
	}
	outN := int(float64(n) * ratio)
	out := make([]byte, outN*2)
	half := lanczosA * scale
	for i := 0; i < outN; i++ {
		center := float64(i) / ratio // position in the input signal
		left := int(math.Ceil(center - half))
		right := int(math.Floor(center + half))
		var sum, wsum float64
		for j := left; j <= right; j++ {
			if j < 0 || j >= n {
				continue
			}
			w := lanczos((center - float64(j)) / scale)
			sum += in[j] * w
			wsum += w
		}
		v := 0.0
		if wsum != 0 {
			v = sum / wsum
		}
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		s := int16(v)
		out[2*i] = byte(s)
		out[2*i+1] = byte(s >> 8)
	}
	return out
}
