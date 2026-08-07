package rexa

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAcceptingUntilCeiling(t *testing.T) {
	m := NewMetrics(3)
	for i := 0; i < 3; i++ {
		if !m.Accepting() {
			t.Fatalf("stopped accepting at %d of 3", i)
		}
		m.CallStarted()
	}
	if m.Accepting() {
		t.Error("still accepting at the ceiling")
	}
	m.CallEnded()
	if !m.Accepting() {
		t.Error("did not resume accepting after a call ended")
	}
}

// The whole point of separating the two counters: a call that reached an
// answering machine has released its pipeline and must not consume capacity.
// A campaign with heavy voicemail hit rates otherwise throttles itself for no
// reason.
func TestVoicemailDoesNotConsumeGPUCapacity(t *testing.T) {
	m := NewMetrics(2)
	for i := 0; i < 10; i++ {
		m.VoicemailStarted()
	}
	if !m.Accepting() {
		t.Error("voicemail calls consumed GPU capacity")
	}
	s := m.Snapshot()
	if s.Calls.Voicemail != 10 || s.Calls.OnGPU != 0 || s.Calls.Total != 10 {
		t.Errorf("calls = %+v, want 10 voicemail / 0 gpu / 10 total", s.Calls)
	}
	if s.Capacity.Headroom != 1.0 {
		t.Errorf("headroom = %v, want 1.0", s.Capacity.Headroom)
	}
}

// A ceiling of zero means unlimited, not "refuse everything". An
// unconfigured registry that rejected every dispatch while reporting itself
// healthy would be the worst possible default.
func TestZeroCeilingMeansUnlimited(t *testing.T) {
	m := NewMetrics(0)
	for i := 0; i < 500; i++ {
		m.CallStarted()
	}
	if !m.Accepting() {
		t.Error("a zero ceiling refused calls; it must mean unlimited")
	}
	if got := m.Snapshot().Capacity.Headroom; got != 0 {
		t.Errorf("headroom = %v, want 0 when there is no ceiling to measure against", got)
	}
}

func TestDrainingStopsAcceptance(t *testing.T) {
	m := NewMetrics(10)
	m.SetDraining(true)
	if m.Accepting() {
		t.Error("accepting while draining")
	}
	// status stays true: the process is alive and finishing its calls. Only
	// `accepting` says stop sending work.
	if s := m.Snapshot(); s.Status {
		t.Error("status should report false once draining")
	}
}

// A cold server must not claim a tier is healthy on two samples, nor saturated
// because the first request was slow.
func TestTierUnknownUntilEnoughSamples(t *testing.T) {
	m := NewMetrics(10)
	m.Observe(TierLLM, 9*time.Second)
	if got := m.Snapshot().Tiers[TierLLM].State; got != TierUnknown {
		t.Errorf("state = %q after 1 sample, want %q", got, TierUnknown)
	}
	if !m.Accepting() {
		t.Error("one slow sample took the agent out of rotation")
	}
	for i := 0; i < minSamplesForState; i++ {
		m.Observe(TierLLM, 10*time.Millisecond)
	}
	if got := m.Snapshot().Tiers[TierLLM].State; got != TierOK {
		t.Errorf("state = %q once warm and fast, want ok", got)
	}
}

func TestTierStatesFromP95(t *testing.T) {
	th := DefaultThresholds(TierLLM)
	cases := []struct {
		name  string
		ms    int
		state string
	}{
		{"fast", 50, TierOK},
		{"degraded", th.DegradedMs + 50, TierDegraded},
		{"saturated", th.SaturatedMs + 50, TierSaturated},
	}
	for _, c := range cases {
		m := NewMetrics(10)
		for i := 0; i < minSamplesForState+5; i++ {
			m.Observe(TierLLM, time.Duration(c.ms)*time.Millisecond)
		}
		if got := m.Snapshot().Tiers[TierLLM].State; got != c.state {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.state)
		}
	}
}

// A saturated tier must stop acceptance even with GPU slots free: the 61-call
// ceiling was measured under one prompt size, and a heavier workload exhausts
// a tier well below it.
func TestSaturatedTierStopsAcceptance(t *testing.T) {
	m := NewMetrics(100)
	for i := 0; i < minSamplesForState+5; i++ {
		m.Observe(TierTTS, 5*time.Second)
	}
	if m.Accepting() {
		t.Error("accepting while TTS is saturated")
	}
	// Degraded alone must NOT stop acceptance — that would refuse traffic the
	// stack is still serving acceptably.
	m2 := NewMetrics(100)
	d := DefaultThresholds(TierTTS).DegradedMs + 10
	for i := 0; i < minSamplesForState+5; i++ {
		m2.Observe(TierTTS, time.Duration(d)*time.Millisecond)
	}
	if s := m2.Snapshot().Tiers[TierTTS].State; s != TierDegraded {
		t.Fatalf("state = %q, want degraded", s)
	}
	if !m2.Accepting() {
		t.Error("a merely degraded tier stopped acceptance")
	}
}

// p95, not p50 — the load test showed the median staying flat at 1112 ms while
// callers heard multi-second hangs.
func TestP95TracksTailNotMedian(t *testing.T) {
	m := NewMetrics(10)
	// 95 fast, 5 very slow: a median-based signal would call this healthy.
	for i := 0; i < 95; i++ {
		m.Observe(TierASR, 10*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		m.Observe(TierASR, 4*time.Second)
	}
	if got := m.Snapshot().Tiers[TierASR].P95Ms; got < 1000 {
		t.Errorf("p95 = %dms — the tail was smoothed away", got)
	}
}

// The window must forget: a tier that recovers has to report ok again, or the
// agent never comes back into rotation after one bad minute.
func TestWindowRecovers(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < latencyWindowSize; i++ {
		m.Observe(TierLLM, 5*time.Second)
	}
	if m.Snapshot().Tiers[TierLLM].State != TierSaturated {
		t.Fatal("expected saturated")
	}
	for i := 0; i < latencyWindowSize; i++ {
		m.Observe(TierLLM, 20*time.Millisecond)
	}
	if got := m.Snapshot().Tiers[TierLLM].State; got != TierOK {
		t.Errorf("state = %q after full recovery, want ok", got)
	}
}

func TestHeadroom(t *testing.T) {
	m := NewMetrics(4)
	m.CallStarted()
	m.CallStarted()
	if got := m.Snapshot().Capacity.Headroom; got != 0.5 {
		t.Errorf("headroom = %v, want 0.5", got)
	}
}

// Counters must never go negative if an End is called without a Start (a
// teardown path running twice), or headroom goes above 1 and the ceiling stops
// meaning anything.
func TestCountersFloorAtZero(t *testing.T) {
	m := NewMetrics(4)
	m.CallEnded()
	m.CallEnded()
	m.VoicemailEnded()
	s := m.Snapshot()
	if s.Calls.OnGPU != 0 || s.Calls.Voicemail != 0 {
		t.Errorf("calls went negative: %+v", s.Calls)
	}
	if s.Capacity.Headroom > 1.0 {
		t.Errorf("headroom = %v, above 1", s.Capacity.Headroom)
	}
}

// The transport must time real requests, including failures — a connection
// refused returning in 1ms would otherwise look like the fastest tier on the
// box and hide an outage behind an excellent p95.
func TestTripperTimesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := NewMetrics(10)
	c := &http.Client{Transport: m.Tripper(TierASR, nil)}
	for i := 0; i < minSamplesForState+2; i++ {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	snap := m.Snapshot().Tiers[TierASR]
	if snap.Samples < minSamplesForState {
		t.Fatalf("samples = %d", snap.Samples)
	}
	if snap.P95Ms < 25 {
		t.Errorf("p95 = %dms, want >=25 for a 30ms handler", snap.P95Ms)
	}
}

func TestTripperTimesFailures(t *testing.T) {
	m := NewMetrics(10)
	c := &http.Client{Transport: m.Tripper(TierLLM, nil)}
	// Nothing listening: RoundTrip errors fast and must still be recorded.
	if _, err := c.Get("http://127.0.0.1:1"); err == nil {
		t.Skip("expected a connection failure")
	}
	if got := m.Snapshot().Tiers[TierLLM].Samples; got != 1 {
		t.Errorf("samples = %d, want 1 — failures must be timed too", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	m := NewMetrics(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.CallStarted()
				m.Observe(TierLLM, 10*time.Millisecond)
				m.Snapshot()
				m.Accepting()
				m.CallEnded()
			}
		}()
	}
	wg.Wait()
	if s := m.Snapshot(); s.Calls.OnGPU != 0 {
		t.Errorf("onGPU = %d after balanced start/end", s.Calls.OnGPU)
	}
}
