package rexa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The behaviour this file protects, in the operator's words: if the platform
// sends calls whose prompts are entirely different and the KV cache cannot cope,
// /health must turn accepting=false immediately, even if that drops the claimed
// concurrency from 61 to 6.

func TestFirstTurnGateTripsWithCapacityToSpare(t *testing.T) {
	m := NewMetrics(61)
	m.SetFirstTurnThresholds(FirstTurnThresholds{
		SaturatedMs: 4000, CriticalMs: 8000, Cooldown: time.Minute,
	})
	// Ten calls in flight against a ceiling of 61: every count-based condition
	// says there is ample room.
	for i := 0; i < 10; i++ {
		if !m.TryReserve(string(rune('a' + i))) {
			t.Fatalf("reservation %d refused with 10 calls against a ceiling of 61", i)
		}
	}
	if !m.Accepting() {
		t.Fatal("not accepting at 10/61 before any latency was observed")
	}

	// One first turn at the measured bad-case latency.
	m.ObserveLLMTurn(9903*time.Millisecond, 1)

	if m.Accepting() {
		t.Fatal("still accepting after a 9903ms first turn — the pooled tier " +
			"windows hide exactly this, which is why the gate exists")
	}
	if got := m.Snapshot().FirstTurn; !got.Blocked || got.Trips != 1 {
		t.Fatalf("first_turn = %+v, want blocked with 1 trip", got)
	}
}

func TestFirstTurnGateIgnoresLaterTurns(t *testing.T) {
	m := NewMetrics(61)
	m.SetFirstTurnThresholds(FirstTurnThresholds{
		SaturatedMs: 4000, CriticalMs: 8000, Cooldown: time.Minute,
	})
	// Turn 8 is cheap by construction — it reuses a warm prefix — so a slow one
	// means something different and must not be read through first-turn
	// thresholds calibrated on cold prefills.
	for i := 0; i < 20; i++ {
		m.ObserveLLMTurn(9903*time.Millisecond, 8)
	}
	// The pooled LLM tier may well refuse on its own here, and should — this is
	// only about which gate owns the decision.
	if s := m.Snapshot().FirstTurn; s.Blocked || s.Trips != 0 || s.Samples != 0 {
		t.Fatalf("first_turn = %+v, want untouched by turn-8 samples", s)
	}
	// They must still reach the LLM tier, which is what late-turn latency is
	// for.
	if s := m.Snapshot().Tiers[TierLLM]; s.Samples != 20 {
		t.Fatalf("llm tier samples = %d, want 20", s.Samples)
	}
}

func TestFirstTurnGateTripsOnSustainedP95BelowCritical(t *testing.T) {
	m := NewMetrics(61)
	m.SetFirstTurnThresholds(FirstTurnThresholds{
		SaturatedMs: 4000, CriticalMs: 20000, Cooldown: time.Minute,
	})
	// Each sample is well under critical, so only the percentile can catch this.
	for i := 0; i < firstTurnMinSamples; i++ {
		m.ObserveLLMTurn(5000*time.Millisecond, 1)
	}
	if m.Accepting() {
		t.Fatal("sustained 5000ms first turns did not trip the gate")
	}
}

func TestFirstTurnGateReopensAndDoesNotDeadlock(t *testing.T) {
	m := NewMetrics(61)
	m.SetFirstTurnThresholds(FirstTurnThresholds{
		SaturatedMs: 4000, CriticalMs: 8000, Cooldown: 40 * time.Millisecond,
	})
	m.ObserveLLMTurn(9903*time.Millisecond, 1)
	if m.Accepting() {
		t.Fatal("gate did not shut")
	}

	// The window is fed only by NEW calls, so a gate that stayed shut until the
	// numbers improved would cut off the samples that could prove recovery.
	// After the cooldown it must reopen on its own, with no new data.
	time.Sleep(60 * time.Millisecond)
	if !m.Accepting() {
		t.Fatal("gate never reopened — with no calls admitted it can never " +
			"observe another first turn, so this is a permanent deadlock")
	}
	if s := m.Snapshot().FirstTurn; s.Samples != 0 {
		t.Fatalf("window kept %d pre-trip samples; recovery must be judged on "+
			"calls admitted after the trip", s.Samples)
	}

	// A healthy first turn after reopening keeps it open.
	m.ObserveLLMTurn(1200*time.Millisecond, 1)
	if !m.Accepting() {
		t.Fatal("a healthy first turn re-tripped the gate")
	}
}

func TestConnectionRefusedWhileFirstTurnBlocked(t *testing.T) {
	m := NewMetrics(61)
	m.SetFirstTurnThresholds(FirstTurnThresholds{CriticalMs: 8000, Cooldown: time.Minute})
	m.ObserveLLMTurn(9903*time.Millisecond, 1)
	// Admission must refuse too, not just the health hint: a platform that
	// dispatches anyway has to be told no at the door.
	if m.TryReserve("x") {
		t.Fatal("TryReserve admitted a call while the first-turn gate was shut")
	}
	if m.Snapshot().Totals.Rejected != 1 {
		t.Fatal("refusal was not counted")
	}
}

func TestSGLangScrapeParsesMetrics(t *testing.T) {
	body := `# HELP sglang:num_running_reqs The number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{model_name="google/gemma-4-E4B-it"} 7.0
sglang:num_queue_reqs{model_name="google/gemma-4-E4B-it"} 3.0
sglang:cache_hit_rate{model_name="google/gemma-4-E4B-it"} 0.82
sglang:token_usage{model_name="google/gemma-4-E4B-it"} 0.41
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Errorf("polled %q, want /metrics", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// Two replicas: counts must sum and rates must average, or two perfectly
	// cached replicas would report a 164% hit rate.
	got := pollAll(context.Background(), srv.Client(), []string{srv.URL + "/v1", srv.URL})
	if got.Replicas != 2 || got.RunningReqs != 14 || got.QueuedReqs != 6 {
		t.Fatalf("counts = %+v, want 2 replicas, 14 running, 6 queued", got)
	}
	if got.CacheHitRate != 0.82 || got.TokenUsage != 0.41 {
		t.Fatalf("rates = %v/%v, want 0.82/0.41", got.CacheHitRate, got.TokenUsage)
	}
	if !got.OK {
		t.Fatal("ok = false after a successful poll")
	}
}

func TestSGLangPollFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	got := pollAll(context.Background(), srv.Client(), []string{srv.URL})
	if got.OK || got.Replicas != 0 {
		t.Fatalf("a failing replica produced %+v, want a not-ok snapshot", got)
	}
}
