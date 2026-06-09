package api

import (
	"errors"
	"net/http"
	"time"
)

type Project struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Stats       ProjectStats `json:"stats,omitempty"`
}

type ProjectStats struct {
	Tests         int `json:"tests"`
	ActiveRuns    int `json:"active_runs"`
	TotalFindings int `json:"total_findings"`
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list projects", "")
		return
	}

	for i := range projects {
		s.fillProjectStats(&projects[i])
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}

	total := len(projects)
	projects, nextCursor := applyPagination(projects, page)
	writeListJSON(w, "projects", total, projects, nextCursor)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.Name == "" {
		writeAPIError(w, r, ErrCodeMissingField, "name is required", "provide a name for the project")
		return
	}
	projectID, err := toResourceID("proj", body.ID)
	if err != nil {
		writeAPIError(w, r, ErrCodeSlugInvalid, err.Error(), "")
		return
	}
	if _, err := s.store.GetProject(projectID); err == nil {
		writeAPIError(w, r, ErrCodeSlugConflict, "project id already exists", "choose a different id")
		return
	} else if !errors.Is(err, errNotFound) {
		writeAPIError(w, r, ErrCodeInternalError, "failed to check project id", "")
		return
	}

	now := time.Now().UTC()
	p := Project{
		ID:          projectID,
		Name:        body.Name,
		Description: body.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.SaveProject(p); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save project", "")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	p, err := s.store.GetProject(pid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	s.fillProjectStats(&p)
	writeJSON(w, p)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	p, err := s.store.GetProject(pid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}

	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.Name != nil {
		if *body.Name == "" {
			writeAPIError(w, r, ErrCodeInvalidField, "name cannot be empty", "")
			return
		}
		p.Name = *body.Name
	}
	if body.Description != nil {
		p.Description = *body.Description
	}
	p.UpdatedAt = time.Now().UTC()

	if err := s.store.SaveProject(p); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save project", "")
		return
	}
	writeJSON(w, p)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if r.URL.Query().Get("confirm") != "true" {
		writeAPIError(w, r, ErrCodeInvalidField, "confirm=true is required to delete a project", "retry with DELETE /api/v1/projects/{id}?confirm=true")
		return
	}
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	if err := s.store.DeleteProject(pid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete project", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProjectSummary(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	p, err := s.store.GetProject(pid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	s.fillProjectStats(&p)
	writeJSON(w, map[string]any{
		"project_id": p.ID,
		"stats":      p.Stats,
	})
}

func (s *Server) fillProjectStats(p *Project) {
	tests, _ := s.store.ListTests(p.ID)
	p.Stats.Tests = len(tests)
	for _, t := range tests {
		runs, _ := s.store.ListRuns(p.ID, t.ID)
		for _, run := range runs {
			if run.Status == "running" {
				p.Stats.ActiveRuns++
			}
		}
		findings, _ := s.store.ListFindings(p.ID, t.ID)
		p.Stats.TotalFindings += len(findings)
	}
}
