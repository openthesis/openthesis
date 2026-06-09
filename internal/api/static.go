package api

import (
	"io/fs"
	"net/http"
	"strings"
)

// webFS holds the embedded web UI filesystem.
// Set via SetWebFS() at startup. If nil, the dashboard is not available.
var webFS fs.FS

// SetWebFS sets the filesystem used to serve the web UI.
// Called from main.go with the embedded web/dist directory.
func SetWebFS(f fs.FS) {
	webFS = f
}

func (s *Server) handleStaticFiles(w http.ResponseWriter, r *http.Request) {
	if webFS == nil {
		writeJSON(w, map[string]string{
			"status":  "ok",
			"message": "OpenThesis API. Dashboard not built; run 'cd web && bun run build' to enable.",
		})
		return
	}

	path := r.URL.Path
	if path == "/" {
		path = "index.html"
	} else {
		path = strings.TrimPrefix(path, "/")
	}

	// Try to serve the file directly.
	if _, err := fs.Stat(webFS, path); err == nil {
		http.ServeFileFS(w, r, webFS, path)
		return
	}

	// SPA fallback: serve index.html for any path that doesn't match a file.
	// This allows client-side routing to work.
	http.ServeFileFS(w, r, webFS, "index.html")
}
