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
    <div class="label">Voicemail</div>
    <div class="big" id="vm">–</div>
    <div class="sub">announcement only — no GPU</div>
  </div>
  <div class="card">
    <div class="label">Total live</div>
    <div class="big" id="total">–</div>
    <div class="sub">on GPU + voicemail</div>
  </div>
  <div class="card">
    <div class="label">Headroom</div>
    <div class="big" id="head">–</div>
    <div class="sub">of the GPU ceiling</div>
  </div>
</div>

<h2>Tiers — p95 of our own requests</h2>
<table>
  <thead><tr><th>Tier</th><th>State</th><th class="n">p95</th><th class="n">Samples</th></tr></thead>
  <tbody id="tiers"><tr><td colspan="4" class="sub">waiting…</td></tr></tbody>
</table>

<h2>Since start</h2>
<table>
  <tbody>
    <tr><td>Calls</td><td class="n" id="t-calls">–</td></tr>
    <tr><td>Voicemail</td><td class="n" id="t-vm">–</td></tr>
    <tr><td>Refused at capacity</td><td class="n" id="t-rej">–</td></tr>
  </tbody>
</table>

<footer>
  Polls <code>/dashboard/data</code> every 2s — the same snapshot <code>/health</code> serves.
  Judge on p95: at 100 concurrent sessions the median stayed near 1.1s while callers heard
  multi-second hangs. Tier thresholds are calibration defaults, not measured values.
</footer>

<script>
const $ = id => document.getElementById(id);
const NBSP = " ";
let misses = 0;

function fmtMs(ms){ return ms >= 1000 ? (ms/1000).toFixed(2)+"s" : ms+"ms"; }

function render(d){
  $("verdict").textContent = d.accepting ? "accepting calls" : "not accepting";
  $("verdict").className = "verdict " + (d.accepting ? "yes" : "no");

  $("ongpu").textContent = d.calls.on_gpu;
  $("vm").textContent    = d.calls.voicemail;
  $("total").textContent = d.calls.total;

  const max = d.capacity.max_gpu_calls;
  // A ceiling of 0 means unlimited, so there is no percentage to show.
  if (max > 0){
    $("ceil").textContent = "of " + max + " ceiling";
    $("head").textContent = Math.round(d.capacity.headroom*100) + "%";
    const used = Math.min(100, Math.round(d.calls.on_gpu / max * 100));
    const bar = $("bar");
    bar.className = "bar" + (used >= 90 ? " bad" : used >= 75 ? " warn" : "");
    bar.firstElementChild.style.width = used + "%";
  } else {
    $("ceil").textContent = "no ceiling configured";
    $("head").textContent = "∞";
    $("bar").firstElementChild.style.width = "0";
  }

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
