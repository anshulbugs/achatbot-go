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
		// TIME TO FIRST TOKEN, pooled across all turns of all calls — not the
		// HTTP round trip. With SSE the round trip returns when the response
		// headers arrive, before the model has produced anything, so transport
		// timing reports single-digit milliseconds while the caller waits.
		// ObserveLLMTurn feeds this instead.
		//
		// Calibrated against the pooled figures from turnbench.py at 60
		// concurrent calls: 1725 ms p95 when prompts share a campaign prefix,
		// 6252 ms when every call carries a different prompt. These sit between
		// the two. They are deliberately looser than the first-turn gate, which
		// is the sharper instrument for the same failure.
		return TierThresholds{DegradedMs: 2500, SaturatedMs: 4500}
	case TierTTS:
		// One request per sentence, so a four-sentence reply is four requests;
		// this is the per-request figure, not the per-reply total.
		return TierThresholds{DegradedMs: 700, SaturatedMs: 1600}
	default:
		return TierThresholds{DegradedMs: 1000, SaturatedMs: 2500}
	}
}

// tierWindow is a fixed-size ring of recent latency samples for one tier.
//
// The capacity is per-window rather than a constant because the first-turn
// window measures one sample per CALL, not one per request: at the same traffic
// a 256-sample window there would span an hour and never react.
type tierWindow struct {
	samples    []float64
	next       int
	filled     int
	minSamples int
	th         TierThresholds
}

func newWindow(size, minSamples int, th TierThresholds) *tierWindow {
	return &tierWindow{samples: make([]float64, size), minSamples: minSamples, th: th}
}

func (w *tierWindow) add(ms float64) {
	if len(w.samples) == 0 {
		return
	}
	w.samples[w.next] = ms
	w.next = (w.next + 1) % len(w.samples)
	if w.filled < len(w.samples) {
		w.filled++
	}
}

// reset forgets every sample. Used when a gate trips, so the recovery decision
// is made on measurements taken AFTER the load was shed rather than on the ones
// that caused the trip.
func (w *tierWindow) reset() {
	w.next = 0
	w.filled = 0
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
	if w.filled < w.minSamples {
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

// First-turn backpressure.
//
// The tier windows above pool every LLM request together, and that pooling is
// exactly what hides the failure this gate exists to catch. Measured at 60
// concurrent calls with 3k-token prompts (deploy/loadtest/turnbench.py):
//
//	                      one campaign   a different prompt per call
//	overall TTFT p95         1725 ms              6252 ms
//	TURN 1 p95               1853 ms              9903 ms
//	turn 8 p95                727 ms              4035 ms
//
// Turn 1 is the only turn that pays a cold prefill, so it is where KV-cache
// pressure shows first and worst — and it is the turn the caller feels, the
// pause after they say "hello". Averaged in with cheap warm turns it drops
// below any threshold that would have fired.
//
// So first-turn TTFT is tracked on its own and can refuse traffic BY ITSELF,
// regardless of how few calls are in flight. The operating rule, stated by the
// operator and worth repeating because it inverts the usual instinct:
//
//	Serving 6 calls well beats accepting 61 and serving all of them badly.
//
// max_gpu_calls was measured under one prompt size and one degree of prefix
// sharing. A workload heavier than the one it was measured under invalidates
// it, and continuing to advertise it is not optimism, it is a false claim.
const (
	// firstTurnWindowSize is small because samples arrive once per call, not
	// once per request.
	firstTurnWindowSize = 32
	// firstTurnMinSamples is deliberately low. The bad case is not subtle —
	// 9903 ms against 2471 ms — so waiting for a statistically comfortable
	// sample count would mean accepting dozens more calls into a stack that has
	// already stopped coping.
	firstTurnMinSamples = 4
)

// FirstTurnThresholds configures the first-turn gate, in milliseconds.
//
// CALIBRATED, not guessed. Measured at 60 concurrent calls, 3k prompts, two
// replicas behind the prefix-hashing balancer (deploy/loadtest/calibrate.sh and
// scenarios.sh):
//
//	one campaign, contact block last     2286 ms   must NOT trip
//	12 independent campaigns             3550 ms   must NOT trip
//	30 independent campaigns             5400 ms   should trip
//	60 unique prompts, varying part first 5906 ms   must trip
//
// So the crossover sits between 3550 and 5400, and the defaults below put
// `saturated` in that gap with margin on the healthy side. Remeasure whenever
// prompt size, model or replica count changes — every number here is specific
// to all three.
type FirstTurnThresholds struct {
	// DegradedMs only colours the dashboard; it does not refuse traffic.
	DegradedMs int
	// SaturatedMs is the p95 at which the gate trips, once there are
	// firstTurnMinSamples to compute it from.
	SaturatedMs int
	// CriticalMs trips the gate on a SINGLE sample, with no minimum count.
	//
	// This is what makes the response immediate rather than statistical: ten
	// calls with ten unrelated prompts produce ten samples, and by the time a
	// p95 over four of them is meaningful the platform has had two more health
	// probes telling it to keep going. One first turn this slow is already a
	// caller sitting in silence; there is nothing to average.
	CriticalMs int
	// Cooldown is how long the gate stays shut after tripping.
	//
	// A duty cycle rather than a latch, and the distinction is load-bearing:
	// the window is fed only by NEW calls, so a gate that stayed shut until the
	// numbers improved would stop receiving the samples that could improve
	// them, and never reopen. Instead it shuts for the cooldown, reopens, and
	// re-measures on fresh calls — tripping again immediately if the workload
	// is still too heavy. Under sustained overload that settles into admitting
	// a trickle, which is the intended behaviour.
	Cooldown time.Duration
}

// DefaultFirstTurnThresholds returns the starting configuration.
func DefaultFirstTurnThresholds() FirstTurnThresholds {
	return FirstTurnThresholds{
		// Above the worst healthy reading (3550 ms at 12 campaigns), so the
		// dashboard is not permanently amber on traffic that is fine.
		DegradedMs: 3000,
		// In the gap between the hardest workload that must be served (3550 ms)
		// and the lightest that should be refused (5400 ms).
		SaturatedMs: 4500,
		// One first turn this slow is already a caller sitting in silence.
		// Above the p95 of every workload measured, so it fires on genuine
		// outliers rather than on a healthy tail.
		CriticalMs: 7000,
		Cooldown:   30 * time.Second,
	}
}

// Call lifecycle states. A call is in exactly one at a time.
//
// The distinction that matters for capacity is reserved-vs-on_gpu. A dispatch
// is accepted the moment the carrier takes the dial, but the pipeline does not
// start until the call is ANSWERED and the media stream connects — up to the
// 30-second dial timeout later. Counting only live pipelines therefore reports
// zero load for the entire ring period, and a caller dispatching against that
// number can push hundreds of calls before the first one registers.
//
// So a dispatched call reserves its slot immediately and is reclassified as we
// learn what it actually became.
const (
	// StateReserved: dispatch accepted, dialing or ringing. Counts against
	// capacity — we must assume it will need a pipeline until proven otherwise.
	StateReserved = "reserved"
	// StateOnGPU: pipeline running, holding VAD/ASR/TTS slots.
	StateOnGPU = "on_gpu"
	// StateVoicemail: a machine answered. With AMD enabled the pipeline never
	// starts — the greeting plays from the announcement cache and the verdict
	// arrives before runVoiceSession is reached — so these hold no pool slots
	// and are released from capacity.
	StateVoicemail = "voicemail"
)

// reservationTTL bounds how long a call may sit in StateReserved before the
// reaper force-releases it.
//
// This exists because a leaked reservation permanently reduces capacity: if a
// dispatch is accepted and no hangup ever arrives — carrier glitch, lost
// webhook, a process that missed the event — that slot is gone until restart.
// Leak enough and the agent silently stops accepting work while reporting
// itself healthy, which is strictly worse than the lag it was introduced to
// fix. Set above the 30 s dial timeout so a legitimately long ring is never
// reaped out from under a live call.
const reservationTTL = 75 * time.Second

// outcomeWindowSize is how many resolved calls the answer-rate estimate looks
// back over. Short enough to track the current campaign rather than a lifetime
// average, long enough not to swing on a handful of calls.
const outcomeWindowSize = 200

// liveCall is one in-flight call's state.
type liveCall struct {
	state        string
	dispatchedAt time.Time
	answeredAt   time.Time // zero until the carrier reports an answer
}

// Metrics is the live view of what this agent is doing and how well.
//
// Safe for concurrent use: state changes arrive on call and webhook
// goroutines, latency samples on HTTP transport goroutines, and snapshots are
// read by the health handler and the dashboard.
type Metrics struct {
	mu sync.RWMutex

	// calls is every in-flight call keyed by carrier call id (or session id
	// before one exists). Counts are DERIVED from this map rather than kept
	// alongside it — parallel counters and a map inevitably drift, and a
	// capacity counter that drifts high refuses work forever.
	calls map[string]*liveCall

	// maxGPUCalls is the pipeline ceiling. Configured, never derived: 61 came
	// from one ramp on one GPU layout with one prompt size.
	maxGPUCalls int
	// maxTotalCalls is the absolute in-flight ceiling regardless of what the
	// GPU-cost estimate allows. It stands in for the limits our counters
	// cannot see — the carrier's concurrent channel cap, CPU for hundreds of
	// media streams, TTS renders at dispatch time — and is the number no
	// amount of optimism may exceed. 0 = unlimited.
	maxTotalCalls int

	// humanWeight is the expected GPU cost of one reservation: the fraction of
	// dispatches that become live pipelines. At 1.0 every reservation costs a
	// full slot (safe, under-utilises). Lower values over-subscribe on the
	// basis that most calls reach an answering machine and never take a slot.
	//
	// Pinned at 1.0 until a real campaign supplies the measurement.
	humanWeight float64
	// minHumanWeight floors the weight when it is computed rather than pinned,
	// so a freak run of voicemails cannot authorise an unbounded dispatch
	// burst on the strength of a lucky sample.
	minHumanWeight float64
	// weightSafetyFactor multiplies the measured answer rate in measured mode,
	// so the charge reflects a plausible worst case rather than the recent
	// mean. See weightLocked.
	weightSafetyFactor float64

	tiers map[string]*tierWindow

	// firstTurn holds one TTFT sample per call: the wait between the caller
	// finishing their first utterance and the first token of our reply.
	firstTurn   *tierWindow
	firstTurnTh FirstTurnThresholds
	// firstTurnBlockUntil is when the gate reopens. Zero means open.
	firstTurnBlockUntil time.Time
	firstTurnTrips      int64

	// sglang is the last successful poll of the LLM server's own metrics.
	// Reported, never acted on — see sglang.go.
	sglang SGLangSnapshot

	draining bool

	// outcomes is a ring of recent resolutions (true = became a live
	// pipeline), for the measured answer rate.
	outcomes    [outcomeWindowSize]bool
	outcomeNext int
	outcomeLen  int

	// ringWindow is a ring of measured dial-to-answer times.
	ringWindow *tierWindow

	totalCalls     int64
	totalVoicemail int64
	totalRejected  int64
	totalReaped    int64
}

// NewMetrics builds a registry with the given GPU-call ceiling.
func NewMetrics(maxGPUCalls int) *Metrics {
	m := &Metrics{
		calls:       map[string]*liveCall{},
		maxGPUCalls: maxGPUCalls,
		tiers:       map[string]*tierWindow{},
		// Full weight by default: assume every dispatch will need a pipeline
		// until measurement says otherwise. Over-subscribing by default would
		// mean the safe configuration is the one you have to remember to set.
		humanWeight:        1.0,
		minHumanWeight:     0.15,
		weightSafetyFactor: 2.0,
		firstTurnTh:        DefaultFirstTurnThresholds(),
		ringWindow:         newWindow(latencyWindowSize, minSamplesForState, TierThresholds{}),
	}
	for _, t := range []string{TierLLM, TierASR, TierTTS} {
		m.tiers[t] = newWindow(latencyWindowSize, minSamplesForState, DefaultThresholds(t))
	}
	m.firstTurn = newWindow(firstTurnWindowSize, firstTurnMinSamples, TierThresholds{
		DegradedMs:  m.firstTurnTh.DegradedMs,
		SaturatedMs: m.firstTurnTh.SaturatedMs,
	})
	return m
}

// SetFirstTurnThresholds overrides the first-turn gate configuration. Zero
// fields keep the current value, so a partly-configured file cannot silently
// disable the gate.
func (m *Metrics) SetFirstTurnThresholds(th FirstTurnThresholds) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if th.DegradedMs > 0 {
		m.firstTurnTh.DegradedMs = th.DegradedMs
	}
	if th.SaturatedMs > 0 {
		m.firstTurnTh.SaturatedMs = th.SaturatedMs
	}
	if th.CriticalMs > 0 {
		m.firstTurnTh.CriticalMs = th.CriticalMs
	}
	if th.Cooldown > 0 {
		m.firstTurnTh.Cooldown = th.Cooldown
	}
	m.firstTurn.th = TierThresholds{
		DegradedMs:  m.firstTurnTh.DegradedMs,
		SaturatedMs: m.firstTurnTh.SaturatedMs,
	}
}

// ObserveLLMTurn records the time a caller waited for the first token of one
// reply. turn counts from 1.
//
// This is the honest measure of LLM latency for a streaming call, and it
// replaces transport timing for this tier: with SSE the HTTP round trip returns
// when the response HEADERS arrive, which happens before the model has produced
// anything, so a transport-timed LLM tier reports a few milliseconds while the
// caller waits ten seconds.
//
// Turn 1 additionally feeds the first-turn gate and can shut admission on its
// own.
func (m *Metrics) ObserveLLMTurn(ttft time.Duration, turn int) {
	ms := float64(ttft.Milliseconds())
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.tiers[TierLLM]; ok {
		w.add(ms)
	}
	if turn != 1 {
		return
	}
	m.firstTurn.add(ms)

	critical := m.firstTurnTh.CriticalMs > 0 && ms >= float64(m.firstTurnTh.CriticalMs)
	sustained := m.firstTurn.state() == TierSaturated
	if !critical && !sustained {
		return
	}
	m.firstTurnBlockUntil = time.Now().Add(m.firstTurnTh.Cooldown)
	m.firstTurnTrips++
	// Judge recovery on calls admitted after the trip, not on the ones that
	// caused it.
	m.firstTurn.reset()
}

// FirstTurnBlocked reports whether the first-turn gate is currently shut, and
// for how much longer. Exposed for logging: a trip is the single most useful
// thing to see in the log when the platform reports being throttled.
func (m *Metrics) FirstTurnBlocked() (bool, time.Duration) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.firstTurnBlockedLocked()
}

func (m *Metrics) firstTurnBlockedLocked() (bool, time.Duration) {
	if m.firstTurnBlockUntil.IsZero() {
		return false, 0
	}
	if d := time.Until(m.firstTurnBlockUntil); d > 0 {
		return true, d
	}
	return false, 0
}

// SetMaxTotalCalls sets the absolute in-flight ceiling. 0 = unlimited.
func (m *Metrics) SetMaxTotalCalls(n int) {
	m.mu.Lock()
	m.maxTotalCalls = n
	m.mu.Unlock()
}

// SetHumanWeight pins the expected GPU cost per reservation, 0 < w <= 1.
//
// Passing 0 switches to the MEASURED rate from recent outcomes, floored at
// minHumanWeight. Measured mode is only safe once a campaign has run: the
// estimator looks backwards, so a regime change — a different segment, a
// different hour — can leave hundreds already dispatched when the answer rate
// jumps.
func (m *Metrics) SetHumanWeight(w float64) {
	m.mu.Lock()
	if w > 1 {
		w = 1
	}
	m.humanWeight = w
	m.mu.Unlock()
}

// SetThresholds overrides a tier's degraded/saturated latencies.
func (m *Metrics) SetThresholds(tier string, th TierThresholds) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.tiers[tier]; ok {
		w.th = th
	}
}

// countsLocked derives the per-state counts. Callers must hold the lock.
func (m *Metrics) countsLocked() (reserved, onGPU, voicemail int) {
	for _, c := range m.calls {
		switch c.state {
		case StateReserved:
			reserved++
		case StateOnGPU:
			onGPU++
		case StateVoicemail:
			voicemail++
		}
	}
	return
}

// weightLocked is the current expected GPU cost of one reservation.
func (m *Metrics) weightLocked() float64 {
	if m.humanWeight > 0 {
		return m.humanWeight
	}
	// Measured mode: fall back to full weight until there is enough history
	// to estimate from, so a cold start is never optimistic.
	if m.outcomeLen < 20 {
		return 1.0
	}
	live := 0
	for i := 0; i < m.outcomeLen; i++ {
		if m.outcomes[i] {
			live++
		}
	}
	// Charge a MULTIPLE of the recent mean, not the mean itself.
	//
	// Overshoot is bounded by (ceiling / weight) x the answer rate that
	// actually materialises. Weighting by the recent mean assumes the next
	// batch behaves like the last one, and it is exactly when that assumption
	// breaks — a different segment, a different hour, a better list — that the
	// error is unrecoverable: a call a human has already answered cannot be
	// refused without hanging up on them.
	//
	// The factor is the answer to "how much better than recent average could
	// the next batch plausibly be?". At 2.0 a measured 10% is charged as 20%,
	// which halves the throughput a naive estimate would allow and keeps
	// on_gpu at or under the ceiling for any rate up to twice the mean.
	w := (float64(live) / float64(m.outcomeLen)) * m.weightSafetyFactor
	if w < m.minHumanWeight {
		w = m.minHumanWeight
	}
	// Never charge MORE than a full slot: above 1.0 we would refuse work the
	// ceiling can genuinely serve.
	if w > 1 {
		w = 1
	}
	return w
}

// TryReserve atomically checks capacity and, if there is room, reserves a slot
// for id. Returns false when the dispatch must be refused.
//
// Check and reserve are ONE operation under one lock. Splitting them lets two
// dispatches arriving together at the ceiling both observe room and both take
// it — a check-then-act race that puts you one over every time it happens, and
// more under load, which is exactly when it matters.
func (m *Metrics) TryReserve(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.acceptingLocked() {
		m.totalRejected++
		return false
	}
	// A repeated id is the platform retrying the same logical dispatch; it
	// must not consume a second slot.
	if _, exists := m.calls[id]; exists {
		return true
	}
	m.calls[id] = &liveCall{state: StateReserved, dispatchedAt: time.Now()}
	m.totalCalls++
	return true
}

// Track registers a call that bypasses admission but must still be counted.
//
// Inbound calls take this path: the carrier leg is already ringing with a
// human on it, so refusing costs a real answered call. Counting them anyway is
// what makes inbound load reduce the outbound allowance rather than silently
// exceeding the ceiling.
func (m *Metrics) Track(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.calls[id]; exists {
		return
	}
	m.calls[id] = &liveCall{state: StateReserved, dispatchedAt: time.Now()}
	m.totalCalls++
}

// Rekey moves a tracked call from one id to another, preserving its state and
// timings.
//
// Admission happens before the carrier has given us anything to call the call
// by, so the slot is reserved under the platform's session_id and re-keyed to
// the carrier's call-control id once Dial returns. Every later transition
// arrives from a carrier webhook and knows only that id.
func (m *Metrics) Rekey(oldID, newID string) {
	if oldID == newID {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.calls[oldID]
	if c == nil {
		return
	}
	delete(m.calls, oldID)
	m.calls[newID] = c
}

// MarkAnswered records the dial-to-answer time, for the ring-time estimate.
func (m *Metrics) MarkAnswered(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.calls[id]; c != nil && c.answeredAt.IsZero() {
		c.answeredAt = time.Now()
		if !c.dispatchedAt.IsZero() {
			m.ringWindow.add(float64(c.answeredAt.Sub(c.dispatchedAt).Milliseconds()))
		}
	}
}

// MarkOnGPU promotes a reservation to a live pipeline.
//
// Idempotent, and it does NOT create an entry for an unknown id: a pipeline
// with no reservation means the call arrived by a path that never reserved
// (the demo server), and inventing capacity for it here would let that path
// quietly consume the contract's ceiling.
func (m *Metrics) MarkOnGPU(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.calls[id]; c != nil {
		c.state = StateOnGPU
	}
}

// MarkVoicemail reclassifies a call as an answering machine, releasing its
// GPU capacity while keeping it counted against the total in-flight ceiling.
func (m *Metrics) MarkVoicemail(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.calls[id]; c != nil && c.state != StateVoicemail {
		c.state = StateVoicemail
		m.totalVoicemail++
	}
}

// Release ends a call and records what it became, for the answer-rate
// estimate. Safe to call more than once and for ids that were never tracked —
// call.hangup can arrive twice, and the demo path never reserves.
func (m *Metrics) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.calls[id]
	if c == nil {
		return
	}
	// Only calls that actually reached a pipeline count as "human" for the
	// weighting. A reservation that ended without answering is a no-answer or
	// a busy — it cost no GPU, so it belongs on the cheap side of the ratio.
	m.recordOutcomeLocked(c.state == StateOnGPU)
	delete(m.calls, id)
}

func (m *Metrics) recordOutcomeLocked(becameLive bool) {
	m.outcomes[m.outcomeNext] = becameLive
	m.outcomeNext = (m.outcomeNext + 1) % outcomeWindowSize
	if m.outcomeLen < outcomeWindowSize {
		m.outcomeLen++
	}
}

// ReapStale force-releases reservations that never resolved, returning how
// many it dropped.
//
// Without this a lost hangup webhook removes a slot permanently. Only
// StateReserved is reaped: a call on GPU is released by runVoiceSession's
// deferred call, which runs whatever happens, and a voicemail call is bounded
// by its own announcement.
func (m *Metrics) ReapStale() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-reservationTTL)
	n := 0
	for id, c := range m.calls {
		if c.state == StateReserved && c.dispatchedAt.Before(cutoff) {
			// Reaped reservations are NOT recorded as outcomes: we never
			// learned what they became, and feeding guesses to the answer-rate
			// estimator would bias the weighting on exactly the calls we
			// understand least.
			delete(m.calls, id)
			m.totalReaped++
			n++
		}
	}
	return n
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

	// The first-turn gate comes before every count, because it is the one
	// condition that must be able to refuse a workload the counters think there
	// is ample room for. Ten calls carrying ten unrelated prompts can exhaust
	// the KV cache while on_gpu reads 10 against a ceiling of 61; answering
	// "yes, send more" there is how a stack ends up serving sixty-one calls
	// badly instead of six well.
	if blocked, _ := m.firstTurnBlockedLocked(); blocked {
		return false
	}

	reserved, onGPU, voicemail := m.countsLocked()

	// The absolute ceiling comes first: it stands in for the limits our
	// counters cannot see (carrier channels, CPU for hundreds of media
	// streams) and no estimate may argue past it.
	if m.maxTotalCalls > 0 && reserved+onGPU+voicemail >= m.maxTotalCalls {
		return false
	}

	// A ceiling of zero or less means UNLIMITED, not "refuse everything".
	// Getting this backwards makes an unconfigured registry reject every
	// dispatch while reporting itself healthy — which is exactly what the
	// demo path and the tests would get by default.
	if m.maxGPUCalls > 0 && m.gpuCostLocked(reserved, onGPU) >= float64(m.maxGPUCalls) {
		return false
	}

	for _, w := range m.tiers {
		if w.state() == TierSaturated {
			return false
		}
	}
	return true
}

// gpuCostLocked is the expected pipeline load: every live pipeline at full
// cost, plus each reservation weighted by how likely it is to become one.
//
// Weighting is what lets a voicemail-heavy campaign run far more calls than
// the pipeline ceiling. At a 10% answer rate a reservation costs a tenth of a
// slot, so ~610 in-flight dispatches equal 61 pipelines — but the total
// ceiling still caps it, because that arithmetic ignores everything a
// voicemail call DOES cost.
func (m *Metrics) gpuCostLocked(reserved, onGPU int) float64 {
	return float64(onGPU) + float64(reserved)*m.weightLocked()
}

// TierSnapshot is one tier's public view.
type TierSnapshot struct {
	P95Ms int    `json:"p95_ms"`
	State string `json:"state"`
	// Samples lets a reader tell "healthy" from "no traffic yet".
	Samples int `json:"samples"`
}

// FirstTurnSnapshot is the state of the first-turn gate.
type FirstTurnSnapshot struct {
	// P95Ms is the 95th-percentile wait for the first token of the first reply
	// of a call — the pause the caller hears after saying hello.
	P95Ms   int    `json:"p95_ms"`
	State   string `json:"state"`
	Samples int    `json:"samples"`
	// Blocked is true while the gate is refusing admission on its own.
	Blocked bool `json:"blocked"`
	// BlockedForSecs is how much longer the cooldown runs.
	BlockedForSecs int `json:"blocked_for_secs"`
	// Trips is how many times the gate has shut since start. A climbing count
	// with calls still flowing means the workload is heavier than the
	// configured ceiling assumes — the prompts are not sharing prefixes.
	Trips int64 `json:"trips"`
}

// CallsSnapshot separates the three in-flight states.
type CallsSnapshot struct {
	Total int `json:"total"`
	// Reserved: dispatched and ringing. Counts against capacity because it
	// will need a pipeline unless it turns out to be a machine.
	Reserved  int `json:"reserved"`
	OnGPU     int `json:"on_gpu"`
	Voicemail int `json:"voicemail"`
}

// CapacitySnapshot is the headroom view.
type CapacitySnapshot struct {
	MaxGPUCalls   int `json:"max_gpu_calls"`
	MaxTotalCalls int `json:"max_total_calls"`
	// GPUCost is the weighted pipeline load: live pipelines plus reservations
	// discounted by how many are expected to become pipelines.
	GPUCost float64 `json:"gpu_cost"`
	// HumanWeight is the cost currently charged per reservation. 1.0 means
	// every dispatch is assumed to need a slot.
	HumanWeight float64 `json:"human_weight"`
	// Headroom is the fraction of the GPU-call ceiling still free, 0..1.
	Headroom float64 `json:"headroom"`
}

// MeasuredSnapshot is what the campaign is actually doing — the numbers you
// need before over-subscription can be tuned from data rather than guessed.
type MeasuredSnapshot struct {
	// AnswerRate is the fraction of resolved calls that became live
	// pipelines: humans who picked up. Machines, no-answers and busies are
	// all on the cheap side.
	AnswerRate float64 `json:"answer_rate"`
	// RingMsP95 is dial-to-answer time. Long rings are why reservations
	// dominate in-flight counts on a fresh campaign.
	RingMsP95 int `json:"ring_ms_p95"`
	Samples   int `json:"samples"`
}

// TotalsSnapshot is cumulative since process start, for the dashboard.
type TotalsSnapshot struct {
	Calls     int64 `json:"calls"`
	Voicemail int64 `json:"voicemail"`
	Rejected  int64 `json:"rejected"`
	// Reaped counts reservations force-released after never resolving. Any
	// non-zero value means hangup events are being missed; a persistently
	// climbing one means capacity would have leaked away without the reaper.
	Reaped int64 `json:"reaped"`
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
	Measured  MeasuredSnapshot        `json:"measured"`
	Tiers     map[string]TierSnapshot `json:"tiers"`
	FirstTurn FirstTurnSnapshot       `json:"first_turn"`
	SGLang    SGLangSnapshot          `json:"sglang"`
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

	reserved, onGPU, voicemail := m.countsLocked()
	cost := m.gpuCostLocked(reserved, onGPU)
	headroom := 0.0
	if m.maxGPUCalls > 0 {
		headroom = (float64(m.maxGPUCalls) - cost) / float64(m.maxGPUCalls)
		if headroom < 0 {
			headroom = 0
		}
	}

	// Answer rate over resolved calls. Reported separately from the weight in
	// use, so pinning the weight at 1.0 while watching the real rate is
	// exactly the calibration workflow.
	answerRate := 0.0
	if m.outcomeLen > 0 {
		live := 0
		for i := 0; i < m.outcomeLen; i++ {
			if m.outcomes[i] {
				live++
			}
		}
		answerRate = float64(live) / float64(m.outcomeLen)
	}

	sg := m.sglang
	if !sg.at.IsZero() {
		sg.AgeSecs = int(time.Since(sg.at).Round(time.Second) / time.Second)
	}

	blocked, blockedFor := m.firstTurnBlockedLocked()
	ftState := m.firstTurn.state()
	if blocked {
		// While the cooldown runs the window has just been reset, so its own
		// state reads "unknown". Report what the gate is DOING, not what the
		// empty window would say.
		ftState = TierSaturated
	}
	ft := FirstTurnSnapshot{
		P95Ms:          int(m.firstTurn.p95()),
		State:          ftState,
		Samples:        m.firstTurn.filled,
		Blocked:        blocked,
		BlockedForSecs: int(blockedFor.Round(time.Second) / time.Second),
		Trips:          m.firstTurnTrips,
	}

	return HealthSnapshot{
		// status stays true while draining: the process is alive and finishing
		// its calls. `accepting` is the field that says stop sending work.
		Status:    !m.draining,
		Accepting: m.acceptingLocked(),
		Calls: CallsSnapshot{
			Total:     reserved + onGPU + voicemail,
			Reserved:  reserved,
			OnGPU:     onGPU,
			Voicemail: voicemail,
		},
		Capacity: CapacitySnapshot{
			MaxGPUCalls:   m.maxGPUCalls,
			MaxTotalCalls: m.maxTotalCalls,
			GPUCost:       cost,
			HumanWeight:   m.weightLocked(),
			Headroom:      headroom,
		},
		Measured: MeasuredSnapshot{
			AnswerRate: answerRate,
			RingMsP95:  int(m.ringWindow.p95()),
			Samples:    m.outcomeLen,
		},
		Tiers:     tiers,
		FirstTurn: ft,
		SGLang:    sg,
		Totals: TotalsSnapshot{
			Calls: m.totalCalls, Voicemail: m.totalVoicemail,
			Rejected: m.totalRejected, Reaped: m.totalReaped,
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
