// Package reporthtml renders a report.Report as a self-contained HTML file.
//
// The output is a single document with all styles inlined. No external
// JavaScript, CSS, or chart frameworks are loaded; the coverage sparkline
// is drawn as static SVG and the snapshot tree uses the browser's native
// <details> element for collapse/expand interactions.
//
// Usage:
//
//	err := reporthtml.Generate(rpt, "report.html")
//
// The caller is responsible for creating the parent directory.
package reporthtml

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/report"
)

//go:embed template.html
var templateFS embed.FS

// htmlTmpl is compiled once at init. Template parsing errors here indicate
// a broken template.html in source and are fatal.
var htmlTmpl = template.Must(
	template.New("report.html").Funcs(funcMap()).ParseFS(templateFS, "template.html"),
)

// GenerateOptions are optional parameters for Generate.
type GenerateOptions struct {
	// EventsPath is the path to the run's events.jsonl file. When set, up to
	// 300 key events (violations, assertions, faults, lifecycle) are embedded
	// in the HTML for offline browsing. Ignored if the file does not exist.
	EventsPath string
}

// violationMeta carries pre-computed per-violation display data that is
// expensive or impossible to derive inside a Go template.
type violationMeta struct {
	IsRare  bool          // PSurvival < 33 from a matching BugReport
	ProbSVG template.HTML // probability-over-time SVG chart; empty if no data
}

// viewData is the payload passed to the template.
type viewData struct {
	Report        *report.Report
	Backend       string
	CoverageSVG   template.HTML
	TreeSVG       template.HTML
	Events        []eventstore.Event
	HasEvents     bool
	ViolationMeta []violationMeta
}

// Generate writes the HTML report for r to outPath. The file is created
// atomically (via a temporary sibling + rename) so a reader can never see
// half-written output. Pass a GenerateOptions to embed events.
func Generate(r *report.Report, outPath string, opts ...GenerateOptions) error {
	if r == nil {
		return fmt.Errorf("reporthtml: nil report")
	}

	var opt GenerateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	var buf bytes.Buffer
	if err := RenderWithOptions(&buf, r, opt); err != nil {
		return err
	}

	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("reporthtml: write tmp: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("reporthtml: rename: %w", err)
	}
	return nil
}

// Render writes the HTML report for r directly to w (no events embedded).
func Render(w io.Writer, r *report.Report) error {
	return RenderWithOptions(w, r, GenerateOptions{})
}

// RenderWithOptions writes the HTML report for r to w, optionally embedding
// events from opts.EventsPath.
func RenderWithOptions(w io.Writer, r *report.Report, opts GenerateOptions) error {
	events := loadEmbeddedEvents(opts.EventsPath)

	// Collect violation snapshot IDs for tree node coloring.
	violationSnapIDs := make(map[uint64]bool, len(r.Violations))
	violationSteps := make([]uint64, 0, len(r.Violations))
	for _, v := range r.Violations {
		violationSnapIDs[uint64(v.SnapshotID)] = true
		if v.Step > 0 {
			violationSteps = append(violationSteps, v.Step)
		}
	}

	data := viewData{
		Report:        r,
		Backend:       deriveBackend(r),
		CoverageSVG:   template.HTML(renderCoverageSVGWithMarkers(r.Coverage.TimeSeries, violationSteps)),
		TreeSVG:       template.HTML(renderTreeSVG(r.Tree, violationSnapIDs)),
		Events:        events,
		HasEvents:     len(events) > 0,
		ViolationMeta: buildViolationMeta(r),
	}
	if err := htmlTmpl.ExecuteTemplate(w, "template.html", data); err != nil {
		return fmt.Errorf("reporthtml: execute template: %w", err)
	}
	return nil
}

// loadEmbeddedEvents loads up to 300 key events from path for embedding in
// standalone HTML. Prioritises violations, then assertions, then faults and
// lifecycle events. Returns nil if path is empty or does not exist.
func loadEmbeddedEvents(path string) []eventstore.Event {
	if path == "" {
		return nil
	}
	// First pass: violations + lifecycle (always include).
	priority, _ := eventstore.Query(path, eventstore.Filter{
		Types: []string{
			eventstore.TypeSDKViolation,
			eventstore.TypeLifecycle,
			eventstore.TypeFaultApplied,
		},
		Limit: 150,
	})
	// Second pass: assertion evals to fill remaining budget.
	remaining := 300 - len(priority)
	if remaining > 0 {
		asserts, _ := eventstore.Query(path, eventstore.Filter{
			Types: []string{eventstore.TypeSDKAssert},
			Limit: remaining,
		})
		priority = append(priority, asserts...)
	}
	return priority
}

// deriveBackend returns the backend string recorded on the first violation
// artifact, or "" if none were produced. Useful for a no-violation run the
// backend ends up blank, which is fine; the template hides that row when
// the field is empty.
func deriveBackend(r *report.Report) string {
	for _, v := range r.Violations {
		if v.Artifact != nil && v.Artifact.Backend != "" {
			return v.Artifact.Backend
		}
	}
	return ""
}

// Template helper functions

// buildViolationMeta matches each violation against BugReports to compute
// per-violation display data (rare classification + probability chart).
func buildViolationMeta(r *report.Report) []violationMeta {
	meta := make([]violationMeta, len(r.Violations))
	for i, v := range r.Violations {
		for _, br := range r.BugReports {
			if br.Message != v.Message {
				continue
			}
			meta[i].IsRare = br.PSurvival > 0 && br.PSurvival < 33
			if len(br.Timeline) >= 2 {
				meta[i].ProbSVG = template.HTML(renderProbabilitySVG(br.Timeline))
			}
			break
		}
	}
	return meta
}

// renderProbabilitySVG draws a compact 600x80 SVG line chart of bug probability
// over virtual time. Sharp vertical jumps identify causal events - the moment
// the bug became much more likely.
func renderProbabilitySVG(points []report.BugProbabilityPoint) string {
	if len(points) < 2 {
		return ""
	}

	const (
		w      = 600.0
		h      = 80.0
		padL   = 36.0
		padR   = 12.0
		padT   = 8.0
		padB   = 20.0
		innerW = w - padL - padR
		innerH = h - padT - padB
	)

	minT := points[0].VTime
	maxT := points[0].VTime
	for _, p := range points {
		if p.VTime < minT {
			minT = p.VTime
		}
		if p.VTime > maxT {
			maxT = p.VTime
		}
	}
	tRange := maxT - minT
	if tRange == 0 {
		tRange = 1
	}

	xOf := func(t float64) float64 {
		return padL + (t-minT)/tRange*innerW
	}
	yOf := func(prob float64) float64 {
		return padT + innerH*(1-prob/100)
	}

	// Find the largest probability jump - this is the inception point.
	inceptionIdx := -1
	maxJump := 0.0
	for i := 1; i < len(points); i++ {
		jump := points[i].Probability - points[i-1].Probability
		if jump > maxJump {
			maxJump = jump
			inceptionIdx = i
		}
	}

	var lineBuf bytes.Buffer
	for i, p := range points {
		if i > 0 {
			lineBuf.WriteByte(' ')
		}
		fmt.Fprintf(&lineBuf, "%.2f,%.2f", xOf(p.VTime), yOf(p.Probability))
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" xmlns="http://www.w3.org/2000/svg" style="width:100%%;height:80px;display:block;margin-top:8px">`, w, h)

	// Grid lines at 0%, 50%, 100%.
	for _, pct := range []float64{0, 50, 100} {
		y := yOf(pct)
		fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#3d3642" stroke-width="1" stroke-dasharray="3,3"/>`, padL, y, w-padR, y)
		fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="9" fill="#9089a0" text-anchor="end" font-family="monospace">%.0f%%</text>`, padL-3, y+3, pct)
	}

	// Inception marker - vertical amber line at the biggest jump.
	if inceptionIdx >= 0 {
		x := xOf(points[inceptionIdx].VTime)
		fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#f59e0b" stroke-width="1.5" stroke-dasharray="4,2" opacity="0.8"/>`, x, padT, x, padT+innerH)
		fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="8" fill="#f59e0b" text-anchor="middle" font-family="monospace">inception</text>`, x, padT-1)
	}

	// Fill area under curve.
	xLast := xOf(points[len(points)-1].VTime)
	xFirst := xOf(points[0].VTime)
	fmt.Fprintf(&b, `<polygon points="%s %.2f,%.2f %.2f,%.2f" fill="#ef4444" fill-opacity="0.08"/>`,
		lineBuf.String(), xLast, padT+innerH, xFirst, padT+innerH)

	// Line.
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#ef4444" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>`, lineBuf.String())

	// Dots at each probe.
	for _, p := range points {
		fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="2.5" fill="#ef4444"/>`, xOf(p.VTime), yOf(p.Probability))
	}

	// X-axis and Y-axis labels.
	fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="9" fill="#9089a0" text-anchor="start" font-family="monospace">virtual time →</text>`, padL, h-3)
	fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="9" fill="#9089a0" text-anchor="start" font-family="monospace">P(bug)</text>`, 2.0, padT+innerH/2+3)

	b.WriteString(`</svg>`)
	return b.String()
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"formatDuration":  formatDuration,
		"totalAssertions": totalAssertions,
		"totalProperties": totalProperties,
		"passRate":        passRate,
		"pct":             pct,
		"joinUint64":      joinUint64,
		"lastStep":        lastStep,
		"add":             func(a, b int) int { return a + b },
		"vmeta": func(meta []violationMeta, i int) violationMeta {
			if i >= 0 && i < len(meta) {
				return meta[i]
			}
			return violationMeta{}
		},
		"countFindings":  countFindings,
		"payloadText":    payloadText,
		"formatVTime":    formatVTime,
		"typeBadgeClass": typeBadgeClass,
	}
}

// payloadText renders an event payload map as a compact key=value string.
func payloadText(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	// Stable key order: message/condition first, then alphabetical.
	priority := []string{"message", "name", "condition", "passed", "property"}
	seen := make(map[string]bool)
	var parts []string
	for _, k := range priority {
		if v, ok := payload[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		v := fmt.Sprintf("%v", payload[k])
		if len(v) > 80 {
			v = v[:77] + "..."
		}
		parts = append(parts, k+"="+v)
	}
	result := strings.Join(parts, "  ")
	if len(result) > 200 {
		result = result[:197] + "..."
	}
	return result
}

// formatVTime formats a VTimeNS uint64 as a compact virtual-time string.
func formatVTime(ns uint64) string {
	if ns == 0 {
		return "0"
	}
	d := time.Duration(ns)
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(ns)/1e3)
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(ns)/1e6)
	}
	return fmt.Sprintf("%.2fs", float64(ns)/1e9)
}

// typeBadgeClass maps an event type constant to a CSS class.
func typeBadgeClass(t string) string {
	switch t {
	case "sdk_violation":
		return "type-violation"
	case "sdk_assert":
		return "type-assert"
	case "fault_applied":
		return "type-fault"
	case "lifecycle":
		return "type-lifecycle"
	case "coverage_burst":
		return "type-coverage"
	default:
		return "type-other"
	}
}

// countFindings returns the number of findings with the given status.
func countFindings(findings []report.Finding, status string) int {
	n := 0
	for _, f := range findings {
		if f.Status == status {
			n++
		}
	}
	return n
}

// formatDuration renders a time.Duration as a compact "1h2m3s" string,
// dropping fractional seconds. Short durations fall back to "NNNms".
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	// Truncate to whole seconds for readability.
	d = d.Truncate(time.Second)
	return d.String()
}

func totalAssertions(a report.AssertionSummary) int {
	return a.Always.Total + a.Sometimes.Total + a.Reachable.Total
}

func totalProperties(groups []report.PropertyGroup) int {
	total := 0
	for _, g := range groups {
		total += g.Total
	}
	return total
}

// passRate formats a passed/total ratio as "XX.X%".
func passRate(passed, total int) string {
	if total <= 0 {
		return "0.0%"
	}
	p := float64(passed) / float64(total) * 100.0
	return pct(p)
}

// pct rounds a percentage to one decimal place and renders it as "XX.X%".
func pct(p float64) string {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	// Round half-up to one decimal.
	r := int(p*10 + 0.5)
	whole := r / 10
	frac := r % 10
	return strconv.Itoa(whole) + "." + strconv.Itoa(frac) + "%"
}

// joinUint64 renders a []uint64 as a separator-joined string. It is returned
// as template.HTML because the separator may contain HTML entities like
// "&rarr;" that should not be escaped.
func joinUint64(ids []uint64, sep string) template.HTML {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return template.HTML(strings.Join(parts, sep))
}

// lastStep returns the step of the final coverage point, or 0 if empty.
func lastStep(points []report.CoveragePoint) uint64 {
	if len(points) == 0 {
		return 0
	}
	return points[len(points)-1].Step
}

// Coverage sparkline SVG

// renderCoverageSVGWithMarkers draws a SVG line+area chart of coverage over
// time with optional red vertical markers at violation steps.
// Viewport is 1000x200 units; rendered at 100% width.
func renderCoverageSVGWithMarkers(points []report.CoveragePoint, violationSteps []uint64) string {
	if len(points) == 0 {
		return ""
	}

	const (
		w      = 1000.0
		h      = 200.0
		padL   = 44.0
		padR   = 20.0
		padT   = 14.0
		padB   = 26.0
		innerW = w - padL - padR
		innerH = h - padT - padB
	)

	minStep := points[0].Step
	maxStep := points[0].Step
	var maxEdges uint64
	for _, p := range points {
		if p.Step < minStep {
			minStep = p.Step
		}
		if p.Step > maxStep {
			maxStep = p.Step
		}
		if p.Edges > maxEdges {
			maxEdges = p.Edges
		}
	}
	stepRange := float64(maxStep - minStep)
	if stepRange == 0 {
		stepRange = 1
	}
	edgeMax := float64(maxEdges)
	if edgeMax == 0 {
		edgeMax = 1
	}

	xOf := func(step uint64) float64 {
		return padL + float64(step-minStep)/stepRange*innerW
	}
	yOf := func(edges uint64) float64 {
		return padT + innerH - float64(edges)/edgeMax*innerH
	}

	var lineBuf, areaBuf bytes.Buffer
	for i, p := range points {
		x := xOf(p.Step)
		y := yOf(p.Edges)
		if i > 0 {
			lineBuf.WriteByte(' ')
			areaBuf.WriteByte(' ')
		}
		fmt.Fprintf(&lineBuf, "%.2f,%.2f", x, y)
		fmt.Fprintf(&areaBuf, "%.2f,%.2f", x, y)
	}
	xLast := xOf(points[len(points)-1].Step)
	xFirst := xOf(points[0].Step)
	fmt.Fprintf(&areaBuf, " %.2f,%.2f %.2f,%.2f", xLast, padT+innerH, xFirst, padT+innerH)

	gridEdges := []uint64{0, maxEdges / 2, maxEdges}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %d %d" preserveAspectRatio="none" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="Coverage timeline">`,
		int(w), int(h))

	for _, g := range gridEdges {
		y := yOf(g)
		fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#3d3642" stroke-width="1" stroke-dasharray="4,4"/>`, padL, y, w-padR, y)
		fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="11" fill="#9089a0" text-anchor="end" font-family="monospace">%d</text>`, padL-5, y+3.5, g)
	}

	// Violation step markers (vertical red lines).
	for _, vs := range violationSteps {
		if vs >= minStep && vs <= maxStep {
			x := xOf(vs)
			fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#ef4444" stroke-width="1.5" stroke-dasharray="3,2" opacity="0.8"/>`, x, padT, x, padT+innerH)
			fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="3.5" fill="#ef4444" opacity="0.9"/>`, x, padT)
		}
	}

	fmt.Fprintf(&b, `<polygon points="%s" fill="#b8a9d4" fill-opacity="0.12"/>`, areaBuf.String())
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#b8a9d4" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`, lineBuf.String())

	last := points[len(points)-1]
	fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="4" fill="#9d8dbe" stroke="#0f0d11" stroke-width="2"/>`, xOf(last.Step), yOf(last.Edges))
	fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="10" fill="#9089a0" text-anchor="start" font-family="monospace">edges</text>`, padL, padT-3)

	b.WriteString(`</svg>`)
	return b.String()
}

// Snapshot tree SVG

const treeMaxNodes = 250

// renderTreeSVG renders the exploration snapshot tree as an SVG graph.
// Nodes are colored: red=violation, amber=faults only, teal=new edges, grey=default.
// Layout uses a dendrogram approach: leaves get sequential Y slots, parents
// are centered between their first and last child.
// The result is truncated to treeMaxNodes if the tree is large.
func renderTreeSVG(nodes []report.TreeNode, violationSnapIDs map[uint64]bool) string {
	if len(nodes) == 0 {
		return ""
	}

	byID := make(map[uint64]int, len(nodes))
	for i, n := range nodes {
		byID[n.ID] = i
	}
	children := make(map[uint64][]int, len(nodes))
	var roots []int
	for i, n := range nodes {
		if _, ok := byID[n.ParentID]; !ok || n.ParentID == n.ID || n.ParentID == 0 {
			roots = append(roots, i)
		} else {
			children[n.ParentID] = append(children[n.ParentID], i)
		}
	}

	// Assign Y positions via DFS: leaves get sequential slots, parents center
	// between their first and last child. Capped at treeMaxNodes.
	posY := make(map[uint64]float64, len(nodes))
	posX := make(map[uint64]float64, len(nodes))
	included := make(map[uint64]bool, treeMaxNodes)
	leafCounter := 0
	nodeCount := 0

	var assignY func(idx int, depth int)
	assignY = func(idx int, depth int) {
		if nodeCount >= treeMaxNodes {
			return
		}
		n := nodes[idx]
		nodeCount++
		included[n.ID] = true

		childIdxs := children[n.ID]
		if len(childIdxs) == 0 {
			posY[n.ID] = float64(leafCounter)
			posX[n.ID] = float64(depth)
			leafCounter++
			return
		}
		firstY := -1.0
		lastY := 0.0
		for _, childIdx := range childIdxs {
			if nodeCount >= treeMaxNodes {
				break
			}
			assignY(childIdx, depth+1)
			cy := posY[nodes[childIdx].ID]
			if firstY < 0 {
				firstY = cy
			}
			lastY = cy
		}
		if firstY < 0 {
			firstY = float64(leafCounter)
			posY[n.ID] = firstY
			posX[n.ID] = float64(depth)
			leafCounter++
			return
		}
		posY[n.ID] = (firstY + lastY) / 2
		posX[n.ID] = float64(depth)
	}
	for _, rootIdx := range roots {
		if nodeCount >= treeMaxNodes {
			break
		}
		assignY(rootIdx, 0)
	}

	const (
		nodeR  = 6.0
		hStep  = 72.0
		vStep  = 22.0
		padL   = 20.0
		padT   = 16.0
		padR   = 20.0
		padBot = 16.0
	)

	// Find bounding box.
	maxDepth := 0.0
	maxLeafY := 0.0
	for id, x := range posX {
		if !included[id] {
			continue
		}
		if x > maxDepth {
			maxDepth = x
		}
		if y := posY[id]; y > maxLeafY {
			maxLeafY = y
		}
	}

	svgW := padL + maxDepth*hStep + nodeR*2 + padR
	svgH := padT + maxLeafY*vStep + nodeR + padBot
	if svgH < 80 {
		svgH = 80
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg class="tree-svg" viewBox="0 0 %.0f %.0f" xmlns="http://www.w3.org/2000/svg">`, svgW, svgH)

	// Edges.
	b.WriteString(`<g>`)
	for _, n := range nodes {
		if !included[n.ID] || !included[n.ParentID] {
			continue
		}
		px := padL + posX[n.ParentID]*hStep
		py := padT + posY[n.ParentID]*vStep
		cx := padL + posX[n.ID]*hStep
		cy := padT + posY[n.ID]*vStep
		mx := (px + cx) / 2
		fmt.Fprintf(&b, `<path d="M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="none" stroke="#3d3642" stroke-width="1.5"/>`,
			px, py, mx, py, mx, cy, cx, cy)
	}
	b.WriteString(`</g>`)

	// Nodes.
	b.WriteString(`<g>`)
	for _, n := range nodes {
		if !included[n.ID] {
			continue
		}
		x := padL + posX[n.ID]*hStep
		y := padT + posY[n.ID]*vStep

		fill := "#1f1b24"
		stroke := "#524a5a"
		switch {
		case violationSnapIDs[n.ID]:
			fill = "#220c0e"
			stroke = "#ef4444"
		case n.Faults > 0 && n.NewEdges > 0:
			fill = "#221b30"
			stroke = "#9d8dbe"
		case n.Faults > 0:
			fill = "#2a1a05"
			stroke = "#f59e0b"
		case n.NewEdges > 0:
			fill = "#0d1f0f"
			stroke = "#22c55e"
		}

		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s" stroke="%s" stroke-width="1.5"><title>#%d depth=%d edges=%d faults=%d</title></circle>`,
			x, y, nodeR, fill, stroke, n.ID, n.Depth, n.NewEdges, n.Faults)
	}
	b.WriteString(`</g>`)

	b.WriteString(`</svg>`)

	if len(nodes) > treeMaxNodes {
		fmt.Fprintf(&b, `<p style="font-size:11px;color:#9089a0;margin-top:8px">Showing %d of %d nodes (DFS, capped for render performance)</p>`,
			treeMaxNodes, len(nodes))
	}

	return b.String()
}
