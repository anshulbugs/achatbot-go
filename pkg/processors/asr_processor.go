package processors

import (
	"strings"

	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/common"
	achatbot_frames "achatbot/pkg/types/frames"
)

type ASRProcessor struct {
	*processors.AsyncFrameProcessor
	provider     common.IASRProvider
	onTranscript func(text string)
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
	logger.Infof("ASR result (%d audio bytes -> %d chars): %q", len(audio), len(text), text)
	if text == "" {
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
