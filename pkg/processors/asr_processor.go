package processors

import (
	"math"
	"strings"
	"unicode"

	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/common"
	achatbot_frames "achatbot/pkg/types/frames"
)

// asrSampleRate is the rate the audio handed to Transcribe is at (16 kHz, 16-bit
// mono), used only to report a duration alongside each transcript.
const asrSampleRate = 16000

// segmentRMS is the loudness of one ASR segment on the int16 scale.
//
// Logged with every transcript because the ASR HALLUCINATES ON NEAR-SILENCE and
// nothing else distinguishes that from a real reply. MEASURED against the
// production Parakeet (nvidia/parakeet-tdt-0.6b-v2), 0.8s clips:
//
//	digital silence   RMS    0.0  -> ""
//	phone-band noise  RMS    1.0  -> "Yeah."
//	phone-band noise  RMS    5.8  -> "Yeah."
//	phone-band noise  RMS   31.4  -> "Uh"
//	real speech       RMS 1425.0  -> "Hello."
//
// "Yeah." was 97 of 290 short transcripts in one run -- 21% of every ASR result
// on the box -- which is not a thing callers do. Note the gap: three orders of
// magnitude between what fools it and what a person sounds like.
func segmentRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// fillerWords are hesitation tokens that, when a transcript contains nothing
// else, mean the caller merely paused to think — not a turn to answer.
var fillerWords = map[string]bool{
	"uh": true, "uhh": true, "um": true, "umm": true, "hmm": true, "hm": true,
	"mm": true, "mmm": true, "er": true, "err": true, "ah": true, "eh": true, "huh": true,
}

// fillerOnly reports whether text carries no real content — punctuation only or
// only hesitation tokens. These come from mid-thought pauses that the VAD ends
// as a turn; answering them chops the caller's sentence into fragments.
func fillerOnly(text string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		}
	}
	words := strings.Fields(b.String())
	if len(words) == 0 {
		return true
	}
	for _, w := range words {
		if !fillerWords[w] {
			return false
		}
	}
	return true
}

type ASRProcessor struct {
	*processors.AsyncFrameProcessor
	provider     common.IASRProvider
	onTranscript func(text string)
	// callID identifies which call this transcript belongs to. Empty on
	// browser sessions, which are one at a time and need no disambiguation.
	callID string
	// minRMS, when > 0, discards a segment quieter than this instead of
	// transcribing it. Zero disables the check -- see segmentRMS for why it
	// exists and why the threshold is measured rather than guessed.
	minRMS float64
	// openingMinRMS is a stricter floor applied only until the caller has said
	// something real. See WithOpeningMinRMS.
	openingMinRMS float64
	// heard is set once a transcript has been accepted, which is what ends the
	// opening. Deliberately NOT "turn 1": when the opening produces two
	// phantoms in a row, protecting only the first leaves the second to be
	// answered, and the whole point is that nothing invented gets answered
	// before the caller has actually spoken.
	heard bool
	// turns counts transcripts emitted on this call, so the FIRST one -- the
	// one callers report as garbled -- can be found without reading backwards
	// through the whole call.
	turns int
}

// WithCallID labels every transcript from this processor with the call it came
// from.
//
// WHY THIS IS NOT COSMETIC. The log is one interleaved stream from up to fifty
// concurrent calls, and an ASR line carried no identity at all, so two lines a
// millisecond apart could belong to different conversations:
//
//	ASR result (30720 audio bytes -> 3 chars): "No?"
//	ASR result (50176 audio bytes -> 16 chars): "Yeah, thank you."
//
// Attributing one of those to a specific call meant finding a phrase unique to
// that conversation inside an LLM ChatHistory dump and working backwards. That
// is just about workable for a post-mortem on a call you already know about,
// and useless for "how often does this happen?" -- which is the question that
// matters when callers report their first sentence being misheard.
func (p *ASRProcessor) WithCallID(id string) *ASRProcessor {
	p.callID = id
	return p
}

// WithMinRMS discards segments quieter than rms without transcribing them.
//
// Off by default (0), deliberately. The measurement above says a floor
// anywhere between about 60 and 300 separates hallucination from speech with
// enormous margin, but that was measured on synthetic audio, and a real phone
// line is quieter than a rendered voice. Set it from production RMS once
// logged, rather than shipping a guess that could swallow a quiet "no".
func (p *ASRProcessor) WithMinRMS(rms float64) *ASRProcessor {
	p.minRMS = rms
	return p
}

// WithOpeningMinRMS discards segments quieter than rms until the caller has
// been heard for the first time.
//
// THE OPENING IS WHERE THIS COSTS MOST. A phantom mid-conversation is a wasted
// turn; a phantom on the opening answers "Is this a good time to chat?" on
// behalf of someone who only said hello, or routes them into the do-not-call
// branch. Both happened on real calls.
//
// MEASURED on one run, RMS of the first segment of each call:
//
//	1072 1289 1629 1779 2387 2551 2562 3527   real speech
//	 113                                       "Yeah." from nothing
//
// The gap is wide but the floor is NOT far below the quietest real opening --
// 1000 against 1072 is about 7% of margin on a sample of nine. That is thin,
// and the failure it buys is mild: a dropped opening is silence, so the caller
// simply speaks again, and on most calls the greeting is still playing anyway.
// The failure it prevents is the agent acting on words nobody said. Watch the
// "ASR dropped" lines, which carry the RMS: real speech appearing there means
// this is set too high.
func (p *ASRProcessor) WithOpeningMinRMS(rms float64) *ASRProcessor {
	p.openingMinRMS = rms
	return p
}

func NewASRProcessor(provider common.IASRProvider) *ASRProcessor {
	return &ASRProcessor{
		AsyncFrameProcessor: processors.NewAsyncFrameProcessor("ASRProcessor"),
		provider:            provider,
	}
}

func (p *ASRProcessor) WithPassRawAudio(passRawAudio bool) *ASRProcessor {
	p.AsyncFrameProcessor = p.AsyncFrameProcessor.WithPassRawAudio(passRawAudio)
	return p
}

// WithOnTranscript registers a callback fired with each non-empty user
// transcript, used to surface the user's speech to the client. The callback
// runs on the ASR processing goroutine.
func (p *ASRProcessor) WithOnTranscript(fn func(text string)) *ASRProcessor {
	p.onTranscript = fn
	return p
}

// emit transcribes the audio and pushes a downstream TextFrame only when the
// result is non-empty, so silence or noise that slips past the VAD doesn't
// trigger a spurious LLM turn (which otherwise makes the model ramble or
// guess a language). Also notifies the transcript callback.
func (p *ASRProcessor) emit(audio []byte) {
	p.turns++
	rms := segmentRMS(audio)
	secs0 := float64(len(audio)) / float64(asrSampleRate*2)
	floor, why := p.minRMS, "too quiet to be speech"
	if !p.heard && p.openingMinRMS > floor {
		floor, why = p.openingMinRMS, "too quiet to be the caller's opening"
	}
	if floor > 0 && rms < floor {
		// Not transcribed at all: this is the audio the model invents "Yeah."
		// from, and skipping it saves the GPU call as well.
		logger.Infof("ASR dropped call=%s turn=%d (%.2fs audio, rms %.0f < %.0f): %s",
			p.callID, p.turns, secs0, rms, floor, why)
		return
	}
	text := strings.TrimSpace(p.provider.Transcribe(audio))
	// Duration, not byte count: "0.96s of audio came back as No?" is a fact
	// anyone can judge, where "30720 bytes" needs the sample rate and a
	// calculator first. asrSampleRate is what Transcribe is fed.
	logger.Infof("ASR result call=%s turn=%d (%.2fs audio, rms %.0f -> %d chars): %q",
		p.callID, p.turns, secs0, rms, len(text), text)
	if text == "" || fillerOnly(text) {
		return
	}
	p.heard = true
	if p.onTranscript != nil {
		p.onTranscript(text)
	}
	p.PushDownstreamFrame(frames.NewTextFrame(text))
}

func (p *ASRProcessor) Start(frame *frames.StartFrame) {
	logger.Info("ASRProcessor Start")
}

func (p *ASRProcessor) Stop(frame *frames.EndFrame) {
	logger.Info("ASRProcessor Stop")
}

func (p *ASRProcessor) Cancel(frame *frames.CancelFrame) {
	p.provider.Release()
	logger.Info("ASRProcessor Cancel")
}

// ProcessFrame processes a frame
func (p *ASRProcessor) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
	// call frame processor to init star frame init
	p.AsyncFrameProcessor.WithPorcessFrameAllowPush(false).ProcessFrame(frame, direction)

	switch f := frame.(type) {
	case *frames.StartFrame:
		p.PushFrame(f, direction)
		p.Start(f)
	case *frames.EndFrame:
		p.PushFrame(f, direction)
		p.Stop(f)
	case *frames.CancelFrame:
		p.PushFrame(f, direction)
		p.Cancel(f)
	case *frames.AudioRawFrame:
		if p.PassRawAudio() {
			p.QueueFrame(f, direction)
		}
		p.emit(f.Audio)
	case *achatbot_frames.VADStateAudioRawFrame:
		if p.PassRawAudio() {
			p.QueueFrame(f, direction)
		}
		p.emit(f.Audio)
	case *achatbot_frames.AnimationAudioRawFrame:
		if p.PassRawAudio() {
			p.QueueFrame(f, direction)
		}
		p.emit(f.Audio)
	default:
		p.QueueFrame(f, direction)
	}

}
