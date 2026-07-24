package processors

import (
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/common"
)

// streamingTTS is the optional low-latency synthesis path; providers that
// implement it deliver audio chunks as they render instead of one blob.
type streamingTTS interface {
	SynthesizeStream(text string, onAudio func(pcm []byte) bool)
}

type TTSProcessor struct {
	*processors.AsyncFrameProcessor
	provider common.ITTSProvider
}

func NewTTSProcessor(provider common.ITTSProvider) *TTSProcessor {
	return &TTSProcessor{
		AsyncFrameProcessor: processors.NewAsyncFrameProcessor("TTSProcessor"),
		provider:            provider,
	}
}

func (p *TTSProcessor) WithPassText(passText bool) *TTSProcessor {
	p.AsyncFrameProcessor = p.AsyncFrameProcessor.WithPassText(passText)
	return p
}

func (p *TTSProcessor) Start(frame *frames.StartFrame) {
	logger.Info("TTSProcessor Start")
}

func (p *TTSProcessor) Stop(frame *frames.EndFrame) {
	logger.Info("TTSProcessor Stop")
}

func (p *TTSProcessor) Cancel(frame *frames.CancelFrame) {
	p.provider.Release()
	logger.Info("TTSProcessor Cancel")
}

// ProcessFrame processes a frame
func (p *TTSProcessor) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
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
	case *frames.TextFrame:
		if p.PassText() {
			p.QueueFrame(f, direction)
		}
		rate, channels, sampleWidth := p.provider.GetSampleInfo()
		if st, ok := p.provider.(streamingTTS); ok {
			// Stream audio chunks as they render so the caller hears the
			// reply's opening ~250ms sooner than waiting for the full clause.
			st.SynthesizeStream(f.Text, func(pcm []byte) bool {
				p.PushDownstreamFrame(frames.NewAudioRawFrame(pcm, rate, channels, sampleWidth))
				return true
			})
		} else {
			audio := p.provider.Synthesize(f.Text)
			p.PushDownstreamFrame(frames.NewAudioRawFrame(audio, rate, channels, sampleWidth))
		}
	default:
		p.QueueFrame(f, direction)
	}

}
