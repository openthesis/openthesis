package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/reporthtml"
)

// CmdServe serves the HTML report for a given run locally and optionally opens
// the browser.
//
// Usage:
//
//	openthesis serve --report run-42-...   (specific run ID)
//	openthesis serve                        (latest run in state-dir)
//	openthesis serve --port 9090            (custom port)
//
// The HTML report is served at / and the raw JSON at /api/report.json.
// Press Ctrl+C to stop the server.
func CmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	runID := fs.String("report", "", "run ID to serve (default: latest)")
	port := fs.Int("port", 8080, "port to listen on (0 = random free port)")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory containing reports/")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser automatically")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis serve [--report <run-id>] [--port 8080] [--state-dir <dir>]

Serves an HTML run report on a local HTTP server and opens it in the browser.
With no --report flag, the latest report in the state directory is served.

The HTML report is available at / and raw JSON at /api/report.json.
Press Ctrl+C to stop the server.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Locate the report JSON file.
	jsonPath, err := resolveReportPath(*stateDir, *runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Load the report so we can generate (or find) the HTML counterpart.
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read report: %v\n", err)
		return 1
	}
	var rpt report.Report
	if err := json.Unmarshal(data, &rpt); err != nil {
		fmt.Fprintf(os.Stderr, "error: parse report: %v\n", err)
		return 1
	}

	// Derive HTML path: same base name, .html extension.
	htmlPath := strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".html"
	// Derive events path: <state-dir>/<run-id>/events.jsonl
	eventsPath := filepath.Join(*stateDir, rpt.RunID, "events.jsonl")
	if _, err := os.Stat(htmlPath); errors.Is(err, os.ErrNotExist) {
		if err := reporthtml.Generate(&rpt, htmlPath, reporthtml.GenerateOptions{EventsPath: eventsPath}); err != nil {
			fmt.Fprintf(os.Stderr, "error: generate HTML report: %v\n", err)
			return 1
		}
	}

	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read HTML report: %v\n", err)
		return 1
	}

	// Bind the listener - try the requested port, fall back to a random free port.
	ln, listenPort, err := bindListener(*port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bind listener: %v\n", err)
		return 1
	}

	addr := fmt.Sprintf("http://localhost:%d", listenPort)

	mux := http.NewServeMux()

	// / → HTML report
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(htmlData)
	})

	// /api/report.json → raw JSON
	mux.HandleFunc("/api/report.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	// /api/events → queryable event log for this run
	// Query params: type (repeatable), container, snapshot_id, from_step, limit (default 200)
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := eventstore.Filter{
			Types:     q["type"],
			Container: q.Get("container"),
			Limit:     200,
		}
		if s := q.Get("snapshot_id"); s != "" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				f.SnapshotID = v
			}
		}
		if s := q.Get("from_step"); s != "" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				f.FromStep = v
			}
		}
		if s := q.Get("limit"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				f.Limit = v
			}
		}
		events, err := eventstore.Query(eventsPath, f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []eventstore.Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		enc := json.NewEncoder(w)
		_ = enc.Encode(events)
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "Serving report at %s - press Ctrl+C to stop\n", addr)
	fmt.Fprintf(os.Stderr, "  run      %s\n", rpt.RunID)
	if rpt.Summary.BugsFound > 0 {
		fmt.Fprintf(os.Stderr, "  status   FAIL (%d violation(s))\n", rpt.Summary.BugsFound)
	} else {
		fmt.Fprintf(os.Stderr, "  status   PASS\n")
	}
	fmt.Fprintf(os.Stderr, "  states   %d  edges %d\n", rpt.Summary.TotalStates, rpt.Coverage.TotalEdges)
	fmt.Fprintf(os.Stderr, "  JSON     %s/api/report.json\n", addr)
	fmt.Fprintf(os.Stderr, "  events   %s/api/events\n", addr)

	if !*noBrowser {
		// Small delay so the server is listening before the browser fires.
		go func() {
			time.Sleep(200 * time.Millisecond)
			openBrowser(addr)
		}()
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		return 1
	}
	return 0
}

// resolveReportPath locates the JSON report file.
// If runID is set, it looks for <stateDir>/reports/<runID>.json.
// Otherwise it returns the most recently modified .json file under <stateDir>/reports/.
func resolveReportPath(stateDir, runID string) (string, error) {
	reportsDir := filepath.Join(stateDir, "reports")
	if _, err := os.Stat(reportsDir); errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reports directory not found: %s (run 'openthesis run' first)", reportsDir)
	}

	if runID != "" {
		// Accept both "run-42-..." and "<runID>.json".
		candidate := filepath.Join(reportsDir, runID+".json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		// Try treating runID as the literal filename.
		if _, err := os.Stat(runID); err == nil {
			return runID, nil
		}
		return "", fmt.Errorf("report not found for run %q in %s", runID, reportsDir)
	}

	// Find latest .json file by modification time.
	entries, err := os.ReadDir(reportsDir)
	if err != nil {
		return "", fmt.Errorf("read reports directory: %w", err)
	}
	var jsonFiles []os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		jsonFiles = append(jsonFiles, info)
	}
	if len(jsonFiles) == 0 {
		return "", fmt.Errorf("no reports found in %s (run 'openthesis run' first)", reportsDir)
	}
	sort.Slice(jsonFiles, func(i, j int) bool {
		return jsonFiles[i].ModTime().After(jsonFiles[j].ModTime())
	})
	return filepath.Join(reportsDir, jsonFiles[0].Name()), nil
}

// bindListener tries to bind on the requested port. If the port is busy or 0,
// it falls back to a random OS-assigned port.
func bindListener(port int) (net.Listener, int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if port == 0 {
			return nil, 0, fmt.Errorf("listen on random port: %w", err)
		}
		// Port busy - try a random free port.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, 0, fmt.Errorf("listen on fallback port: %w", err)
		}
	}
	assignedPort := ln.Addr().(*net.TCPAddr).Port
	return ln, assignedPort, nil
}

// openBrowser launches the system default browser for the given URL.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		// Linux and anything else.
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stderr = nil
	cmd.Stdout = nil
	_ = cmd.Start()
}
