package processors

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/turngate"
)

// TurnGateProcessor sits between ASR and the LLM. It accumulates ASR text
// fragments (emitted whenever the VAD sees a short pause) and asks a small
// gate LLM whether the caller has finished. Only completed turns are forwarded
// downstream to the main LLM; incomplete ones are held until the caller
// continues or a safety timeout fires. This makes a short VAD end-of-turn
// silence usable without answering half-finished sentences.
type TurnGateProcessor struct {
	*processors.AsyncFrameProcessor
	gate    *turngate.Gate
	maxWait time.Duration

	mu       sync.Mutex
	buffer   string
	timer    *time.Timer
	onDecide func(refined string, complete bool)
}

// NewTurnGateProcessor builds the gate. maxWait is the absolute silence after
// which a held (incomplete) utterance is forwarded anyway, so the agent never
// dead-airs when the gate misjudges.
func NewTurnGateProcessor(gate *turngate.Gate, maxWait time.Duration) *TurnGateProcessor {
	return &TurnGateProcessor{
		AsyncFrameProcessor: processors.NewAsyncFrameProcessor("TurnGateProcessor"),
		gate:                gate,
		maxWait:             maxWait,
	}
}

// WithOnDecide registers a callback fired with each gate decision (for logging).
func (p *TurnGateProcessor) WithOnDecide(fn func(refined string, complete bool)) *TurnGateProcessor {
	p.onDecide = fn
	return p
}

func (p *TurnGateProcessor) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
	p.AsyncFrameProcessor.WithPorcessFrameAllowPush(false).ProcessFrame(frame, direction)

	switch f := frame.(type) {
	case *frames.StartFrame:
		p.PushFrame(f, direction)
	case *frames.EndFrame:
		p.cancelTimer()
		p.PushFrame(f, direction)
	case *frames.CancelFrame:
		p.cancelTimer()
		p.PushFrame(f, direction)
	case *frames.TextFrame:
		p.handleText(f.Text)
	default:
		p.QueueFrame(f, direction)
	}
}

func (p *TurnGateProcessor) handleText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	p.mu.Lock()
	if p.buffer == "" {
		p.buffer = text
	} else {
		p.buffer += " " + text
	}
	combined := p.buffer
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	decision := p.gate.Decide(ctx, combined)
	cancel()
	if p.onDecide != nil {
		p.onDecide(decision.Refined, decision.Complete)
	}

	if decision.Complete {
		p.flush(decision.Refined)
		return
	}
	// Incomplete: keep buffering, but arm the safety timeout so a misjudged
	// "wait" cannot hang the call.
	p.armTimer()
}

// flush forwards the given text as the caller's completed turn and clears state.
func (p *TurnGateProcessor) flush(text string) {
	p.mu.Lock()
	p.buffer = ""
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return
	}
	p.QueueFrame(frames.NewTextFrame(text), processors.FrameDirectionDownstream)
}

// armTimer (re)starts the safety timeout; on expiry the buffered text is
// forwarded as-is.
func (p *TurnGateProcessor) armTimer() {
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.maxWait, func() {
		p.mu.Lock()
		buf := p.buffer
		p.buffer = ""
		p.timer = nil
		p.mu.Unlock()
		if strings.TrimSpace(buf) != "" {
			logger.Infof("turn gate: safety timeout, forwarding %q", buf)
			p.QueueFrame(frames.NewTextFrame(buf), processors.FrameDirectionDownstream)
		}
	})
	p.mu.Unlock()
}

func (p *TurnGateProcessor) cancelTimer() {
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
}
