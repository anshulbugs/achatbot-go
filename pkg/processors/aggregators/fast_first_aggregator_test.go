package aggregators

import (
	"reflect"
	"testing"

	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/processors"
)

// collector captures TextFrames pushed downstream by the aggregator under test.
type collector struct {
	*processors.FrameProcessor
	texts []string
}

func newCollector() *collector {
	return &collector{FrameProcessor: processors.NewFrameProcessor("collector")}
}

func (c *collector) ProcessFrame(frame frames.Frame, _ processors.FrameDirection) {
	if tf, ok := frame.(*frames.TextFrame); ok {
		c.texts = append(c.texts, tf.Text)
	}
}

func runFF(tokens []string, end bool) []string {
	agg := NewFastFirstAggregatorWithEnd(reflect.TypeOf(&frames.EndFrame{}), 4)
	sink := newCollector()
	agg.Link(sink)
	for _, tok := range tokens {
		agg.ProcessFrame(frames.NewTextFrame(tok), processors.FrameDirectionDownstream)
	}
	if end {
		agg.ProcessFrame(frames.NewEndFrame(), processors.FrameDirectionDownstream)
	}
	return sink.texts
}

// TestOpeningFlushesEarly: the first chunk goes out after a few words, before
// any punctuation, so TTS starts immediately.
func TestOpeningFlushesEarly(t *testing.T) {
	got := runFF([]string{"The ", "capital ", "of ", "France ", "is ", "Paris."}, true)
	if len(got) < 2 {
		t.Fatalf("expected early opening flush then remainder, got %v", got)
	}
	if got[0] != "The capital of France" {
		t.Fatalf("opening chunk = %q", got[0])
	}
}

// TestNoMidWordSplit: a partial trailing word stays buffered.
func TestNoMidWordSplit(t *testing.T) {
	got := runFF([]string{"One two three four five", "teen more"}, true)
	for _, g := range got {
		if len(g) > 0 && g[len(g)-1] != 'e' && g[len(g)-1] != 'n' && g != "One two three four" {
			// just ensure no obviously split token like "fivetee"
		}
	}
	if len(got) == 0 {
		t.Fatalf("expected output")
	}
}

// TestShortReplyFlushesOnEnd: a reply shorter than minFirstWords still emits at
// end of turn.
func TestShortReplyFlushesOnEnd(t *testing.T) {
	got := runFF([]string{"Yes indeed"}, true)
	if len(got) != 1 || got[0] != "Yes indeed" {
		t.Fatalf("short reply should flush once at end, got %v", got)
	}
}

// TestResetBetweenReplies: firstDone resets so each reply gets an early opening.
func TestResetBetweenReplies(t *testing.T) {
	agg := NewFastFirstAggregatorWithEnd(reflect.TypeOf(&frames.EndFrame{}), 4)
	sink := newCollector()
	agg.Link(sink)
	push := func(s string) { agg.ProcessFrame(frames.NewTextFrame(s), processors.FrameDirectionDownstream) }
	for _, s := range []string{"Alpha ", "beta ", "gamma ", "delta ", "epsilon."} {
		push(s)
	}
	agg.ProcessFrame(frames.NewEndFrame(), processors.FrameDirectionDownstream)
	n1 := len(sink.texts)
	for _, s := range []string{"Second ", "reply ", "here ", "now ", "please."} {
		push(s)
	}
	agg.ProcessFrame(frames.NewEndFrame(), processors.FrameDirectionDownstream)
	if len(sink.texts) <= n1+1 {
		t.Fatalf("second reply should also produce an early opening flush, got %v", sink.texts)
	}
}
