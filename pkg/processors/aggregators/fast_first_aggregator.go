package aggregators

import (
	"reflect"
	"strings"

	"github.com/weedge/pipeline-go/pkg"
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/processors"
)

// FastFirstAggregator batches streamed LLM tokens into speakable units for TTS.
// The FIRST unit of each reply is flushed as soon as a few words are available
// (at a word boundary), so the caller hears the opening of the answer hundreds
// of milliseconds sooner instead of waiting for a full clause. Every unit after
// the first flushes at normal sentence/clause punctuation. The "first" state
// resets when endFrame (the LLM's turn-end frame) passes, i.e. once per reply.
type FastFirstAggregator struct {
	*processors.FrameProcessor
	aggregation   string
	endFrame      reflect.Type
	firstDone     bool
	minFirstWords int
}

// NewFastFirstAggregatorWithEnd builds the aggregator. endFrame marks end of a
// reply (flush remainder + reset). minFirstWords is how many complete words to
// accumulate before flushing the opening chunk.
func NewFastFirstAggregatorWithEnd(endFrame reflect.Type, minFirstWords int) *FastFirstAggregator {
	if minFirstWords < 1 {
		minFirstWords = 4
	}
	return &FastFirstAggregator{
		FrameProcessor: processors.NewFrameProcessor("FastFirstAggregator"),
		endFrame:       endFrame,
		minFirstWords:  minFirstWords,
	}
}

func (a *FastFirstAggregator) hasEndOfSentence(s string) bool {
	return pkg.MatchEndOfSentence(strings.TrimSpace(s))
}

func (a *FastFirstAggregator) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
	isPushAgg := reflect.TypeOf(frame) == a.endFrame

	switch f := frame.(type) {
	case *frames.TextFrame:
		a.aggregation += f.Text
		if !a.firstDone {
			if a.hasEndOfSentence(a.aggregation) {
				a.flushAll(direction)
				a.firstDone = true
			} else if a.flushOpening(direction) {
				a.firstDone = true
			}
		} else if a.hasEndOfSentence(a.aggregation) {
			a.flushAll(direction)
		}
	case *frames.EndFrame:
		a.flushAll(direction)
		a.firstDone = false
		a.PushFrame(f, direction)
	default:
		a.PushFrame(frame, direction)
	}

	if isPushAgg {
		a.flushAll(direction)
		a.firstDone = false
	}
}

// flushOpening emits the opening words once at least minFirstWords are complete
// (i.e. followed by whitespace), never splitting a word still being streamed.
// Returns true if it flushed.
func (a *FastFirstAggregator) flushOpening(direction processors.FrameDirection) bool {
	fields := strings.Fields(a.aggregation)
	endsSpace := strings.HasSuffix(a.aggregation, " ") || strings.HasSuffix(a.aggregation, "\n")
	complete := len(fields)
	if !endsSpace {
		complete-- // the last token may still be a partial word
	}
	if complete < a.minFirstWords {
		return false
	}
	var head string
	if endsSpace {
		head = strings.TrimSpace(a.aggregation)
		a.aggregation = ""
	} else {
		cut := strings.LastIndex(strings.TrimRight(a.aggregation, " "), " ")
		if cut <= 0 {
			return false
		}
		head = a.aggregation[:cut]
		a.aggregation = a.aggregation[cut+1:]
	}
	if strings.TrimSpace(head) != "" {
		a.PushFrame(frames.NewTextFrame(head), direction)
	}
	return true
}

func (a *FastFirstAggregator) flushAll(direction processors.FrameDirection) {
	if strings.TrimSpace(a.aggregation) == "" {
		a.aggregation = ""
		return
	}
	a.PushFrame(frames.NewTextFrame(a.aggregation), direction)
	a.aggregation = ""
}
