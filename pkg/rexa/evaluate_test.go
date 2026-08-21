package rexa

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeLLM struct {
	calls  int
	system string
	user   string
	reply  string
	err    error
}

func (f *fakeLLM) Complete(_ context.Context, system, user string, _ int) (string, error) {
	f.calls++
	f.system, f.user = system, user
	return f.reply, f.err
}

// newTestEvaluator drives the gate on a fake clock, so a test that exercises a
// twenty-second wait still runs instantly and deterministically.
func newTestEvaluator(llm LLMClient, m *Metrics, maxWait time.Duration) (*Evaluator, *Gate) {
	g := newTestGate(m, 1, maxWait)
	return NewEvaluator(llm, g), g
}

// newTestGate drives admission control on a fake clock, so a test that
// exercises a sixty-second wait still runs instantly and deterministically.
func newTestGate(m *Metrics, concurrency int, maxWait time.Duration) *Gate {
	g := NewGate(m, concurrency, maxWait)
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }
	g.sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		return nil
	}
	return g
}

func TestEvaluatorRunsImmediatelyOnAQuietBox(t *testing.T) {
	llm := &fakeLLM{reply: "score 8"}
	e, _ := newTestEvaluator(llm, NewMetrics(20), 20*time.Second)

	got, err := e.Run(context.Background(), EvalRequest{
		SessionID:   "s1",
		Instruction: "Rate the agent out of ten.",
		Transcript:  []MessageTurn{{Role: RoleAgent, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Result != "score 8" || got.SessionID != "s1" {
		t.Errorf("unexpected response %+v", got)
	}
	if got.WaitedMS != 0 || got.Deferred {
		t.Errorf("a quiet box should not have waited: waited=%dms deferred=%v", got.WaitedMS, got.Deferred)
	}
}

// The whole point of the feature: a box carrying calls must not have a
// transcript-sized prefill dropped on it the instant one ends.
func TestEvaluatorWaitsWhileCallsAreOnTheGPU(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < 6; i++ { // past half the ceiling
		if !m.TryReserve(sessionN(i)) {
			t.Fatalf("could not reserve session %d", i)
		}
	}
	llm := &fakeLLM{reply: "ok"}
	e, _ := newTestEvaluator(llm, m, 20*time.Second)

	got, err := e.Run(context.Background(), EvalRequest{
		SessionID:   "s1",
		Instruction: "Rate it.",
		Transcript:  []MessageTurn{{Role: RoleAgent, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.WaitedMS < 20000 {
		t.Errorf("expected to wait out the full deadline on a busy box, waited %dms", got.WaitedMS)
	}
	if !got.Deferred {
		t.Error("a run that waited out the deadline must report Deferred")
	}
	// It still ran: an evaluation that never happens is a broken feature.
	if llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1 — it must run anyway once the wait expires", llm.calls)
	}
}

// A box that frees up mid-wait should proceed at once rather than serving out
// the whole deadline.
func TestEvaluatorProceedsAsSoonAsTheBoxGoesQuiet(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < 6; i++ {
		m.TryReserve(sessionN(i))
	}
	llm := &fakeLLM{reply: "ok"}
	e, g := newTestEvaluator(llm, m, 20*time.Second)

	// Release the calls after the first poll.
	polls := 0
	realSleep := g.sleep
	g.sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		if polls == 2 {
			for i := 0; i < 6; i++ {
				m.Release(sessionN(i))
			}
		}
		return realSleep(ctx, d)
	}

	got, err := e.Run(context.Background(), EvalRequest{
		SessionID:   "s1",
		Instruction: "Rate it.",
		Transcript:  []MessageTurn{{Role: RoleAgent, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Deferred {
		t.Error("should not be marked Deferred when the box went quiet in time")
	}
	if got.WaitedMS >= 20000 {
		t.Errorf("should have stopped waiting early, waited %dms", got.WaitedMS)
	}
}

// Queued LLM requests mean a live caller is already waiting for a slot, which
// is the one signal that should stop an evaluation dead regardless of how many
// calls are connected.
func TestEvaluatorTreatsAQueuedLLMAsBusy(t *testing.T) {
	m := NewMetrics(100) // nowhere near the call ceiling
	m.setSGLang(SGLangSnapshot{OK: true, QueuedReqs: 3, at: time.Now()})
	_, g := newTestEvaluator(&fakeLLM{reply: "ok"}, m, 5*time.Second)
	if !g.busy() {
		t.Error("a non-zero LLM queue must count as busy")
	}
}

// The queue almost never moves on this hardware: measured at 100 concurrent
// requests, num_queue_reqs stayed 0 while num_running_reqs sat at ~105. If
// running requests did not also count as busy, the gate would be inert exactly
// when it is needed.
func TestEvaluatorTreatsRunningRequestsAsBusyEvenWithAnEmptyQueue(t *testing.T) {
	m := NewMetrics(100)
	m.setSGLang(SGLangSnapshot{OK: true, QueuedReqs: 0, RunningReqs: 105, at: time.Now()})
	_, g := newTestEvaluator(&fakeLLM{reply: "ok"}, m, 5*time.Second)
	if !g.busy() {
		t.Error("a busy LLM with an empty queue must still count as busy")
	}
}

func TestEvaluatorIgnoresATrickleOfLLMTraffic(t *testing.T) {
	m := NewMetrics(100)
	// A handful in flight is what normal call traffic looks like.
	m.setSGLang(SGLangSnapshot{OK: true, RunningReqs: EvalBusyRunningReqs - 1, at: time.Now()})
	_, g := newTestEvaluator(&fakeLLM{reply: "ok"}, m, 5*time.Second)
	if g.busy() {
		t.Error("ordinary call traffic must not block evaluations forever")
	}
}

func TestEvaluatorRefusesIncompleteRequests(t *testing.T) {
	e, _ := newTestEvaluator(&fakeLLM{}, NewMetrics(10), time.Second)
	_, err := e.Run(context.Background(), EvalRequest{SessionID: "s1"})
	if err != nil {
		// Run itself does not validate; the handler does. This just documents
		// that an empty transcript reaches the model as empty text rather than
		// panicking.
		t.Fatalf("Run with an empty transcript should not error: %v", err)
	}
}

// Prefill is the cost that hurts live calls, so an unbounded transcript must
// not become an unbounded prompt.
func TestRenderTranscriptTrimsTheMiddleOfAVeryLongCall(t *testing.T) {
	turns := make([]MessageTurn, EvalMaxTranscriptTurns*3)
	for i := range turns {
		turns[i] = MessageTurn{Role: RoleUser, Content: "turn"}
	}
	turns[0].Content = "FIRST"
	turns[len(turns)-1].Content = "LAST"

	out := renderTranscript(turns)
	if !strings.Contains(out, "FIRST") || !strings.Contains(out, "LAST") {
		t.Error("the opening and the ending must both survive trimming")
	}
	if !strings.Contains(out, "middle of a long call omitted") {
		t.Error("a trimmed transcript must say so, not silently lose turns")
	}
	if n := strings.Count(out, "\n"); n > EvalMaxTranscriptTurns+2 {
		t.Errorf("rendered %d lines, want at most %d", n, EvalMaxTranscriptTurns+2)
	}
}

func TestRenderTranscriptLabelsSpeakers(t *testing.T) {
	out := renderTranscript([]MessageTurn{
		{Role: RoleAgent, Content: "Hi there"},
		{Role: RoleUser, Content: "Who is this?"},
	})
	want := "AGENT: Hi there\nPERSON: Who is this?\n"
	if out != want {
		t.Errorf("renderTranscript = %q, want %q", out, want)
	}
}

// The system prompt must tell the model the text came from ASR — otherwise it
// marks the agent down for the recogniser's mistakes.
func TestEvalSystemPromptWarnsAboutTranscription(t *testing.T) {
	got := evalSystemPrompt("Rate politeness.")
	if !strings.Contains(got, "speech recognition") {
		t.Error("the prompt must say the transcript is from ASR")
	}
	if !strings.Contains(got, "Rate politeness.") {
		t.Error("the platform's own instruction must be carried through verbatim")
	}
}

func sessionN(i int) string { return "sess-" + string(rune('a'+i)) }
