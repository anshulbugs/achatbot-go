package rexa

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// Capacity + bottleneck telemetry for the /health endpoint and the live
// dashboard.
//
// Every number here comes from IN-PROCESS counters. The health endpoint must
// never probe SGLang, Parakeet or Kokoro to answer: the platform's load
// balancer hits /health every 5 s across the whole fleet, and a probe that
// fanned out to downstream services would take the agent out of rotation
// whenever one of them was merely slow — turning a latency blip into an
// outage. The Go server already sees every call and times every downstream
// request, so it can answer honestly without asking anyone.
//
// What the 61-call load test established, and why the shape is what it is
// (deploy/loadtest/README.md):
//
//   - 60 concurrent agent sessions is the safe operating point: p95 1628 ms,
//     zero dropped audio writes. 80 degrades (p95 4348 ms), 100 breaks
//     (p95 6244 ms, 234 dropped writes).
//   - p50 is useless as a health signal. At 100 agents the median still read
//     1112 ms while callers heard multi-second hangs. Everything here is p95.
//   - At 60 agents all three tiers sit at 77-96% utilisation simultaneously,
//     so there is rarely one culprit — the stack runs out everywhere at once.
//     The dashboard shows all three rather than nominating a winner.

// Tier names. ASR and STT are the same tier (Parakeet); there are three
// downstream services, not four.
const (
	TierLLM = "llm"
	TierASR = "asr"
	TierTTS = "tts"
)

// Tier health states.
const (
	TierOK        = "ok"
	TierDegraded  = "degraded"
	TierSaturated = "saturated"
	// TierUnknown is reported until a tier has enough samples for a
	// meaningful p95. A cold server must not claim a tier is healthy on the
	// strength of two requests, nor saturated because the first one was slow.
	TierUnknown = "unknown"
)

// latencyWindowSize is how many recent samples a tier's p95 is computed over.
//
// At the measured turn rate (one turn per 12.4 s per agent) sixty concurrent
// calls produce roughly five samples a second, so 256 samples is about a
// 50-second window — long enough to smooth a single slow request, short enough
// to react before a caller gives up.
const latencyWindowSize = 256

// minSamplesForState is the sample count below which a tier reports
// TierUnknown rather than guessing.
const minSamplesForState = 20

// TierThresholds are the p95 latencies at which a tier is considered degraded
// or saturated, in milliseconds.
//
// IMPORTANT: these defaults are calibration starting points, NOT measured
// values. The load test measured per-tier THROUGHPUT (LLM 29 req/s, TTS 25.7,
// ASR 26-36) but never recorded per-tier p95 latency, so there is no measured
// number to put here. They are derived from the turn budget: at the safe
// operating point a whole turn is ~1628 ms p95, of which ~600 ms is the VAD
// stop_secs the caller pays before we do anything, leaving ~1000 ms across the
// three tiers. Recalibrate from a real ramp before trusting them to turn away
// traffic.
type TierThresholds struct {
	DegradedMs  int
	SaturatedMs int
}

// DefaultThresholds returns the starting thresholds per tier.
func DefaultThresholds(tier string) TierThresholds {
	switch tier {
	case TierASR:
		// Transcription of one utterance; the shortest of the three.
		return TierThresholds{DegradedMs: 400, SaturatedMs: 900}
	case TierLLM:
		// Prefill + decode of a whole reply, and the largest share of the
		// budget.
		return TierThresholds{DegradedMs: 900, SaturatedMs: 2000}
	case TierTTS:
		// One request per sentence, so a four-sentence reply is four requests;
		// this is the per-request figure, not the per-reply total.
		return TierThresholds{DegradedMs: 700, SaturatedMs: 1600}
	default:
		return TierThresholds{DegradedMs: 1000, SaturatedMs: 2500}
	}
}

// tierWindow is a fixed-size ring of recent latency samples for one tier.
type tierWindow struct {
	samples [latencyWindowSize]float64
	next    int
	filled  int
	th      TierThresholds
}

func (w *tierWindow) add(ms float64) {
	w.samples[w.next] = ms
	w.next = (w.next + 1) % latencyWindowSize
	if w.filled < latencyWindowSize {
		w.filled++
	}
}

// p95 returns the 95th percentile of the window, or 0 when empty.
//
// Sorting a copy on every read is fine because reads are rare — a health probe
// every 5 s and a dashboard poll every second or two — while writes are hot.
// Paying on the rare path keeps the call path free of bookkeeping.
func (w *tierWindow) p95() float64 {
	if w.filled == 0 {
		return 0
	}
	buf := make([]float64, w.filled)
	copy(buf, w.samples[:w.filled])
	sort.Float64s(buf)
	idx := (w.filled * 95) / 100
	if idx >= w.filled {
		idx = w.filled - 1
	}
	return buf[idx]
}

func (w *tierWindow) state() string {
	if w.filled < minSamplesForState {
		return TierUnknown
	}
	switch p := w.p95(); {
	case p >= float64(w.th.SaturatedMs):
		return TierSaturated
	case p >= float64(w.th.DegradedMs):
		return TierDegraded
	default:
		return TierOK
	}
}

// Metrics is the live view of what this agent is doing and how well.
//
// Safe for concurrent use: counters move on call goroutines, latency samples
// arrive on HTTP transport goroutines, and snapshots are read by the health
// handler and the dashboard.
type Metrics struct {
	mu sync.RWMutex

	// onGPU counts calls currently holding a VAD/ASR/TTS pipeline.
	onGPU int
	// voicemail counts calls playing a pre-rendered announcement to an
	// answering machine. These hold NO pool slots — once AMD returns a machine
	// verdict the pipeline is released and the message is played from cache —
	// so they must not count against GPU capacity. A campaign hitting 40%
	// answering machines has far more real headroom than its call count
	// suggests.
	voicemail int

	// maxGPUCalls is the concurrency ceiling. Configured, never derived: 61
	// came from one ramp on one GPU layout with one prompt size, and has to be
	// remeasured whenever the model, prompt or hardware changes.
	maxGPUCalls int

	tiers map[string]*tierWindow

	// draining is set during shutdown so the platform stops routing new calls
	// here while those already running finish.
	draining bool

	// totals are cumulative for the dashboard.
	totalCalls     int64
	totalVoicemail int64
	totalRejected  int64
}

// NewMetrics builds a registry with the given GPU-call ceiling.
func NewMetrics(maxGPUCalls int) *Metrics {
	m := &Metrics{maxGPUCalls: maxGPUCalls, tiers: map[string]*tierWindow{}}
	for _, t := range []string{TierLLM, TierASR, TierTTS} {
		m.tiers[t] = &tierWindow{th: DefaultThresholds(t)}
	}
	return m
}

// SetThresholds overrides a tier's degraded/saturated latencies.
func (m *Metrics) SetThresholds(tier string, th TierThresholds) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.tiers[tier]; ok {
		w.th = th
	}
}

// CallStarted records a call taking a pipeline. Pair with CallEnded.
func (m *Metrics) CallStarted() {
	m.mu.Lock()
	m.onGPU++
	m.totalCalls++
	m.mu.Unlock()
}

// CallEnded releases a pipeline call.
func (m *Metrics) CallEnded() {
	m.mu.Lock()
	if m.onGPU > 0 {
		m.onGPU--
	}
	m.mu.Unlock()
}

// VoicemailStarted records a call that reached an answering machine and is
// playing a cached announcement without a pipeline.
func (m *Metrics) VoicemailStarted() {
	m.mu.Lock()
	m.voicemail++
	m.totalVoicemail++
	m.mu.Unlock()
}

// VoicemailEnded releases a voicemail call.
func (m *Metrics) VoicemailEnded() {
	m.mu.Lock()
	if m.voicemail > 0 {
		m.voicemail--
	}
	m.mu.Unlock()
}

// RejectedAtCapacity counts a dispatch we turned away.
func (m *Metrics) RejectedAtCapacity() {
	m.mu.Lock()
	m.totalRejected++
	m.mu.Unlock()
}

// Observe records one downstream request's latency against a tier.
func (m *Metrics) Observe(tier string, d time.Duration) {
	m.mu.Lock()
	if w, ok := m.tiers[tier]; ok {
		w.add(float64(d.Milliseconds()))
	}
	m.mu.Unlock()
}

// SetDraining takes the agent out of rotation without affecting live calls.
func (m *Metrics) SetDraining(v bool) {
	m.mu.Lock()
	m.draining = v
	m.mu.Unlock()
}

// Accepting reports whether the platform should send more calls.
//
// False when draining, when the GPU-call ceiling is reached, or when any tier
// is saturated. The tier condition is what makes this more than a counter: the
// ceiling was measured under one prompt size and one model, and a heavier
// workload can exhaust a tier well below 61 calls.
//
// Note the feedback loop this creates — refusing calls lowers load, which
// lowers p95, which resumes acceptance. The 256-sample (~50 s) window is what
// damps it; a shorter window would oscillate.
func (m *Metrics) Accepting() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.acceptingLocked()
}

func (m *Metrics) acceptingLocked() bool {
	if m.draining {
		return false
	}
	// A ceiling of zero or less means UNLIMITED, not "refuse everything".
	// Getting this backwards makes an unconfigured registry reject every
	// dispatch while reporting itself healthy — which is exactly what the
	// demo path and the tests would get by default.
	if m.maxGPUCalls > 0 && m.onGPU >= m.maxGPUCalls {
		return false
	}
	for _, w := range m.tiers {
		if w.state() == TierSaturated {
			return false
		}
	}
	return true
}

// TierSnapshot is one tier's public view.
type TierSnapshot struct {
	P95Ms int    `json:"p95_ms"`
	State string `json:"state"`
	// Samples lets a reader tell "healthy" from "no traffic yet".
	Samples int `json:"samples"`
}

// CallsSnapshot separates pipeline calls from announcement-only calls.
type CallsSnapshot struct {
	Total     int `json:"total"`
	OnGPU     int `json:"on_gpu"`
	Voicemail int `json:"voicemail"`
}

// CapacitySnapshot is the headroom view.
type CapacitySnapshot struct {
	MaxGPUCalls int `json:"max_gpu_calls"`
	// Headroom is the fraction of the GPU-call ceiling still free, 0..1.
	Headroom float64 `json:"headroom"`
}

// TotalsSnapshot is cumulative since process start, for the dashboard.
type TotalsSnapshot struct {
	Calls     int64 `json:"calls"`
	Voicemail int64 `json:"voicemail"`
	Rejected  int64 `json:"rejected"`
}

// HealthSnapshot is the body of GET /health.
//
// `status` is the only field the platform's v1 contract knows about. Its Zod
// schema is a plain object, which strips unknown keys rather than rejecting
// them, so everything below is additive and safe to ship before the platform
// reads any of it.
type HealthSnapshot struct {
	Status    bool                    `json:"status"`
	Accepting bool                    `json:"accepting"`
	Calls     CallsSnapshot           `json:"calls"`
	Capacity  CapacitySnapshot        `json:"capacity"`
	Tiers     map[string]TierSnapshot `json:"tiers"`
	Totals    TotalsSnapshot          `json:"totals"`
}

// Snapshot builds the current view.
func (m *Metrics) Snapshot() HealthSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tiers := make(map[string]TierSnapshot, len(m.tiers))
	for name, w := range m.tiers {
		tiers[name] = TierSnapshot{
			P95Ms:   int(w.p95()),
			State:   w.state(),
			Samples: w.filled,
		}
	}
	headroom := 0.0
	if m.maxGPUCalls > 0 {
		headroom = float64(m.maxGPUCalls-m.onGPU) / float64(m.maxGPUCalls)
		if headroom < 0 {
			headroom = 0
		}
	}
	return HealthSnapshot{
		// status stays true while draining: the process is alive and finishing
		// its calls. `accepting` is the field that says stop sending work.
		Status:    !m.draining,
		Accepting: m.acceptingLocked(),
		Calls: CallsSnapshot{
			Total:     m.onGPU + m.voicemail,
			OnGPU:     m.onGPU,
			Voicemail: m.voicemail,
		},
		Capacity: CapacitySnapshot{MaxGPUCalls: m.maxGPUCalls, Headroom: headroom},
		Tiers:    tiers,
		Totals: TotalsSnapshot{
			Calls: m.totalCalls, Voicemail: m.totalVoicemail, Rejected: m.totalRejected,
		},
	}
}

// Tripper wraps an http.RoundTripper to time every request against a tier.
//
// Used as the Transport on the ASR/TTS/LLM HTTP clients, which measures true
// wire latency — what the caller actually waits for — without any of the
// providers knowing metrics exist.
func (m *Metrics) Tripper(tier string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &timingTripper{tier: tier, base: base, m: m}
}

type timingTripper struct {
	tier string
	base http.RoundTripper
	m    *Metrics
}

func (t *timingTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	// Time failures too. A connection refused that returns in 1 ms would
	// otherwise look like the fastest tier on the box, hiding an outage behind
	// an excellent p95.
	t.m.Observe(t.tier, time.Since(start))
	return resp, err
}
