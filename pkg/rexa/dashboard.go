package rexa

import (
	"net/http"
)

// Live capacity dashboard.
//
// Served by the agent itself rather than the platform, because the numbers are
// in-process: the Go server is the only thing that sees every call and times
// every downstream request. It polls the same snapshot /health returns, so
// what an operator sees is exactly what the platform sees — no second code
// path to drift.
//
// Deliberately dependency-free: one self-contained HTML page, no CDN, no build
// step. It has to load on a box behind a tunnel at 3am during an incident.

// RoutesDashboard registers the operator dashboard and its JSON feed.
//
// Kept separate from Routes() so the contract surface the platform talks to
// and the operator surface stay independently mountable — you may want the
// dashboard on an internal port and never exposed alongside /connection.
func (s *Server) RoutesDashboard(mux *http.ServeMux) {
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})
	// The feed is the health snapshot verbatim. A separate path so the
	// dashboard can poll faster than the platform's 5 s probe without
	// muddying health-probe traffic in the logs.
	mux.HandleFunc("GET /dashboard/data", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.metrics.Snapshot())
	})
}

const dashboardHTML = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Voice agent — capacity</title>
<style>
  :root{
    --bg:#0f1115; --panel:#171a21; --line:#242833; --tx:#e6e9ef; --dim:#8b93a7;
    --ok:#3fb950; --warn:#d29922; --bad:#f85149; --idle:#484f5e; --accent:#58a6ff;
  }
  @media (prefers-color-scheme:light){
    :root{ --bg:#f6f7f9; --panel:#fff; --line:#e3e6ea; --tx:#1c2027; --dim:#5d6575; --idle:#c2c8d2; }
  }
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--tx);
       font:14px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;padding:20px}
  h1{font-size:15px;font-weight:600;margin:0;letter-spacing:.02em}
  .head{display:flex;align-items:baseline;gap:12px;flex-wrap:wrap;margin-bottom:16px}
  .verdict{font-size:12px;font-weight:600;padding:3px 10px;border-radius:999px;
           border:1px solid var(--line)}
  .verdict.yes{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 40%,transparent)}
  .verdict.no{color:var(--bad);border-color:color-mix(in srgb,var(--bad) 40%,transparent)}
  .stale{color:var(--warn);font-size:12px}
  .grid{display:grid;gap:12px;grid-template-columns:repeat(auto-fit,minmax(190px,1fr))}
  .card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
  .label{font-size:11px;text-transform:uppercase;letter-spacing:.07em;color:var(--dim)}
  .big{font-size:30px;font-weight:650;font-variant-numeric:tabular-nums;line-height:1.15;margin-top:2px}
  .sub{font-size:12px;color:var(--dim)}
  .bar{height:6px;border-radius:3px;background:var(--idle);overflow:hidden;margin-top:10px}
  .bar>i{display:block;height:100%;background:var(--ok);transition:width .4s,background .4s}
  .bar.warn>i{background:var(--warn)} .bar.bad>i{background:var(--bad)}
  h2{font-size:11px;text-transform:uppercase;letter-spacing:.07em;color:var(--dim);
     margin:22px 0 10px;font-weight:600}
  table{width:100%;border-collapse:collapse;background:var(--panel);
        border:1px solid var(--line);border-radius:10px;overflow:hidden}
  th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--line);font-size:13px}
  th{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:var(--dim);font-weight:600}
  tr:last-child td{border-bottom:none}
  td.n{font-variant-numeric:tabular-nums;text-align:right}
  .pill{font-size:11px;font-weight:600;padding:2px 9px;border-radius:999px;display:inline-block}
  .s-ok{color:var(--ok)} .s-degraded{color:var(--warn)} .s-saturated{color:var(--bad)}
  .s-unknown{color:var(--dim)}
  footer{margin-top:18px;font-size:11px;color:var(--dim)}
  code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px}
</style>

<div class="head">
  <h1>Voice agent — capacity</h1>
  <span id="verdict" class="verdict">…</span>
  <span id="stale" class="stale"></span>
</div>

<div class="grid">
  <div class="card">
    <div class="label">On GPU</div>
    <div class="big" id="ongpu">–</div>
    <div class="sub" id="ceil">holding a pipeline</div>
    <div class="bar" id="bar"><i style="width:0"></i></div>
  </div>
  <div class="card">
    <div class="label">Ringing</div>
    <div class="big" id="res">–</div>
    <div class="sub">dispatched, not yet answered</div>
  </div>
  <div class="card">
    <div class="label">Voicemail</div>
    <div class="big" id="vm">–</div>
    <div class="sub">announcement only — no GPU</div>
  </div>
  <div class="card">
    <div class="label">Total live</div>
    <div class="big" id="total">–</div>
    <div class="sub" id="totalcap">all three states</div>
  </div>
  <div class="card">
    <div class="label">Headroom</div>
    <div class="big" id="head">–</div>
    <div class="sub" id="cost">of the GPU ceiling</div>
  </div>
</div>

<h2>Measured — what this campaign is actually doing</h2>
<table>
  <tbody>
    <tr><td>Answer rate <span class="sub">— reached a live pipeline</span></td>
        <td class="n" id="ar">–</td></tr>
    <tr><td>Ring time p95 <span class="sub">— dial to answer</span></td>
        <td class="n" id="ring">–</td></tr>
    <tr><td>Cost charged per dispatch <span class="sub">— 1.0 = no over-subscription</span></td>
        <td class="n" id="weight">–</td></tr>
    <tr><td>Resolved calls in window</td><td class="n" id="samples">–</td></tr>
  </tbody>
</table>

<h2>First turn — the pause after the caller says hello</h2>
<table>
  <tbody>
    <tr><td>State</td><td class="n" id="ft-state">–</td></tr>
    <tr><td>First-token p95 <span class="sub">— first reply of a call only</span></td>
        <td class="n" id="ft-p95">–</td></tr>
    <tr><td>Calls measured</td><td class="n" id="ft-n">–</td></tr>
    <tr><td>Times it refused traffic <span class="sub">— climbing means prompts are not sharing prefixes</span></td>
        <td class="n" id="ft-trips">–</td></tr>
  </tbody>
</table>
<p class="sub">
  Tracked apart from every other turn because it is the only one that pays a cold prefill:
  at 60 calls, sharing a campaign prompt gave 1.85s here while a distinct prompt per call gave 9.9s —
  and the average across all turns read 6.3s, low enough to pass. This can refuse work on its own,
  with the GPU ceiling nowhere near reached.
</p>

<h2>SGLang — the LLM server's own view</h2>
<table>
  <tbody>
    <tr><td>Prefix cache hit rate <span class="sub">— collapses when prompts are unrelated</span></td>
        <td class="n" id="sg-hit">–</td></tr>
    <tr><td>KV pool in use</td><td class="n" id="sg-tok">–</td></tr>
    <tr><td>Running requests</td><td class="n" id="sg-run">–</td></tr>
    <tr><td>Queued <span class="sub">— sustained non-zero means the LLM is the bottleneck</span></td>
        <td class="n" id="sg-q">–</td></tr>
    <tr><td>Reading age</td><td class="n" id="sg-age">–</td></tr>
  </tbody>
</table>
<p class="sub">Reported, never acted on — no threshold here refuses traffic. Read it next to
  first turn: a falling hit rate says cache thrash (fix the prompt layout), a growing queue
  says too many requests (send fewer).</p>

<h2>Tiers — p95 of our own requests</h2>
<table>
  <thead><tr><th>Tier</th><th>State</th><th class="n">p95</th><th class="n">Samples</th></tr></thead>
  <tbody id="tiers"><tr><td colspan="4" class="sub">waiting…</td></tr></tbody>
</table>

<h2>Process — is it leaking?</h2>
<table>
  <tbody>
    <tr><td>Goroutines <span class="sub">— should track live calls, not climb with total</span></td>
        <td class="n" id="rt-gor">–</td></tr>
    <tr><td>Go heap</td><td class="n" id="rt-heap">–</td></tr>
    <tr><td>RSS <span class="sub">— includes ONNX; this is what gets OOM-killed</span></td>
        <td class="n" id="rt-rss">–</td></tr>
    <tr><td>Open file descriptors <span class="sub">— sockets leak before memory does</span></td>
        <td class="n" id="rt-fds">–</td></tr>
    <tr><td>Last GC pause</td><td class="n" id="rt-gc">–</td></tr>
  </tbody>
</table>
<p class="sub">Read these against <b>total live</b> above. Goroutines rising while calls stay flat is
  the earliest leak signal there is — every call starts several and every one must end with it.
  RSS drifting away from heap is not Go memory: it is the speech runtimes, which grow on demand
  and never shrink.</p>

<h2>Since start</h2>
<table>
  <tbody>
    <tr><td>Calls</td><td class="n" id="t-calls">–</td></tr>
    <tr><td>Voicemail</td><td class="n" id="t-vm">–</td></tr>
    <tr><td>Refused at capacity</td><td class="n" id="t-rej">–</td></tr>
    <tr><td>Reaped <span class="sub">— reservations that never resolved</span></td>
        <td class="n" id="t-reap">–</td></tr>
    <tr><td>Live-publish failures <span class="sub">— calls whose Redis events never arrived</span></td>
        <td class="n" id="t-live">–</td></tr>
  </tbody>
</table>

<footer>
  Polls <code>/dashboard/data</code> every 2s — the same snapshot <code>/health</code> serves.
  Judge on p95: at 100 concurrent sessions the median stayed near 1.1s while callers heard
  multi-second hangs. Tier thresholds are calibration defaults, not measured values.
  A ringing call costs capacity because it will need a pipeline unless it turns out to be a
  machine — that is what stops a fresh campaign dispatching without limit before anything answers.
</footer>

<script>
const $ = id => document.getElementById(id);
const NBSP = " ";
let misses = 0;

function fmtMs(ms){ return ms >= 1000 ? (ms/1000).toFixed(2)+"s" : ms+"ms"; }

function render(d){
  // Say WHY when we are refusing. "not accepting" alone sends whoever is
  // watching to read code to find out which of five conditions fired.
  $("verdict").textContent = d.accepting ? "accepting calls"
    : d.first_turn && d.first_turn.blocked ? "not accepting — first turn too slow"
    : "not accepting";
  $("verdict").className = "verdict " + (d.accepting ? "yes" : "no");

  $("ongpu").textContent = d.calls.on_gpu;
  $("res").textContent   = d.calls.reserved;
  $("vm").textContent    = d.calls.voicemail;
  $("total").textContent = d.calls.total;

  const max = d.capacity.max_gpu_calls;
  // A ceiling of 0 means unlimited, so there is no percentage to show.
  if (max > 0){
    $("ceil").textContent = "of " + max + " ceiling";
    $("head").textContent = Math.round(d.capacity.headroom*100) + "%";
    // The bar tracks weighted GPU COST, not the raw pipeline count: that is
    // the number admission actually decides on, and while calls are ringing
    // it is the only one that reflects what has been committed.
    $("cost").textContent = "cost " + d.capacity.gpu_cost.toFixed(1) + " of " + max;
    const used = Math.min(100, Math.round(d.capacity.gpu_cost / max * 100));
    const bar = $("bar");
    bar.className = "bar" + (used >= 90 ? " bad" : used >= 75 ? " warn" : "");
    bar.firstElementChild.style.width = used + "%";
  } else {
    $("ceil").textContent = "no ceiling configured";
    $("head").textContent = "∞";
    $("cost").textContent = "";
    $("bar").firstElementChild.style.width = "0";
  }
  $("totalcap").textContent = d.capacity.max_total_calls > 0
    ? "of " + d.capacity.max_total_calls + " hard cap" : "all three states";

  // Answer rate is meaningless until calls have actually resolved.
  const meas = d.measured;
  $("ar").textContent     = meas.samples > 0 ? Math.round(meas.answer_rate*100) + "%" : "—";
  $("ring").textContent   = meas.ring_ms_p95 > 0 ? fmtMs(meas.ring_ms_p95) : "—";
  $("weight").textContent = d.capacity.human_weight.toFixed(2);
  $("samples").textContent = meas.samples;

  const ft = d.first_turn || {};
  $("ft-state").innerHTML = "<span class='pill s-" + (ft.state||"unknown") + "'>" +
    (ft.blocked ? "refusing for " + ft.blocked_for_secs + "s" : (ft.state||"unknown")) + "</span>";
  $("ft-p95").textContent   = ft.samples > 0 ? fmtMs(ft.p95_ms) : "—";
  $("ft-n").textContent     = ft.samples || 0;
  $("ft-trips").textContent = ft.trips || 0;

  const sg = d.sglang || {};
  const pct = v => (v*100).toFixed(0) + "%";
  // Never show a stale reading as current: a poller that stopped answering
  // leaves the last good numbers in place, and they look perfectly healthy.
  const sgLive = sg.ok && sg.age_secs < 30;
  $("sg-hit").textContent = sgLive ? pct(sg.cache_hit_rate) : "—";
  $("sg-tok").textContent = sgLive ? pct(sg.token_usage) : "—";
  $("sg-run").textContent = sgLive ? sg.running_reqs : "—";
  $("sg-q").textContent   = sgLive ? sg.queued_reqs : "—";
  $("sg-age").textContent = sg.ok
    ? (sgLive ? sg.age_secs + "s ago (" + sg.replicas + " replicas)" : "stale — " + sg.age_secs + "s old")
    : "not polling";

  const order = ["llm","asr","tts"];
  $("tiers").innerHTML = order.filter(k => d.tiers[k]).map(k => {
    const t = d.tiers[k];
    // "unknown" means too few samples for a meaningful p95 — show it as
    // absent rather than implying a real measurement of 0ms.
    const p95 = t.state === "unknown" ? NBSP + "—" : fmtMs(t.p95_ms);
    return "<tr><td>" + k.toUpperCase() + "</td>" +
           "<td><span class='pill s-" + t.state + "'>" + t.state + "</span></td>" +
           "<td class='n'>" + p95 + "</td>" +
           "<td class='n'>" + t.samples + "</td></tr>";
  }).join("");

  $("t-calls").textContent = d.totals.calls;
  $("t-vm").textContent    = d.totals.voicemail;
  $("t-rej").textContent   = d.totals.rejected;
  const rt = d.runtime || {};
  $("rt-gor").textContent  = rt.goroutines ?? "–";
  $("rt-heap").textContent = (rt.heap_mb ?? 0) + " MB";
  $("rt-rss").textContent  = rt.rss_mb ? rt.rss_mb + " MB" : "–";
  $("rt-fds").textContent  = rt.open_fds || "–";
  $("rt-gc").textContent   = (rt.gc_pause_ms ?? 0).toFixed(1) + " ms";

  $("t-reap").textContent  = d.totals.reaped;
  // Publishing is fire-and-forget, so this counter is the only place the
  // failure surfaces. Colour it when non-zero: the symptom on the caller's
  // side is an empty wallboard, which reads as "the feature is missing".
  const lpf = d.totals.live_publish_failures || 0;
  $("t-live").textContent = lpf;
  $("t-live").style.color = lpf > 0 ? "var(--bad)" : "";
}

async function tick(){
  try {
    const r = await fetch("/dashboard/data", {cache:"no-store"});
    if (!r.ok) throw new Error(r.status);
    render(await r.json());
    misses = 0;
    $("stale").textContent = "";
  } catch (e) {
    // Say the data is stale rather than leaving numbers that look live.
    // One miss is a blip; several mean the agent is gone.
    if (++misses > 1) $("stale").textContent = "stale — " + misses + " failed polls";
  }
}
tick(); setInterval(tick, 2000);
</script>
`
