package api

import "net/http"

func (s *Server) handleSDKLifecycle(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (s *Server) handleSDKAssertions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (s *Server) handleSDKCoverage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (s *Server) handleSDKGuidance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (s *Server) handleSDKConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"version": version,
		"api":     "/api/v1/sdk",
	})
}
