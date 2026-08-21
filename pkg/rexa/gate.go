package rexa

import (
	"context"
	"time"
)

// Gate is the admission control that keeps background GPU work off the calls'
// critical path.
//
// ONE GATE FOR ALL BACKGROUND WORK, deliberately. Anything the platform asks
// the model for runs on the same GPU the calls run on, so the agent keeps a
// single, stated budget for everything that is not a live call rather than one
// budget per feature — which would make the real limit their sum, a limit
// nobody chose.
//
// What it does, in order:
//
//  1. Takes a concurrency slot, BEFORE waiting. Taking it after would let an
//     unbounded number of requests queue in the wait and then stampede the
//     instant the box went quiet, which is the opposite of the intent.
//  2. Waits for the box to look idle, re-checking every pollEvery.
//  3. Gives up after maxWait and proceeds anyway, because work that never runs
//     is a broken feature, and by then the concurrency slot alone bounds what
//     it can cost.
type Gate struct {
	metrics *Metrics
	sem     chan struct{}
	maxWait time.Duration

	pollEvery time.Duration
	// now and sleep are injectable so tests drive the gate on a fake clock
	// instead of actually waiting twenty seconds.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewGate builds admission control. Non-positive values take defaults:
// 2 concurrent, 60 seconds of waiting.
func NewGate(metrics *Metrics, concurrency int, maxWait time.Duration) *Gate {
	if concurrency <= 0 {
		concurrency = 2
	}
	if maxWait <= 0 {
		maxWait = 60 * time.Second
	}
	return &Gate{
		metrics:   metrics,
		sem:       make(chan struct{}, concurrency),
		maxWait:   maxWait,
		pollEvery: 500 * time.Millisecond,
		now:       time.Now,
		// sleepCtx is shared with the callback poster.
		sleep: sleepCtx,
	}
}

// GateBusyRunningReqs is how many in-flight LLM generations count as "the box
// is working". Live calls generate few concurrent requests — a turn takes about
// a second and a caller speaks every fifteen or so, so even sixty calls sit
// around five in flight. Anything well above that is a real burst.
const GateBusyRunningReqs = 8

// busy reports whether the box is currently too loaded to take background work.
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
// The thresholds are deliberately generous. Refusing whenever a single call is
// live would mean never running background work on a busy fleet, and work that
// never runs is worse than work that costs a few milliseconds of someone
// else's TTFT.
func (g *Gate) busy() bool {
	if g == nil || g.metrics == nil {
		return false
	}
	snap := g.metrics.Snapshot()
	if snap.SGLang.OK {
		if snap.SGLang.QueuedReqs > 0 {
			return true
		}
		if snap.SGLang.RunningReqs >= GateBusyRunningReqs {
			return true
		}
	}
	if snap.Capacity.MaxGPUCalls > 0 {
		if snap.Capacity.GPUCost*2 >= float64(snap.Capacity.MaxGPUCalls) {
			return true
		}
	}
	return false
}

// waitForQuiet blocks until the box is idle enough, the deadline passes, or ctx
// ends. It reports how long it waited and whether it gave up waiting.
func (g *Gate) waitForQuiet(ctx context.Context) (time.Duration, bool) {
	start := g.now()
	for {
		if !g.busy() {
			return g.now().Sub(start), false
		}
		if g.now().Sub(start) >= g.maxWait {
			return g.now().Sub(start), true
		}
		if err := g.sleep(ctx, g.pollEvery); err != nil {
			return g.now().Sub(start), true
		}
	}
}

// Run takes a slot, waits for quiet, then calls fn.
//
// Returns how long it waited and whether it gave up waiting, alongside fn's
// error. Both numbers are worth surfacing to the caller: a deferred that is
// regularly true means background work is competing with calls, and that is
// invisible from the other side of the API otherwise.
func (g *Gate) Run(ctx context.Context, fn func(context.Context) error) (waited time.Duration, deferred bool, err error) {
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
	waited, deferred = g.waitForQuiet(ctx)
	if ctx.Err() != nil {
		return waited, deferred, ctx.Err()
	}
	return waited, deferred, fn(ctx)
}
