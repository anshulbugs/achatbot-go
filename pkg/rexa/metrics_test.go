package rexa

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAcceptingUntilCeiling(t *testing.T) {
	m := NewMetrics(3)
	for i := 0; i < 3; i++ {
		if !m.TryReserve(id(i)) {
			t.Fatalf("refused reservation %d of 3", i)
		}
	}
	if m.TryReserve("one-too-many") {
		t.Error("reserved past the ceiling")
	}
	m.Release(id(0))
	if !m.TryReserve("replacement") {
		t.Error("did not resume accepting after a call ended")
	}
}

func id(i int) string { return "call-" + strconv.Itoa(i) }

// The whole point of separating the two counters: a call that reached an
// answering machine has released its pipeline and must not consume capacity.
// A campaign with heavy voicemail hit rates otherwise throttles itself for no
// reason.
func TestVoicemailDoesNotConsumeGPUCapacity(t *testing.T) {
	m := NewMetrics(2)
	for i := 0; i < 10; i++ {
		m.Track(id(i))
		m.MarkVoicemail(id(i))
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
		m.TryReserve(id(i))
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
	m.TryReserve(id(1))
	m.TryReserve(id(2))
	if got := m.Snapshot().Capacity.Headroom; got != 0.5 {
		t.Errorf("headroom = %v, want 0.5", got)
	}
}

// Releasing an unknown id, or the same id twice, must be harmless: call.hangup
// can arrive more than once and the demo path never reserves at all.
func TestReleaseIsIdempotentAndSafeForUnknownIds(t *testing.T) {
	m := NewMetrics(4)
	m.Release("never-seen")
	m.TryReserve(id(1))
	m.Release(id(1))
	m.Release(id(1))
	s := m.Snapshot()
	if s.Calls.Total != 0 {
		t.Errorf("calls = %+v, want empty", s.Calls)
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
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				k := id(worker*1000 + j)
				m.TryReserve(k)
				m.MarkOnGPU(k)
				m.Observe(TierLLM, 10*time.Millisecond)
				m.Snapshot()
				m.Accepting()
				m.Release(k)
			}
		}(i)
	}
	wg.Wait()
	if s := m.Snapshot(); s.Calls.OnGPU != 0 {
		t.Errorf("onGPU = %d after balanced start/end", s.Calls.OnGPU)
	}
}

// THE BUG THIS MODEL EXISTS FOR. A dispatch is accepted while the phone is
// still ringing — the pipeline does not start until the call is answered, up
// to the 30s dial timeout later. Counting only live pipelines reports zero
// load for that whole window, so a caller dispatching against it can push
// unbounded calls before the first one registers.
func TestReservationsCountBeforeAnyPipelineStarts(t *testing.T) {
	m := NewMetrics(5)
	for i := 0; i < 5; i++ {
		if !m.TryReserve(id(i)) {
			t.Fatalf("refused reservation %d", i)
		}
	}
	// Nothing has answered; no pipeline exists. Capacity must still be spent.
	if s := m.Snapshot(); s.Calls.OnGPU != 0 || s.Calls.Reserved != 5 {
		t.Fatalf("calls = %+v, want 5 reserved / 0 on_gpu", s.Calls)
	}
	if m.TryReserve("sixth") {
		t.Error("accepted a 6th dispatch while 5 were still ringing")
	}
}

// Check-and-reserve must be one atomic step. Split into two, concurrent
// dispatches at the ceiling all observe room and all take it.
func TestConcurrentReserveNeverExceedsCeiling(t *testing.T) {
	const ceiling = 20
	m := NewMetrics(ceiling)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if m.TryReserve(id(n)) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if granted != ceiling {
		t.Errorf("granted %d reservations against a ceiling of %d", granted, ceiling)
	}
	if got := m.Snapshot().Calls.Total; got != ceiling {
		t.Errorf("in flight = %d, want %d", got, ceiling)
	}
}

// A leaked reservation permanently removes capacity, which is worse than the
// lag the model replaced. The reaper is what makes reservations safe.
func TestReaperReleasesStaleReservations(t *testing.T) {
	m := NewMetrics(2)
	m.TryReserve("stuck")
	m.TryReserve("also-stuck")
	if m.TryReserve("blocked") {
		t.Fatal("expected to be at the ceiling")
	}
	// Age both past the TTL.
	m.mu.Lock()
	for _, c := range m.calls {
		c.dispatchedAt = time.Now().Add(-reservationTTL - time.Second)
	}
	m.mu.Unlock()

	if n := m.ReapStale(); n != 2 {
		t.Errorf("reaped %d, want 2", n)
	}
	if !m.TryReserve("now-fine") {
		t.Error("capacity was not recovered by the reaper")
	}
	if got := m.Snapshot().Totals.Reaped; got != 2 {
		t.Errorf("reaped total = %d, want 2", got)
	}
}

// The reaper must not touch live pipelines or voicemail playback, however long
// they run — a 20-minute call is legitimate.
func TestReaperLeavesLiveCallsAlone(t *testing.T) {
	m := NewMetrics(10)
	m.TryReserve("talking")
	m.MarkOnGPU("talking")
	m.TryReserve("machine")
	m.MarkVoicemail("machine")
	m.mu.Lock()
	for _, c := range m.calls {
		c.dispatchedAt = time.Now().Add(-24 * time.Hour)
	}
	m.mu.Unlock()

	if n := m.ReapStale(); n != 0 {
		t.Errorf("reaped %d live calls", n)
	}
	if got := m.Snapshot().Calls.Total; got != 2 {
		t.Errorf("in flight = %d, want 2", got)
	}
}

// The absolute ceiling stands in for limits the counters cannot see — carrier
// channels, CPU for hundreds of media streams. No weighting may argue past it.
func TestMaxTotalCallsCapsEvenWhenGPUCostIsLow(t *testing.T) {
	m := NewMetrics(61)
	m.SetMaxTotalCalls(10)
	for i := 0; i < 10; i++ {
		m.TryReserve(id(i))
		m.MarkVoicemail(id(i)) // zero GPU cost
	}
	if s := m.Snapshot(); s.Capacity.GPUCost != 0 {
		t.Fatalf("gpu cost = %v, want 0 for all-voicemail", s.Capacity.GPUCost)
	}
	if m.TryReserve("eleventh") {
		t.Error("total ceiling was exceeded because GPU cost looked free")
	}
}

// Over-subscription: at a 10% answer rate a reservation costs a tenth of a
// slot, so ~10x the pipeline ceiling can be in flight. This is the arithmetic
// behind "90% voicemail means we can run far more calls".
func TestHumanWeightOverSubscribes(t *testing.T) {
	m := NewMetrics(10)
	m.SetHumanWeight(0.1)
	granted := 0
	for i := 0; i < 500; i++ {
		if m.TryReserve(id(i)) {
			granted++
		}
	}
	if granted < 90 || granted > 110 {
		t.Errorf("granted %d at weight 0.1 against a ceiling of 10, want ~100", granted)
	}
}

// Full weight is the default, so the safe configuration is not one you have to
// remember to set.
func TestDefaultWeightIsFull(t *testing.T) {
	m := NewMetrics(5)
	granted := 0
	for i := 0; i < 50; i++ {
		if m.TryReserve(id(i)) {
			granted++
		}
	}
	if granted != 5 {
		t.Errorf("granted %d by default, want 5 — default must not over-subscribe", granted)
	}
	if got := m.Snapshot().Capacity.HumanWeight; got != 1.0 {
		t.Errorf("weight = %v, want 1.0", got)
	}
}

// Measured mode must be conservative before it has data, or a cold start
// over-subscribes on no evidence at all.
func TestMeasuredWeightFallsBackToFullWhenCold(t *testing.T) {
	m := NewMetrics(5)
	m.SetHumanWeight(0) // measured mode
	if got := m.Snapshot().Capacity.HumanWeight; got != 1.0 {
		t.Errorf("cold weight = %v, want 1.0", got)
	}
	granted := 0
	for i := 0; i < 50; i++ {
		if m.TryReserve(id(i)) {
			granted++
		}
	}
	if granted != 5 {
		t.Errorf("granted %d on a cold start, want 5", granted)
	}
}

// With history, measured mode tracks the real answer rate — and never drops
// below the floor, so a lucky streak cannot authorise an unbounded burst.
func TestMeasuredWeightTracksAnswerRateAndRespectsFloor(t *testing.T) {
	m := NewMetrics(100)
	m.SetHumanWeight(0)
	// 100 resolved calls, none of which reached a pipeline: a 0% answer rate.
	for i := 0; i < 100; i++ {
		k := id(i)
		m.Track(k)
		m.MarkVoicemail(k)
		m.Release(k)
	}
	s := m.Snapshot()
	if s.Measured.AnswerRate != 0 {
		t.Errorf("answer rate = %v, want 0", s.Measured.AnswerRate)
	}
	if s.Capacity.HumanWeight != m.minHumanWeight {
		t.Errorf("weight = %v, want the floor %v", s.Capacity.HumanWeight, m.minHumanWeight)
	}
}

func TestAnswerRateCountsOnlyPipelines(t *testing.T) {
	m := NewMetrics(100)
	for i := 0; i < 10; i++ {
		k := id(i)
		m.TryReserve(k)
		if i < 3 {
			m.MarkOnGPU(k) // a human answered
		} else if i < 6 {
			m.MarkVoicemail(k) // a machine
		} // the rest never answered at all
		m.Release(k)
	}
	if got := m.Snapshot().Measured.AnswerRate; got < 0.29 || got > 0.31 {
		t.Errorf("answer rate = %v, want 0.3", got)
	}
}

func TestRingTimeMeasured(t *testing.T) {
	m := NewMetrics(10)
	m.TryReserve("ringing")
	m.mu.Lock()
	m.calls["ringing"].dispatchedAt = time.Now().Add(-8 * time.Second)
	m.mu.Unlock()
	m.MarkAnswered("ringing")
	if got := m.Snapshot().Measured.RingMsP95; got < 7500 || got > 8500 {
		t.Errorf("ring p95 = %dms, want ~8000", got)
	}
}

// Admission reserves under session_id before the carrier names the call; every
// later transition knows only the call-control id.
func TestRekeyPreservesState(t *testing.T) {
	m := NewMetrics(10)
	m.TryReserve("session-abc")
	m.Rekey("session-abc", "v3:call-xyz")
	m.MarkOnGPU("v3:call-xyz")
	if s := m.Snapshot(); s.Calls.OnGPU != 1 || s.Calls.Total != 1 {
		t.Errorf("calls = %+v, want a single on_gpu call", s.Calls)
	}
	m.Release("v3:call-xyz")
	if got := m.Snapshot().Calls.Total; got != 0 {
		t.Errorf("in flight = %d after release, want 0", got)
	}
}

// The platform reuses one dispatch across retries; a retry must not consume a
// second slot.
func TestDuplicateReserveDoesNotDoubleCount(t *testing.T) {
	m := NewMetrics(2)
	m.TryReserve("same")
	m.TryReserve("same")
	if got := m.Snapshot().Calls.Total; got != 1 {
		t.Errorf("in flight = %d, want 1", got)
	}
}

// Measured mode charges a multiple of the recent mean, not the mean itself.
// Overshoot is bounded by (ceiling / weight) x the rate that materialises, so
// weighting by the average leaves nothing in hand when the next batch answers
// better than the last — and a call a human has answered cannot be refused.
func TestMeasuredWeightAppliesSafetyFactor(t *testing.T) {
	m := NewMetrics(100)
	m.SetHumanWeight(0)
	// 100 resolved calls, 20 of which reached a pipeline: a 20% answer rate.
	for i := 0; i < 100; i++ {
		k := id(i)
		m.TryReserve(k)
		if i < 20 {
			m.MarkOnGPU(k)
		} else {
			m.MarkVoicemail(k)
		}
		m.Release(k)
	}
	s := m.Snapshot()
	if got := s.Measured.AnswerRate; got < 0.19 || got > 0.21 {
		t.Fatalf("answer rate = %v, want 0.2", got)
	}
	// 0.2 measured x 2.0 factor = 0.4 charged.
	if got := s.Capacity.HumanWeight; got < 0.39 || got > 0.41 {
		t.Errorf("weight = %v, want ~0.4 (mean x safety factor)", got)
	}
}

// The charge must never exceed a full slot, or we refuse work the ceiling can
// genuinely serve.
func TestMeasuredWeightNeverExceedsOne(t *testing.T) {
	m := NewMetrics(100)
	m.SetHumanWeight(0)
	// Everyone answers: a 100% rate, which the safety factor would double.
	for i := 0; i < 100; i++ {
		k := id(i)
		m.TryReserve(k)
		m.MarkOnGPU(k)
		m.Release(k)
	}
	if got := m.Snapshot().Capacity.HumanWeight; got != 1.0 {
		t.Errorf("weight = %v, want it capped at 1.0", got)
	}
}
