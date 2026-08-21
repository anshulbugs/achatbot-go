package rexa

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// End-of-call evaluation, run on the agent so the platform never has to reach
// the LLM directly.
//
// WHY IT LIVES HERE. The LLM listens on 127.0.0.1 with no API key, which is
// safe only because nothing outside the box can reach it. Exposing it would
// mean publishing an unauthenticated GPU. This endpoint borrows the
// authentication the platform already uses — the same HMAC envelope as
// /connection — and keeps the model private.
//
// THE WHOLE DESIGN IS ABOUT NOT HURTING CALLS. An evaluation feeds a full
// transcript, so its prefill is an order of magnitude larger than a live turn's,
// and it arrives exactly when a call has just ended and others are still
// running. Three things keep it out of the way, in order of how much they
// matter:
//
//  1. It WAITS for the box to be quiet, rather than starting on arrival.
//  2. At most EvalConcurrency run at once, so a burst of hangups cannot
//     become a burst of prefills.
//  3. It gives up waiting after EvalMaxWait and runs anyway, because a
//     permanently deferred evaluation is a broken feature, and by then the
//     concurrency cap alone bounds the damage.
//
// Latency is explicitly not a goal. Seconds are fine. Minutes are not, which
// is what EvalMaxWait encodes.

// LLMClient is the minimum this package needs from the language model.
//
// An interface rather than an HTTP client so this package keeps its
// stdlib-only dependency set, and so tests can run the gate logic without a
// model behind it.
type LLMClient interface {
	// Complete returns the model's reply to a system and user message. It must
	// honour ctx, which carries the caller's deadline.
	Complete(ctx context.Context, system, user string, maxTokens int) (string, error)
}

// EvalRequest is what the platform posts to /evaluate.
type EvalRequest struct {
	// SessionID ties the evaluation to the call it judges. Logged, and echoed
	// back so a reply cannot be matched to the wrong call.
	SessionID string `json:"session_id"`
	// Instruction is what to assess — the platform owns the rubric, because it
	// changes per campaign and should not need an agent deploy.
	Instruction string `json:"instruction"`
	// Transcript is the conversation to judge. Sent by the platform rather
	// than looked up here: the agent's copy is dropped when the call ends, and
	// the platform already holds the report it built from our callback.
	Transcript []MessageTurn `json:"transcript"`
	// MaxTokens bounds the reply. Zero means EvalDefaultMaxTokens.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// EvalResponse is the reply. Result is the model's raw output — this endpoint
// deliberately does not parse it, because the rubric lives on the platform and
// only the platform knows what shape it asked for.
type EvalResponse struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	// WaitedMS is how long the request sat waiting for a quiet window. Worth
	// returning rather than hiding: a WaitedMS that is regularly at the
	// ceiling means evaluations are competing with calls and the fleet needs
	// more headroom, and that is invisible from the platform side otherwise.
	WaitedMS int64 `json:"waited_ms"`
	// Deferred is true when the wait ran out and this was run against a busy
	// box anyway.
	Deferred bool `json:"deferred,omitempty"`
}

const (
	// EvalDefaultMaxTokens is a few hundred words — enough for a rubric answer
	// with reasoning, short enough that one evaluation cannot occupy a
	// generation slot for long.
	EvalDefaultMaxTokens = 512
	// EvalMaxTranscriptTurns bounds prefill. A pathological call could
	// otherwise put thousands of turns of prefill in front of live traffic.
	// The opening states intent and the ending states the outcome, so an
	// overlong transcript is trimmed from the middle exactly as the report is.
	EvalMaxTranscriptTurns = 200
)

// Evaluator runs evaluations off the critical path.
type Evaluator struct {
	llm     LLMClient
	metrics *Metrics
	// sem bounds how many evaluations may run at once.
	sem chan struct{}
	// maxWait is how long a request may sit waiting for a quiet window before
	// running regardless.
	maxWait time.Duration
	// pollEvery is how often the gate re-checks while waiting.
	pollEvery time.Duration
	// now and sleep exist so tests can drive the gate without real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewEvaluator builds an evaluator. concurrency and maxWait fall back to
// sensible defaults when non-positive.
func NewEvaluator(llm LLMClient, metrics *Metrics, concurrency int, maxWait time.Duration) *Evaluator {
	if concurrency <= 0 {
		concurrency = 2
	}
	if maxWait <= 0 {
		maxWait = 20 * time.Second
	}
	return &Evaluator{
		llm:       llm,
		metrics:   metrics,
		sem:       make(chan struct{}, concurrency),
		maxWait:   maxWait,
		pollEvery: 500 * time.Millisecond,
		now:       time.Now,
		// sleepCtx is shared with the callback poster.
		sleep: sleepCtx,
	}
}

// EvalBusyRunningReqs is how many in-flight LLM generations count as "the box
// is working". Live calls generate few concurrent requests — a turn takes about
// a second and a caller speaks every fifteen or so, so even sixty calls sit
// around five in flight. Anything well above that is a real burst.
const EvalBusyRunningReqs = 8

// busy reports whether the box is currently too loaded to take an evaluation.
//
// THREE SIGNALS, ANSWERING DIFFERENT QUESTIONS.
//
// Queued LLM requests is the unambiguous one: something is already waiting for
// a generation slot, so a large prefill in front of it makes a live caller wait
// longer. It is also the one that almost never fires here — MEASURED on the
// GH200 at 100 concurrent requests, num_queue_reqs stayed 0 while
// num_running_reqs sat at ~105 and token_usage at 0.03. The KV cache is big
// enough that SGLang admits everything and queues nothing, so treating an empty
// queue as "idle" would have made this whole gate inert.
//
// Running requests is therefore the signal that actually fires. It says how
// much generation is in flight regardless of whether anything had to wait.
//
// GPU-call occupancy is the predictive one: a call that is connected but
// currently listening is not using the LLM this instant, yet it will within a
// turn or two, so a box near its ceiling is about to be busy even if the LLM
// looks idle right now.
//
// The thresholds are deliberately generous. Refusing to evaluate whenever a
// single call is live would mean never evaluating on a busy fleet, and
// evaluations that never run are worse than evaluations that cost a few
// milliseconds of someone else's TTFT.
func (e *Evaluator) busy() bool {
	if e.metrics == nil {
		return false
	}
	snap := e.metrics.Snapshot()
	if snap.SGLang.OK {
		// Anything queued means a caller is already waiting for a slot.
		if snap.SGLang.QueuedReqs > 0 {
			return true
		}
		if snap.SGLang.RunningReqs >= EvalBusyRunningReqs {
			return true
		}
	}
	// Past half the GPU-call ceiling, assume the lull is temporary.
	if snap.Capacity.MaxGPUCalls > 0 {
		if snap.Capacity.GPUCost*2 >= float64(snap.Capacity.MaxGPUCalls) {
			return true
		}
	}
	return false
}

// waitForQuiet blocks until the box is idle enough, the deadline passes, or ctx
// ends. It reports how long it waited and whether it gave up waiting.
func (e *Evaluator) waitForQuiet(ctx context.Context) (time.Duration, bool) {
	start := e.now()
	for {
		if !e.busy() {
			return e.now().Sub(start), false
		}
		waited := e.now().Sub(start)
		if waited >= e.maxWait {
			return waited, true
		}
		if err := e.sleep(ctx, e.pollEvery); err != nil {
			return e.now().Sub(start), true
		}
	}
}

// Run performs one evaluation, waiting for a quiet window first.
func (e *Evaluator) Run(ctx context.Context, req EvalRequest) (EvalResponse, error) {
	if e.llm == nil {
		return EvalResponse{}, errors.New("no LLM client configured")
	}
	// The concurrency cap is taken BEFORE the quiet wait, not after. Taking it
	// after would let an unbounded number of requests sit in the gate and then
	// stampede the moment the box went quiet, which is the opposite of the
	// intent.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return EvalResponse{}, ctx.Err()
	}

	waited, deferred := e.waitForQuiet(ctx)
	if ctx.Err() != nil {
		return EvalResponse{}, ctx.Err()
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = EvalDefaultMaxTokens
	}
	out, err := e.llm.Complete(ctx, evalSystemPrompt(req.Instruction),
		renderTranscript(req.Transcript), maxTokens)
	if err != nil {
		return EvalResponse{}, err
	}
	return EvalResponse{
		SessionID: req.SessionID,
		Result:    out,
		WaitedMS:  waited.Milliseconds(),
		Deferred:  deferred,
	}, nil
}

// evalSystemPrompt wraps the platform's rubric.
//
// The wrapper is minimal on purpose: the platform owns what to assess, and an
// agent-side opinion about the rubric would be a second place to change when
// the rubric changes. What IS stated here is the only thing the platform
// cannot know — that this text is a phone transcript produced by speech
// recognition, so imperfect words are transcription noise rather than evidence
// about the call.
func evalSystemPrompt(instruction string) string {
	return "You are assessing a completed phone call between an AI agent and a person. " +
		"The transcript comes from automatic speech recognition, so expect misheard words, " +
		"missing punctuation and no speaker labels beyond the ones given; judge the " +
		"conversation, not the transcription quality.\n\n" + instruction
}

// renderTranscript turns turns into plain text for the model, trimming the
// middle of an overlong call rather than the ends.
func renderTranscript(turns []MessageTurn) string {
	if len(turns) > EvalMaxTranscriptTurns {
		half := EvalMaxTranscriptTurns / 2
		trimmed := make([]MessageTurn, 0, EvalMaxTranscriptTurns+1)
		trimmed = append(trimmed, turns[:half]...)
		trimmed = append(trimmed, MessageTurn{Role: RoleAgent,
			Content: "[... middle of a long call omitted ...]"})
		trimmed = append(trimmed, turns[len(turns)-half:]...)
		turns = trimmed
	}
	var b []byte
	for _, t := range turns {
		who := "AGENT"
		if t.Role == RoleUser {
			who = "PERSON"
		}
		b = append(b, who...)
		b = append(b, ": "...)
		b = append(b, t.Content...)
		b = append(b, '\n')
	}
	return string(b)
}

// SetEvaluator installs the evaluator that backs POST /evaluate. Without it the
// route is not registered at all, so a deployment with no LLM configured
// answers 404 rather than pretending to accept work.
func (s *Server) SetEvaluator(e *Evaluator) { s.eval = e }

// handleEvaluate serves POST /evaluate.
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	var req EvalRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.SessionID == "" || len(req.Transcript) == 0 {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "session_id and a non-empty transcript are required"))
		return
	}
	if req.Instruction == "" {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "instruction is required: it is the rubric to assess against"))
		return
	}

	resp, err := s.eval.Run(r.Context(), req)
	if err != nil {
		// A cancelled context is the platform hanging up on us, not a fault.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("rexa: evaluation for session=%s abandoned by the caller", req.SessionID)
			return
		}
		log.Printf("rexa: evaluation for session=%s failed: %v", req.SessionID, err)
		writeErr(w, Errorf(ErrCodeInternal, "evaluation failed"))
		return
	}
	if resp.Deferred {
		log.Printf("rexa: evaluation for session=%s ran on a busy box after waiting %dms",
			req.SessionID, resp.WaitedMS)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
