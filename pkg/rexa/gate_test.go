package rexa

import (
	"context"
	"testing"
	"time"
)

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

func sessionN(i int) string { return "sess-" + string(rune('a'+i)) }

// run is the common shape: call the gate and record whether fn ran.
func run(g *Gate) (waited time.Duration, deferred bool, ran bool) {
	waited, deferred, _ = g.Run(context.Background(), func(context.Context) error {
		ran = true
		return nil
	})
	return waited, deferred, ran
}

func TestGateRunsImmediatelyOnAQuietBox(t *testing.T) {
	waited, deferred, ran := run(newTestGate(NewMetrics(20), 1, 60*time.Second))
	if !ran {
		t.Fatal("work did not run")
	}
	if waited != 0 || deferred {
		t.Errorf("a quiet box should not have waited: waited=%v deferred=%v", waited, deferred)
	}
}

// The whole point of the gate: a box carrying calls must not have a large
// prefill dropped on it the moment the platform asks for one.
func TestGateWaitsWhileCallsAreOnTheGPU(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < 6; i++ { // past half the ceiling
		if !m.TryReserve(sessionN(i)) {
			t.Fatalf("could not reserve session %d", i)
		}
	}
	waited, deferred, ran := run(newTestGate(m, 1, 60*time.Second))
	if waited < 60*time.Second {
		t.Errorf("expected to wait out the full deadline on a busy box, waited %v", waited)
	}
	if !deferred {
		t.Error("a run that waited out the deadline must report deferred")
	}
	// It still ran: work that never happens is a broken feature.
	if !ran {
		t.Error("work must run anyway once the wait expires")
	}
}

// A box that frees up mid-wait should proceed at once rather than serving out
// the whole deadline.
func TestGateProceedsAsSoonAsTheBoxGoesQuiet(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < 6; i++ {
		m.TryReserve(sessionN(i))
	}
	g := newTestGate(m, 1, 60*time.Second)

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

	waited, deferred, ran := run(g)
	if !ran {
		t.Fatal("work did not run")
	}
	if deferred {
		t.Error("should not be deferred when the box went quiet in time")
	}
	if waited >= 60*time.Second {
		t.Errorf("should have stopped waiting early, waited %v", waited)
	}
}

// Queued LLM requests mean a live caller is already waiting for a slot.
func TestGateTreatsAQueuedLLMAsBusy(t *testing.T) {
	m := NewMetrics(100) // nowhere near the call ceiling
	m.setSGLang(SGLangSnapshot{OK: true, QueuedReqs: 3, at: time.Now()})
	if !newTestGate(m, 1, 5*time.Second).busy() {
		t.Error("a non-zero LLM queue must count as busy")
	}
}

// The queue almost never moves on this hardware: measured at 100 concurrent
// requests, num_queue_reqs stayed 0 while num_running_reqs sat at ~105. If
// running requests did not also count as busy, the gate would be inert exactly
// when it is needed.
func TestGateTreatsRunningRequestsAsBusyEvenWithAnEmptyQueue(t *testing.T) {
	m := NewMetrics(100)
	m.setSGLang(SGLangSnapshot{OK: true, QueuedReqs: 0, RunningReqs: 105, at: time.Now()})
	if !newTestGate(m, 1, 5*time.Second).busy() {
		t.Error("a busy LLM with an empty queue must still count as busy")
	}
}

func TestGateIgnoresATrickleOfLLMTraffic(t *testing.T) {
	m := NewMetrics(100)
	// A handful in flight is what normal call traffic looks like.
	m.setSGLang(SGLangSnapshot{OK: true, RunningReqs: GateBusyRunningReqs - 1, at: time.Now()})
	if newTestGate(m, 1, 5*time.Second).busy() {
		t.Error("ordinary call traffic must not block platform requests forever")
	}
}

// The slot is taken BEFORE the wait. Taking it after would let an unbounded
// number of requests pile up in the gate and stampede the instant the box went
// quiet, which is the opposite of the intent.
func TestGateCapsConcurrency(t *testing.T) {
	g := newTestGate(NewMetrics(100), 1, time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _, _ = g.Run(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	// A second caller must not get in while the first holds the only slot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := g.Run(ctx, func(context.Context) error {
		t.Error("a second request ran while the only slot was held")
		return nil
	}); err == nil {
		t.Error("expected the second request to be turned away, not admitted")
	}
	close(release)
}
