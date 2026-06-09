package report

import (
	"html/template"
)

// htmlTmpl is the compiled HTML report template. It is a self-contained single-file
// document with all CSS inlined; no external dependencies.
var htmlTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"statusClass": func(failed int) string {
		if failed > 0 {
			return "fail"
		}
		return "pass"
	},
	"pct": func(p float64) string {
		// Format percentage to one decimal without fmt.
		v := int(p*10 + 0.5) // round half-up
		if v < 0 {
			v = 0
		}
		whole := v / 10
		frac := v % 10
		return itoa(whole) + "." + itoa(frac) + "%"
	},
	"itoa":   itoa,
	"u64toa": uitoa,
	"u32toa": func(v uint32) string {
		return uitoa(uint64(v))
	},
	"lenViolations": func(v []ViolationEntry) int {
		return len(v)
	},
	"lenFindings": func(v []Finding) int {
		return len(v)
	},
}).Parse(htmlTemplate))

// itoa converts an int to a decimal string without importing fmt or strconv.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	buf := make([]byte, 20)
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// uitoa converts a uint64 to a decimal string without importing fmt or strconv.
func uitoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	buf := make([]byte, 20)
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[pos:])
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OpenThesis Report{{if .ProjectName}}; {{.ProjectName}}{{end}}</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#e2e8f0;background:#0f172a}
a{color:#60a5fa}
h1,h2,h3{font-weight:600;line-height:1.25}
h1{font-size:1.5rem}
h2{font-size:1.125rem;margin-bottom:.75rem}
h3{font-size:.9375rem;margin-bottom:.5rem}

/* layout */
.header{background:linear-gradient(135deg,#1e3a5f 0%,#0f172a 100%);border-bottom:1px solid #1e40af;padding:1.25rem 2rem;display:flex;align-items:center;gap:1rem}
.header-logo{width:2rem;height:2rem;background:#3b82f6;border-radius:.375rem;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:1rem;color:#fff;flex-shrink:0}
.header-title{flex:1}
.header-title h1{color:#f1f5f9}
.header-meta{font-size:.8125rem;color:#94a3b8;margin-top:.125rem}
.badge{display:inline-block;padding:.125rem .5rem;border-radius:.25rem;font-size:.75rem;font-weight:600;text-transform:uppercase;letter-spacing:.04em}
.badge-pass{background:#14532d;color:#4ade80}
.badge-fail{background:#7f1d1d;color:#f87171}
.badge-info{background:#1e3a5f;color:#93c5fd}

main{max-width:1100px;margin:2rem auto;padding:0 1.5rem}
.section{background:#1e293b;border:1px solid #334155;border-radius:.5rem;padding:1.25rem;margin-bottom:1.5rem}

/* summary grid */
.summary-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:1rem;margin-bottom:1.5rem}
.metric{background:#0f172a;border:1px solid #334155;border-radius:.5rem;padding:1rem;text-align:center}
.metric-value{font-size:2rem;font-weight:700;line-height:1;color:#f1f5f9;margin-bottom:.25rem}
.metric-label{font-size:.75rem;color:#94a3b8;text-transform:uppercase;letter-spacing:.06em}
.metric-value.ok{color:#4ade80}
.metric-value.warn{color:#fb923c}
.metric-value.err{color:#f87171}

/* assertions */
.assertion-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:1rem}
.assertion-card{background:#0f172a;border:1px solid #334155;border-radius:.5rem;padding:1rem}
.assertion-card .title{font-size:.8125rem;font-weight:600;text-transform:uppercase;letter-spacing:.06em;color:#94a3b8;margin-bottom:.75rem}
.assertion-row{display:flex;justify-content:space-between;align-items:center;padding:.25rem 0;border-bottom:1px solid #1e293b;font-size:.875rem}
.assertion-row:last-child{border-bottom:none}
.pass{color:#4ade80}
.fail{color:#f87171}
.neutral{color:#94a3b8}

/* table */
table{width:100%;border-collapse:collapse;font-size:.875rem}
thead tr{background:#0f172a}
th{padding:.625rem .75rem;text-align:left;font-size:.75rem;font-weight:600;text-transform:uppercase;letter-spacing:.06em;color:#64748b;border-bottom:1px solid #334155}
td{padding:.625rem .75rem;border-bottom:1px solid #1e293b;vertical-align:top;color:#cbd5e1}
tr:last-child td{border-bottom:none}
tr:hover td{background:#0f172a}
.mono{font-family:"SFMono-Regular",Consolas,"Liberation Mono",Menlo,monospace;font-size:.8125rem}
.text-muted{color:#64748b}
.text-err{color:#f87171}
.text-ok{color:#4ade80}
.text-warn{color:#fb923c}

/* coverage bar */
.cov-bar-outer{background:#1e293b;border-radius:9999px;height:.625rem;width:100%;overflow:hidden;margin-top:.5rem}
.cov-bar-inner{background:linear-gradient(90deg,#3b82f6,#60a5fa);height:100%;border-radius:9999px;transition:width .4s ease}

/* empty state */
.empty{text-align:center;padding:2rem;color:#64748b;font-size:.875rem}

/* footer */
footer{text-align:center;padding:2rem 1.5rem;color:#334155;font-size:.75rem;border-top:1px solid #1e293b;margin-top:2rem}
</style>
</head>
<body>

<div class="header">
  <div class="header-logo">OT</div>
  <div class="header-title">
    <h1>{{if .ProjectName}}{{.ProjectName}}{{else}}OpenThesis{{end}}; Triage Report</h1>
    <div class="header-meta">
      Run <span class="mono">{{.RunID}}</span>
      {{if .Description}}&nbsp;&middot;&nbsp;{{.Description}}{{end}}
      &nbsp;&middot;&nbsp; Generated {{.CreatedAt.Format "2006-01-02 15:04:05 UTC"}}
    </div>
  </div>
  {{if .Summary.BugsFound}}
    <span class="badge badge-fail">{{itoa .Summary.BugsFound}} bug{{if gt .Summary.BugsFound 1}}s{{end}}</span>
  {{else}}
    <span class="badge badge-pass">No Bugs</span>
  {{end}}
</div>

<main>

<!-- Summary Metrics -->
<div class="summary-grid">
  <div class="metric">
    <div class="metric-value">{{u64toa .Summary.TotalStates}}</div>
    <div class="metric-label">States Explored</div>
  </div>
  <div class="metric">
    <div class="metric-value {{if .Summary.BugsFound}}err{{else}}ok{{end}}">{{itoa .Summary.BugsFound}}</div>
    <div class="metric-label">Bugs Found</div>
  </div>
  <div class="metric">
    <div class="metric-value">{{u32toa .Summary.MaxDepth}}</div>
    <div class="metric-label">Max Depth</div>
  </div>
  <div class="metric">
    <div class="metric-value">{{u64toa .Coverage.TotalEdges}}</div>
    <div class="metric-label">Total Edges</div>
  </div>
  <div class="metric">
    <div class="metric-value {{if gt .Coverage.Percentage 0.0}}ok{{else}}neutral{{end}}">{{pct .Coverage.Percentage}}</div>
    <div class="metric-label">Coverage</div>
  </div>
</div>

<!-- Assertions -->
<div class="section">
  <h2>Assertions</h2>
  <div class="assertion-grid">
    <div class="assertion-card">
      <div class="title">Always</div>
      <div class="assertion-row">
        <span>Total</span>
        <span class="mono">{{itoa .Assertions.Always.Total}}</span>
      </div>
      <div class="assertion-row">
        <span>Passed</span>
        <span class="mono {{if gt .Assertions.Always.Passed 0}}pass{{end}}">{{itoa .Assertions.Always.Passed}}</span>
      </div>
      <div class="assertion-row">
        <span>Failed</span>
        <span class="mono {{if gt .Assertions.Always.Failed 0}}fail{{end}}">{{itoa .Assertions.Always.Failed}}</span>
      </div>
    </div>
    <div class="assertion-card">
      <div class="title">Sometimes</div>
      <div class="assertion-row">
        <span>Total</span>
        <span class="mono">{{itoa .Assertions.Sometimes.Total}}</span>
      </div>
      <div class="assertion-row">
        <span>Observed</span>
        <span class="mono {{if gt .Assertions.Sometimes.Passed 0}}pass{{end}}">{{itoa .Assertions.Sometimes.Passed}}</span>
      </div>
      <div class="assertion-row">
        <span>Not Observed</span>
        <span class="mono {{statusClass .Assertions.Sometimes.Failed}}">{{itoa .Assertions.Sometimes.Failed}}</span>
      </div>
    </div>
    <div class="assertion-card">
      <div class="title">Reachable</div>
      <div class="assertion-row">
        <span>Total</span>
        <span class="mono">{{itoa .Assertions.Reachable.Total}}</span>
      </div>
      <div class="assertion-row">
        <span>Reached</span>
        <span class="mono {{if gt .Assertions.Reachable.Passed 0}}pass{{end}}">{{itoa .Assertions.Reachable.Passed}}</span>
      </div>
      <div class="assertion-row">
        <span>Not Reached</span>
        <span class="mono {{statusClass .Assertions.Reachable.Failed}}">{{itoa .Assertions.Reachable.Failed}}</span>
      </div>
    </div>
  </div>
</div>

<!-- Coverage -->
<div class="section">
  <h2>Coverage</h2>
  <table>
    <thead>
      <tr><th>Metric</th><th>Value</th></tr>
    </thead>
    <tbody>
      <tr>
        <td>Total Edges</td>
        <td class="mono">{{u64toa .Coverage.TotalEdges}}</td>
      </tr>
      <tr>
        <td>New Edges Discovered</td>
        <td class="mono">{{u64toa .Coverage.NewEdges}}</td>
      </tr>
      <tr>
        <td>Coverage Percentage</td>
        <td>
          <div style="display:flex;align-items:center;gap:.75rem">
            <span class="mono">{{pct .Coverage.Percentage}}</span>
            <div class="cov-bar-outer" style="flex:1">
              <div class="cov-bar-inner" style="width:{{pct .Coverage.Percentage}}"></div>
            </div>
          </div>
        </td>
      </tr>
    </tbody>
  </table>
</div>

<!-- Violations -->
<div class="section">
  <h2>Violations{{if .Violations}} <span class="badge badge-fail">{{itoa (len .Violations)}}</span>{{end}}</h2>
  {{if .Violations}}
  <table>
    <thead>
      <tr>
        <th>Property</th>
        <th>Message</th>
        <th>Step</th>
        <th>Path Depth</th>
        <th>Seed</th>
      </tr>
    </thead>
    <tbody>
      {{range .Violations}}
      <tr>
        <td class="text-err mono">{{.Property}}</td>
        <td>{{.Message}}</td>
        <td class="mono text-muted">{{u64toa .Step}}</td>
        <td class="mono text-muted">{{itoa .PathDepth}}</td>
        <td class="mono text-muted">{{u64toa .Seed}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{else}}
  <div class="empty">
    <span class="text-ok">No violations detected</span>; all always-assertions held throughout the exploration.
  </div>
  {{end}}
</div>

<!-- Properties -->
{{if .Properties}}
<div class="section">
  <h2>Properties</h2>
  <table>
    <thead>
      <tr>
        <th>Property</th>
        <th>Type</th>
        <th>Total Evals</th>
        <th>Passed</th>
        <th>Failed</th>
        <th>Status</th>
      </tr>
    </thead>
    <tbody>
      {{range .Properties}}
        {{range .Properties}}
        <tr>
          <td>{{.Message}}</td>
          <td class="mono text-muted">{{.AssertType}}</td>
          <td class="mono">{{itoa .Total}}</td>
          <td class="mono {{if gt .Passed 0}}pass{{end}}">{{itoa .Passed}}</td>
          <td class="mono {{if gt .Failed 0}}fail{{end}}">{{itoa .Failed}}</td>
          <td>
            {{if gt .Failed 0}}
              <span class="badge badge-fail">failing</span>
            {{else if gt .Passed 0}}
              <span class="badge badge-pass">passing</span>
            {{else}}
              <span class="badge badge-info">pending</span>
            {{end}}
          </td>
        </tr>
        {{end}}
      {{end}}
    </tbody>
  </table>
</div>
{{end}}

<!-- Findings -->
{{if .Findings}}
<div class="section">
  <h2>Findings</h2>
  <table>
    <thead>
      <tr>
        <th>Property</th>
        <th>Type</th>
        <th>Status</th>
        <th>Failed Count</th>
      </tr>
    </thead>
    <tbody>
      {{range .Findings}}
      <tr>
        <td>{{.Message}}</td>
        <td class="mono text-muted">{{.AssertType}}</td>
        <td>
          {{if eq .Status "new"}}
            <span class="badge badge-fail">new</span>
          {{else if eq .Status "ongoing"}}
            <span class="badge badge-fail">ongoing</span>
          {{else if eq .Status "resolved"}}
            <span class="badge badge-pass">resolved</span>
          {{else}}
            <span class="badge badge-info">{{.Status}}</span>
          {{end}}
        </td>
        <td class="mono">{{itoa .FailedCount}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
</div>
{{end}}

<!-- SometimesAll Progress -->
{{if .SometimesAllProgress}}
<div class="section">
  <h2>SometimesAll Sub-goal Progress</h2>
  {{range .SometimesAllProgress}}
  <div style="margin-bottom:1.25rem">
    <div style="display:flex;align-items:center;gap:.75rem;margin-bottom:.5rem">
      <span style="font-weight:600">{{.Message}}</span>
      {{if .Satisfied}}
        <span class="badge badge-pass">all satisfied</span>
      {{else}}
        <span class="badge badge-info">in progress</span>
      {{end}}
    </div>
    <table style="width:100%">
      <thead>
        <tr><th>Sub-goal</th><th>Status</th></tr>
      </thead>
      <tbody>
        {{range .SubGoals}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td>
            {{if .Satisfied}}
              <span class="badge badge-pass">satisfied</span>
            {{else}}
              <span class="badge badge-info">not yet seen</span>
            {{end}}
          </td>
        </tr>
        {{end}}
      </tbody>
    </table>
  </div>
  {{end}}
</div>
{{end}}

<!-- Run Info -->
<div class="section">
  <h2>Run Information</h2>
  <table>
    <thead>
      <tr><th>Field</th><th>Value</th></tr>
    </thead>
    <tbody>
      <tr><td>Run ID</td><td class="mono">{{.RunID}}</td></tr>
      <tr><td>Seed</td><td class="mono">{{u64toa .Seed}}</td></tr>
      {{if .ProjectName}}<tr><td>Project</td><td>{{.ProjectName}}</td></tr>{{end}}
      {{if .Description}}<tr><td>Description</td><td>{{.Description}}</td></tr>{{end}}
      <tr><td>Created At</td><td class="mono">{{.CreatedAt.Format "2006-01-02T15:04:05Z"}}</td></tr>
    </tbody>
  </table>
</div>

</main>

<footer>
  Generated by <strong>OpenThesis</strong> &mdash; deterministic distributed system testing
</footer>

</body>
</html>`
