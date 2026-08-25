package processors

import (
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
	text := strings.TrimSpace(p.provider.Transcribe(audio))
	p.turns++
	// Duration, not byte count: "0.96s of audio came back as No?" is a fact
	// anyone can judge, where "30720 bytes" needs the sample rate and a
	// calculator first. asrSampleRate is what Transcribe is fed.
	secs := float64(len(audio)) / float64(asrSampleRate*2)
	logger.Infof("ASR result call=%s turn=%d (%.2fs audio -> %d chars): %q",
		p.callID, p.turns, secs, len(text), text)
	if text == "" || fillerOnly(text) {
		return
	}
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
