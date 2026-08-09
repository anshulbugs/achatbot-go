package llm_processors

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/common"
	"achatbot/pkg/types"
)

// The first-turn backpressure gate in pkg/rexa is only as good as the samples
// reaching it, and nothing else in the process notices if they stop: the gate
// simply never fires and the agent goes on advertising a ceiling measured under
// a lighter workload. These tests hold the wiring in place.

// fakeProvider streams a scripted reply after a fixed delay, so a turn has a
// known time-to-first-token.
type fakeProvider struct {
	delay      time.Duration
	toolRounds int // how many tool-call-only rounds precede the spoken reply
	rounds     int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Generate(context.Context, types.LMGenerateArgs, string, common.OpenAICompletionRespFunc) {
}

func (f *fakeProvider) GenerateStream(context.Context, types.LMGenerateArgs, string, common.OpenAIStreamCompletionRespFunc) {
}

func (f *fakeProvider) Chat(context.Context, types.LMGenerateArgs, []types.Message, common.OpenAIChatCompletionRespFunc) {
}

func (f *fakeProvider) ChatStream(_ context.Context, _ types.LMGenerateArgs,
	_ []types.Message, respFunc common.OpenAIStreamChatCompletionRespFunc) {
	f.rounds++
	time.Sleep(f.delay)
	if f.rounds <= f.toolRounds {
		// A tool round produces no content at all — the caller hears nothing
		// and keeps waiting.
		return
	}
	_ = respFunc(&openai.ChatCompletionChunk{
		Choices: []openai.ChatCompletionChunkChoice{{
			Delta: openai.ChatCompletionChunkChoiceDelta{Content: "Hello there."},
		}},
	})
}

func runTurns(t *testing.T, p *fakeProvider, turns int) []time.Duration {
	t.Helper()
	size := 6
	session := common.NewSession("s", &size)

	var mu sync.Mutex
	var got []time.Duration
	session.SetLLMObserver(func(ttft time.Duration, turn int) {
		mu.Lock()
		defer mu.Unlock()
		if turn != len(got)+1 {
			t.Errorf("turn = %d, want %d — the number is what separates the "+
				"cold-prefill turn from the cheap ones", turn, len(got)+1)
		}
		got = append(got, ttft)
	})

	proc := NewLLMOpenAIApiProcessor(p, session, "chat", true, types.LMGenerateArgs{})
	for i := 0; i < turns; i++ {
		proc.chat(frames.NewTextFrame("hello"), processors.FrameDirectionDownstream)
	}
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestLLMTurnTimingReachesTheSession(t *testing.T) {
	got := runTurns(t, &fakeProvider{delay: 60 * time.Millisecond}, 2)
	if len(got) != 2 {
		t.Fatalf("observed %d turns, want 2 — without these samples the "+
			"first-turn gate never fires and never says so", len(got))
	}
	for i, d := range got {
		if d < 50*time.Millisecond || d > 2*time.Second {
			t.Fatalf("turn %d took %v, want ~60ms", i+1, d)
		}
	}
}

func TestToolRoundsCountTowardTheTurnTheCallerWaited(t *testing.T) {
	// Two tool rounds and then a reply: the caller waited for all three
	// requests. Timing each request separately would report this turn as fast
	// when the person on the phone heard a silence three times as long.
	got := runTurns(t, &fakeProvider{delay: 60 * time.Millisecond, toolRounds: 2}, 1)
	if len(got) != 1 {
		t.Fatalf("observed %d turns, want exactly 1 for a turn that made 3 "+
			"requests", len(got))
	}
	if got[0] < 150*time.Millisecond {
		t.Fatalf("turn took %v, want >=180ms — the tool rounds the caller "+
			"waited through were not counted", got[0])
	}
}

// slowProvider streams a long reply, so a test can interrupt it partway.
type slowProvider struct {
	chunks   int
	gap      time.Duration
	mu       sync.Mutex
	streamed int
	ctxErr   error
}

func (f *slowProvider) Name() string { return "slow" }
func (f *slowProvider) Generate(context.Context, types.LMGenerateArgs, string, common.OpenAICompletionRespFunc) {
}
func (f *slowProvider) GenerateStream(context.Context, types.LMGenerateArgs, string, common.OpenAIStreamCompletionRespFunc) {
}
func (f *slowProvider) Chat(context.Context, types.LMGenerateArgs, []types.Message, common.OpenAIChatCompletionRespFunc) {
}

func (f *slowProvider) ChatStream(ctx context.Context, _ types.LMGenerateArgs,
	_ []types.Message, respFunc common.OpenAIStreamChatCompletionRespFunc) {
	for i := 0; i < f.chunks; i++ {
		select {
		case <-ctx.Done():
			// This is the whole point: a cancelled turn must stop generating.
			f.mu.Lock()
			f.ctxErr = ctx.Err()
			f.mu.Unlock()
			return
		case <-time.After(f.gap):
		}
		f.mu.Lock()
		f.streamed++
		f.mu.Unlock()
		_ = respFunc(&openai.ChatCompletionChunk{
			Choices: []openai.ChatCompletionChunkChoice{{
				Delta: openai.ChatCompletionChunkChoiceDelta{Content: "word "},
			}},
		})
	}
}

// When the caller interrupts, the pipeline drops the TTS audio so they stop
// hearing the reply — but the model used to run the turn to completion anyway.
// The whole reply then landed in the end-of-call transcript as if it had been
// spoken, and a cancelled turn kept consuming LLM capacity live calls needed.
func TestInterruptionStopsGeneration(t *testing.T) {
	prov := &slowProvider{chunks: 50, gap: 20 * time.Millisecond}
	size := 6
	session := common.NewSession("s", &size)
	proc := NewLLMOpenAIApiProcessor(prov, session, "chat", true, types.LMGenerateArgs{})

	done := make(chan struct{})
	go func() {
		proc.chat(frames.NewTextFrame("hello"), processors.FrameDirectionDownstream)
		close(done)
	}()

	time.Sleep(120 * time.Millisecond)
	proc.ProcessFrame(&frames.StartInterruptionFrame{}, processors.FrameDirectionDownstream)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("generation did not stop after the interruption")
	}

	prov.mu.Lock()
	streamed, ctxErr := prov.streamed, prov.ctxErr
	prov.mu.Unlock()
	if ctxErr == nil {
		t.Fatal("the provider never saw a cancelled context — generation ran to completion")
	}
	if streamed >= prov.chunks {
		t.Fatalf("streamed %d/%d chunks; the interruption did not cut it short", streamed, prov.chunks)
	}
	t.Logf("stopped after %d/%d chunks (%v)", streamed, prov.chunks, ctxErr)
}
