package devobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux
)

// Server serves the live developer observability dashboard over HTTP.
// Start it with go srv.ListenAndServe(ctx).
type Server struct {
	obs  *Observer
	addr string
}

// NewServer creates an HTTP server bound to addr that serves the dashboard.
func NewServer(obs *Observer, addr string) *Server {
	return &Server{obs: obs, addr: addr}
}

// ListenAndServe starts the server and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/campaign", s.handleCampaign)
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.Handle("/debug/", http.DefaultServeMux) // pprof routes registered by net/http/pprof init()

	srv := &http.Server{Addr: s.addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	slog.Info("devobs: dashboard started", "addr", "http://"+s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Warn("devobs: server error", "err", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	snap := s.obs.Snapshot()
	json.NewEncoder(w).Encode(snap)
}

// handleCampaign serves director-level campaign metadata: current round, phase,
// frontier size, saturation rate, and escalation level. Feed this to the
// dashboard header so users can see what the engine is doing without tailing logs.
func (s *Server) handleCampaign(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	c := s.obs.CampaignSnapshot()
	json.NewEncoder(w).Encode(c)
}

// handleSSE streams state snapshots as Server-Sent Events. Each event is the
// full JSON state after a burst. Clients reconnect automatically on disconnect.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send initial state immediately.
	s.sendSSEState(w, flusher)

	ch := s.obs.Subscribe()
	defer s.obs.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
			s.sendSSEState(w, flusher)
		}
	}
}

func (s *Server) sendSSEState(w http.ResponseWriter, flusher http.Flusher) {
	snap := s.obs.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// handleMetrics serves a Prometheus-compatible text exposition of campaign counters.
// Exposed metrics:
//   - openthesis_states_total      (counter) total states explored
//   - openthesis_edges_total       (counter) total unique coverage edges
//   - openthesis_violations_total  (counter) total violations found
//   - openthesis_saturation_rate   (gauge)   recent burst edge-yield rate
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	snap := s.obs.Snapshot()
	camp := s.obs.CampaignSnapshot()
	fmt.Fprintf(w, "# HELP openthesis_states_total Total states explored by all workers.\n")
	fmt.Fprintf(w, "# TYPE openthesis_states_total counter\n")
	fmt.Fprintf(w, "openthesis_states_total %d\n", snap.TotalStates)
	fmt.Fprintf(w, "# HELP openthesis_edges_total Total unique coverage edges seen.\n")
	fmt.Fprintf(w, "# TYPE openthesis_edges_total counter\n")
	fmt.Fprintf(w, "openthesis_edges_total %d\n", snap.TotalEdges)
	fmt.Fprintf(w, "# HELP openthesis_violations_total Total violations found.\n")
	fmt.Fprintf(w, "# TYPE openthesis_violations_total counter\n")
	fmt.Fprintf(w, "openthesis_violations_total %d\n", snap.TotalViolations)
	fmt.Fprintf(w, "# HELP openthesis_saturation_rate Recent burst edge-yield rate (edges per burst).\n")
	fmt.Fprintf(w, "# TYPE openthesis_saturation_rate gauge\n")
	fmt.Fprintf(w, "openthesis_saturation_rate %g\n", snap.SatRate)
	fmt.Fprintf(w, "# HELP openthesis_escalation_level Current fault-rate escalation multiplier.\n")
	fmt.Fprintf(w, "# TYPE openthesis_escalation_level gauge\n")
	fmt.Fprintf(w, "openthesis_escalation_level %g\n", camp.EscalationLevel)
	fmt.Fprintf(w, "# HELP openthesis_round Current campaign round number.\n")
	fmt.Fprintf(w, "# TYPE openthesis_round gauge\n")
	fmt.Fprintf(w, "openthesis_round %d\n", camp.Round)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>OpenThesis Dev Observer</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:'SF Mono',ui-monospace,monospace;font-size:13px;background:#0d1117;color:#c9d1d9;line-height:1.5}
header{background:#161b22;border-bottom:1px solid #30363d;padding:10px 16px;display:flex;gap:24px;align-items:center;flex-wrap:wrap}
header h1{font-size:15px;font-weight:600;color:#58a6ff}
.stat{display:flex;flex-direction:column;gap:2px}
.stat-label{font-size:10px;color:#8b949e;text-transform:uppercase;letter-spacing:.05em}
.stat-value{font-size:16px;font-weight:600;color:#e6edf3}
.stat-value.green{color:#3fb950}
.stat-value.red{color:#f85149}
.stat-value.yellow{color:#d29922}
.stat-value.dim{color:#8b949e}
main{padding:16px;display:flex;flex-direction:column;gap:20px}
section{background:#161b22;border:1px solid #30363d;border-radius:6px;overflow:hidden}
section h2{font-size:11px;font-weight:600;color:#8b949e;text-transform:uppercase;letter-spacing:.08em;padding:10px 14px;background:#0d1117;border-bottom:1px solid #21262d}
table{width:100%;border-collapse:collapse}
th{font-size:10px;font-weight:600;color:#8b949e;text-transform:uppercase;letter-spacing:.05em;padding:6px 12px;text-align:left;border-bottom:1px solid #21262d;white-space:nowrap}
td{padding:6px 12px;border-bottom:1px solid #21262d;color:#c9d1d9;white-space:nowrap}
tr:last-child td{border-bottom:none}
tr:hover td{background:#1c2128}
.badge{display:inline-block;padding:1px 6px;border-radius:3px;font-size:11px;font-weight:600}
.badge-red{background:#3d1c1c;color:#f85149;border:1px solid #6e2222}
.badge-yellow{background:#2d2508;color:#d29922;border:1px solid #5a4500}
.badge-green{background:#0d2b14;color:#3fb950;border:1px solid #1a5c2a}
.badge-dim{background:#1c2128;color:#8b949e;border:1px solid #30363d}
.fault-tag{display:inline-block;padding:1px 5px;border-radius:3px;font-size:10px;background:#1c2128;color:#79c0ff;border:1px solid #1f3a5a;margin:1px}
.bar{display:inline-block;height:8px;border-radius:2px;vertical-align:middle;min-width:2px}
.bar-green{background:#238636}
.bar-yellow{background:#9e6a03}
.bar-red{background:#8b1a1a}
.never{color:#6e7681;font-style:italic}
.hint{font-size:11px;color:#f0883e;padding:6px 12px;background:#291c12;border-top:1px solid #4d3009}
.connected{color:#3fb950;font-size:11px;margin-left:auto}
.disconnected{color:#8b949e;font-size:11px;margin-left:auto}
#status{font-size:11px;margin-left:auto}
</style>
</head>
<body>
<header>
  <h1>OpenThesis Dev Observer</h1>
  <div class="stat"><div class="stat-label">States</div><div class="stat-value" id="h-states">-</div></div>
  <div class="stat"><div class="stat-label">Edges</div><div class="stat-value" id="h-edges">-</div></div>
  <div class="stat"><div class="stat-label">Violations</div><div class="stat-value" id="h-violations">-</div></div>
  <div class="stat"><div class="stat-label">Sat. Rate</div><div class="stat-value dim" id="h-sat">-</div></div>
  <div class="stat"><div class="stat-label">Uptime</div><div class="stat-value dim" id="h-uptime">-</div></div>
  <span id="status" class="disconnected">connecting...</span>
</header>
<main>
  <section>
    <h2>Assertions</h2>
    <div id="assert-hint" class="hint" style="display:none"></div>
    <div style="overflow-x:auto">
    <table>
      <thead><tr>
        <th>Property</th><th>Type</th><th>Evals</th><th>Pass%</th>
        <th title="Evaluations where any fault was active">Fault-Active%</th>
        <th title="Minimum left_value seen (for numeric assertions)">Min Val</th>
        <th title="Maximum left_value seen">Max Val</th>
        <th>Status</th>
      </tr></thead>
      <tbody id="assert-body"></tbody>
    </table>
    </div>
  </section>
  <section>
    <h2>Fault Efficacy</h2>
    <div style="overflow-x:auto">
    <table>
      <thead><tr>
        <th>Kind</th><th>Applications</th>
        <th title="Fraction of applications that produced new coverage edges">Edge Yield%</th>
        <th title="Total assertion evaluations while this fault was active">Assert Evals</th>
        <th>Violations</th>
      </tr></thead>
      <tbody id="fault-body"></tbody>
    </table>
    </div>
  </section>
  <section>
    <h2>Recent Bursts</h2>
    <div style="overflow-x:auto">
    <table>
      <thead><tr>
        <th>Step</th><th>Worker</th><th>Faults</th><th>New Edges</th>
        <th>Assert Evals</th><th>Violations</th><th>Burst Insns</th>
      </tr></thead>
      <tbody id="burst-body"></tbody>
    </table>
    </div>
  </section>
  <section>
    <h2>Workers</h2>
    <div style="overflow-x:auto">
    <table>
      <thead><tr><th>ID</th><th>Step</th><th>Active Faults</th><th>Last Seen</th></tr></thead>
      <tbody id="worker-body"></tbody>
    </table>
    </div>
  </section>
</main>
<script>
const fmt = n => n >= 1e9 ? (n/1e9).toFixed(1)+'B' : n >= 1e6 ? (n/1e6).toFixed(1)+'M' : n >= 1e3 ? (n/1e3).toFixed(1)+'K' : String(n);
const pct = (a,b) => b > 0 ? (a/b*100).toFixed(1)+'%' : '-';
const num = n => n == null ? '-' : typeof n === 'number' ? (Number.isInteger(n) ? n.toLocaleString() : n.toFixed(4)) : n;

function badge(text, color) {
  return '<span class="badge badge-'+color+'">'+text+'</span>';
}
function faultTags(kinds) {
  if (!kinds || !kinds.length) return '<span class="never">none</span>';
  return kinds.map(k=>'<span class="fault-tag">'+k+'</span>').join('');
}
function bar(pctVal, color) {
  const w = Math.min(60, Math.max(2, Math.round(pctVal * 0.6)));
  return '<span class="bar bar-'+color+'" style="width:'+w+'px"></span> ';
}

function renderAssertions(assertions, totalFaultApps) {
  const tbody = document.getElementById('assert-body');
  let hints = [];
  if (!assertions || !assertions.length) {
    tbody.innerHTML = '<tr><td colspan="8" class="never" style="text-align:center;padding:20px">No assertions evaluated yet</td></tr>';
    return;
  }
  tbody.innerHTML = assertions.map(a => {
    const evalCount = a.eval_count || 0;
    const faultPct = evalCount > 0 ? a.eval_with_fault_active / evalCount * 100 : 0;
    const passPct = evalCount > 0 ? a.pass_count / evalCount * 100 : 0;

    // Determine status badge and hints.
    let status, hint = null;
    if (evalCount === 0) {
      status = badge('NEVER REACHED', 'red');
      hint = '"'+a.property+'" was declared but never evaluated - this code path was not exercised.';
    } else if (a.fail_count > 0) {
      status = badge('VIOLATION', 'red');
    } else if (faultPct < 1 && totalFaultApps > 100) {
      status = badge('LOW COUPLING', 'yellow');
      hint = '"'+a.property+'": only '+faultPct.toFixed(1)+'% of evaluations occurred while a fault was active. Faults fire but the system recovers before this assertion runs.';
    } else if (faultPct < 10 && totalFaultApps > 100) {
      status = badge('OK (low coupling)', 'yellow');
    } else {
      status = badge('OK', 'green');
    }
    if (hint) hints.push(hint);

    const faultColor = faultPct >= 10 ? 'green' : faultPct >= 1 ? 'yellow' : 'red';
    const minVal = a.has_numeric_values ? num(a.min_left_value) : '-';
    const maxVal = a.has_numeric_values ? num(a.max_left_value) : '-';

    return '<tr>' +
      '<td title="'+a.property+'">'+a.property.substring(0,60)+(a.property.length>60?'...':'')+'</td>' +
      '<td>'+a.assert_type+'</td>' +
      '<td>'+(evalCount === 0 ? '<span class="never">0</span>' : fmt(evalCount))+'</td>' +
      '<td>'+(evalCount > 0 ? passPct.toFixed(1)+'%' : '-')+'</td>' +
      '<td>'+bar(faultPct, faultColor)+faultPct.toFixed(1)+'%</td>' +
      '<td>'+(a.has_numeric_values ? minVal : '<span class="never">-</span>')+'</td>' +
      '<td>'+(a.has_numeric_values ? maxVal : '<span class="never">-</span>')+'</td>' +
      '<td>'+status+'</td>' +
    '</tr>';
  }).join('');

  const hintEl = document.getElementById('assert-hint');
  if (hints.length > 0) {
    hintEl.style.display = '';
    hintEl.innerHTML = '<b>Diagnostics:</b> ' + hints.slice(0,3).join(' | ');
  } else {
    hintEl.style.display = 'none';
  }
}

function renderFaults(faults) {
  const tbody = document.getElementById('fault-body');
  if (!faults || !faults.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="never" style="text-align:center;padding:20px">No faults applied yet</td></tr>';
    return;
  }
  tbody.innerHTML = faults.map(f => {
    const yieldPct = f.applications > 0 ? f.edge_yield_pct : 0;
    const yieldColor = yieldPct >= 20 ? 'green' : yieldPct >= 5 ? 'yellow' : 'red';
    return '<tr>' +
      '<td><span class="fault-tag">'+f.kind+'</span></td>' +
      '<td>'+fmt(f.applications)+'</td>' +
      '<td>'+bar(yieldPct, yieldColor)+yieldPct.toFixed(1)+'%</td>' +
      '<td>'+fmt(f.assert_eval_count)+'</td>' +
      '<td>'+(f.violation_count > 0 ? badge(f.violation_count, 'red') : '0')+'</td>' +
    '</tr>';
  }).join('');
}

function renderBursts(bursts) {
  const tbody = document.getElementById('burst-body');
  if (!bursts || !bursts.length) {
    tbody.innerHTML = '<tr><td colspan="7" class="never" style="text-align:center;padding:20px">No bursts yet</td></tr>';
    return;
  }
  tbody.innerHTML = bursts.slice(0,50).map(b => {
    const edgeColor = b.new_edges > 0 ? 'green' : 'dim';
    return '<tr>' +
      '<td>'+fmt(b.step)+'</td>' +
      '<td>w'+b.worker_id+'</td>' +
      '<td>'+faultTags(b.fault_kinds)+'</td>' +
      '<td>'+(b.new_edges > 0 ? '<span style="color:#3fb950">+'+b.new_edges+'</span>' : '<span class="never">0</span>')+'</td>' +
      '<td>'+b.assert_evals+'</td>' +
      '<td>'+(b.violations > 0 ? badge(b.violations,'red') : '0')+'</td>' +
      '<td>'+fmt(b.burst_insns)+'</td>' +
    '</tr>';
  }).join('');
}

function renderWorkers(workers) {
  const tbody = document.getElementById('worker-body');
  if (!workers || !workers.length) {
    tbody.innerHTML = '<tr><td colspan="4" class="never" style="text-align:center;padding:20px">No workers</td></tr>';
    return;
  }
  tbody.innerHTML = workers.map(w => {
    const ago = w.last_updated ? Math.round((Date.now() - new Date(w.last_updated).getTime())/1000) : null;
    return '<tr>' +
      '<td>w'+w.id+'</td>' +
      '<td>'+fmt(w.step)+'</td>' +
      '<td>'+faultTags(w.fault_kinds)+'</td>' +
      '<td>'+(ago != null ? ago+'s ago' : '-')+'</td>' +
    '</tr>';
  }).join('');
}

function render(data) {
  document.getElementById('h-states').textContent = fmt(data.total_states);
  const vEl = document.getElementById('h-violations');
  vEl.textContent = data.total_violations;
  vEl.className = 'stat-value ' + (data.total_violations > 0 ? 'red' : 'green');
  document.getElementById('h-edges').textContent = fmt(data.total_edges);
  document.getElementById('h-sat').textContent = (data.sat_rate||0).toFixed(2)+' e/burst';
  const uptime = data.uptime_seconds || 0;
  const h = Math.floor(uptime/3600), m = Math.floor((uptime%3600)/60), s = Math.floor(uptime%60);
  document.getElementById('h-uptime').textContent = (h>0?h+'h ':'')+m+'m '+s+'s';

  const totalApps = (data.faults||[]).reduce((s,f)=>s+f.applications,0);
  renderAssertions(data.assertions, totalApps);
  renderFaults(data.faults);
  renderBursts(data.recent_bursts);
  renderWorkers(data.workers);
}

// Connect via SSE with fallback polling.
function connect() {
  const es = new EventSource('/events');
  const statusEl = document.getElementById('status');
  es.onopen = () => { statusEl.textContent = 'live'; statusEl.className = 'connected'; };
  es.onmessage = e => { try { render(JSON.parse(e.data)); } catch(_){} };
  es.onerror = () => {
    statusEl.textContent = 'reconnecting...';
    statusEl.className = 'disconnected';
  };
}

// Initial fetch then SSE.
fetch('/api/state').then(r=>r.json()).then(render).catch(()=>{});
connect();
</script>
</body>
</html>`
