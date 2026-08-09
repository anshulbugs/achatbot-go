package rexa

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SGLang's own view of KV-cache pressure, polled in the background.
//
// Everything else in this package infers load from latency. These numbers are a
// DIRECT read of the thing latency is a symptom of: how much of the cache the
// running requests are using, how many are waiting for a slot, and how often a
// new request finds its prefix already resident. When first-turn TTFT climbs,
// this is what says whether the cause is cache thrash (hit rate collapsing) or
// simply too many requests (queue growing) — a distinction that decides whether
// the fix is prompt layout or fewer calls.
//
// REPORTED, NOT ACTED ON. No threshold here refuses traffic. Latency is what
// the caller experiences and what the gates are calibrated against; these
// numbers have no calibrated crossover yet, and gating on an uncalibrated
// signal would refuse work for a reading nobody has correlated with a bad call.
// Watch them next to first_turn on the dashboard during a ramp, and if a
// crossover turns out to be reliable, promote it then.

// SGLangSnapshot is the last successful poll, merged across replicas.
type SGLangSnapshot struct {
	// OK is false when no replica has ever answered, which is also the state
	// when polling is simply not configured. Read it as "no data", not "bad".
	OK bool `json:"ok"`
	// Replicas is how many endpoints answered the most recent poll.
	Replicas int `json:"replicas"`
	// RunningReqs is generation slots in use across all replicas.
	RunningReqs float64 `json:"running_reqs"`
	// QueuedReqs is requests waiting for a slot. Sustained non-zero means the
	// LLM tier is the bottleneck, whatever the latency window says.
	QueuedReqs float64 `json:"queued_reqs"`
	// CacheHitRate is the mean prefix-cache hit rate, 0..1. This is the number
	// that collapses when every call carries an unrelated prompt.
	CacheHitRate float64 `json:"cache_hit_rate"`
	// TokenUsage is the fraction of the KV pool in use, 0..1.
	TokenUsage float64 `json:"token_usage"`
	// AgeSecs is how stale the reading is. A poller that has stopped answering
	// leaves the last good values in place, so age is the only way to tell a
	// current reading from a fossil.
	AgeSecs int `json:"age_secs"`

	at time.Time
}

// setSGLang stores a poll result.
func (m *Metrics) setSGLang(s SGLangSnapshot) {
	m.mu.Lock()
	m.sglang = s
	m.mu.Unlock()
}

// PollSGLang polls each URL's /metrics endpoint until ctx is cancelled,
// updating the snapshot. urls are SGLang base URLs (http://host:port), NOT the
// /v1 chat endpoint.
//
// Runs on its own goroutine on its own clock. It must never be driven from the
// health handler: /health is probed every 5 s fleet-wide, and a handler that
// fanned out to a downstream service would take the agent out of rotation
// whenever that service was merely slow — converting a latency blip into an
// outage.
func (m *Metrics) PollSGLang(ctx context.Context, urls []string, every time.Duration) {
	if len(urls) == 0 {
		return
	}
	if every <= 0 {
		every = 5 * time.Second
	}
	// A timeout well under the interval: a poll that outlives its own tick is
	// measuring a server too sick to report on, and stacking them up would make
	// this poller part of the problem.
	client := &http.Client{Timeout: 2 * time.Second}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		m.setSGLang(pollAll(ctx, client, urls))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func pollAll(ctx context.Context, client *http.Client, urls []string) SGLangSnapshot {
	out := SGLangSnapshot{at: time.Now()}
	var hitSum, usageSum float64
	for _, u := range urls {
		vals, err := scrape(ctx, client, u)
		if err != nil {
			continue
		}
		out.Replicas++
		out.RunningReqs += vals["sglang:num_running_reqs"]
		out.QueuedReqs += vals["sglang:num_queue_reqs"]
		hitSum += vals["sglang:cache_hit_rate"]
		usageSum += vals["sglang:token_usage"]
	}
	if out.Replicas == 0 {
		return SGLangSnapshot{at: out.at}
	}
	out.OK = true
	// Counts sum across replicas; rates average. Summing a hit rate would
	// report 2.0 for two perfectly-cached replicas.
	out.CacheHitRate = hitSum / float64(out.Replicas)
	out.TokenUsage = usageSum / float64(out.Replicas)
	// SGLang reports cache_hit_rate as a percentage in some builds and a
	// fraction in others. Normalise so the dashboard cannot show 4700%.
	if out.CacheHitRate > 1 {
		out.CacheHitRate /= 100
	}
	return out
}

// scrape reads one replica's Prometheus text exposition and returns the last
// value seen for each metric name, labels discarded.
//
// Labels are dropped deliberately: SGLang labels every series with the model
// name, and a single-model replica emits exactly one series per metric, so
// there is nothing to disambiguate. If a replica ever served two models this
// would silently report only the last one — which is why the name is the key
// and not something that pretends to be more.
func scrape(ctx context.Context, client *http.Client, base string) (map[string]float64, error) {
	url := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1") + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errStatus(resp.StatusCode)
	}
	vals := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	// Prometheus lines are short, but HELP text on a histogram can be long
	// enough to trip the 64KB default and abort the scan mid-file.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := splitMetric(line)
		if !ok {
			continue
		}
		vals[name] = value
	}
	return vals, sc.Err()
}

// splitMetric parses `name{labels...} value` into its name and value.
func splitMetric(line string) (string, float64, bool) {
	sp := strings.LastIndex(line, " ")
	if sp < 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
	if err != nil {
		return "", 0, false
	}
	name := strings.TrimSpace(line[:sp])
	if br := strings.IndexByte(name, '{'); br >= 0 {
		name = name[:br]
	}
	return name, v, true
}

type statusError int

func (e statusError) Error() string { return "sglang metrics: HTTP " + strconv.Itoa(int(e)) }

func errStatus(code int) error { return statusError(code) }
