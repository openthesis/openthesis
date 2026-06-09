package api

import (
	"errors"
	"net/http"
	"slices"
	"time"
)

var supportedBackends = []string{"tcg", "patched", "gvisor"}

type Environment struct {
	ID             string         `json:"id"`
	ProjectID      string         `json:"project_id"`
	Name           string         `json:"name"`
	Backend        string         `json:"backend"`
	ComposeFile    string         `json:"compose_file,omitempty"`
	ComposeContent string         `json:"compose_file_content,omitempty"`
	Images         []ImageRef     `json:"images,omitempty"`
	Nodes          []NodeDef      `json:"nodes,omitempty"`
	TestDir        string         `json:"test_dir,omitempty"`
	Resources      ResourceLimits `json:"resources,omitempty"`
	Version        int            `json:"version"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type ImageRef struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

type NodeDef struct {
	Name       string            `json:"name"`
	Image      string            `json:"image,omitempty"`
	Binary     string            `json:"binary,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	ReadyProbe string            `json:"ready_probe,omitempty"`
	Daemon     *bool             `json:"daemon,omitempty"`
}

type ResourceLimits struct {
	MemoryMB int `json:"memory_mb,omitempty"`
	VCPUs    int `json:"vcpus,omitempty"`
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	envs, err := s.store.ListEnvironments(pid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list environments", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}

	total := len(envs)
	envs, nextCursor := applyPagination(envs, page)
	writeListJSON(w, "environments", total, envs, nextCursor)
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}

	var body struct {
		ID             string         `json:"id"`
		Name           string         `json:"name"`
		Backend        string         `json:"backend"`
		ComposeFile    string         `json:"compose_file"`
		ComposeContent string         `json:"compose_file_content"`
		Images         []ImageRef     `json:"images"`
		Nodes          []NodeDef      `json:"nodes"`
		TestDir        string         `json:"test_dir"`
		Resources      ResourceLimits `json:"resources"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.Name == "" {
		writeAPIError(w, r, ErrCodeMissingField, "name is required", "")
		return
	}
	if body.Backend == "" {
		writeAPIError(w, r, ErrCodeMissingField, "backend is required", "provide one of: tcg, patched, gvisor")
		return
	}
	if !slices.Contains(supportedBackends, body.Backend) {
		writeAPIError(w, r, ErrCodeInvalidField, "invalid backend", "backend must be one of: tcg, patched, gvisor")
		return
	}

	envID, err := toResourceID("env", body.ID)
	if err != nil {
		writeAPIError(w, r, ErrCodeSlugInvalid, err.Error(), "")
		return
	}
	if _, err := s.store.GetEnvironment(pid, envID); err == nil {
		writeAPIError(w, r, ErrCodeSlugConflict, "environment id already exists", "choose a different id")
		return
	} else if !errors.Is(err, errNotFound) {
		writeAPIError(w, r, ErrCodeInternalError, "failed to check environment id", "")
		return
	}

	now := time.Now().UTC()
	env := Environment{
		ID:             envID,
		ProjectID:      pid,
		Name:           body.Name,
		Backend:        body.Backend,
		ComposeFile:    body.ComposeFile,
		ComposeContent: body.ComposeContent,
		Images:         body.Images,
		Nodes:          body.Nodes,
		TestDir:        body.TestDir,
		Resources:      body.Resources,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.SaveEnvironment(env); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save environment", "")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, env)
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	eid := r.PathValue("environment_id")
	env, err := s.store.GetEnvironment(pid, eid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
		return
	}
	writeJSON(w, env)
}

func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	eid := r.PathValue("environment_id")
	env, err := s.store.GetEnvironment(pid, eid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
		return
	}

	var body struct {
		Name           *string         `json:"name"`
		Backend        *string         `json:"backend"`
		ComposeFile    *string         `json:"compose_file"`
		ComposeContent *string         `json:"compose_file_content"`
		Images         []ImageRef      `json:"images"`
		Nodes          []NodeDef       `json:"nodes"`
		TestDir        *string         `json:"test_dir"`
		Resources      *ResourceLimits `json:"resources"`
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
		env.Name = *body.Name
	}
	if body.Backend != nil {
		if !slices.Contains(supportedBackends, *body.Backend) {
			writeAPIError(w, r, ErrCodeInvalidField, "invalid backend", "backend must be one of: tcg, patched, gvisor")
			return
		}
		env.Backend = *body.Backend
	}
	if body.ComposeFile != nil {
		env.ComposeFile = *body.ComposeFile
	}
	if body.ComposeContent != nil {
		env.ComposeContent = *body.ComposeContent
	}
	if body.Images != nil {
		env.Images = body.Images
	}
	if body.Nodes != nil {
		env.Nodes = body.Nodes
	}
	if body.TestDir != nil {
		env.TestDir = *body.TestDir
	}
	if body.Resources != nil {
		env.Resources = *body.Resources
	}
	env.Version++
	env.UpdatedAt = time.Now().UTC()

	if err := s.store.SaveEnvironment(env); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save environment", "")
		return
	}
	writeJSON(w, env)
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	eid := r.PathValue("environment_id")
	if _, err := s.store.GetEnvironment(pid, eid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
		return
	}
	if err := s.store.DeleteEnvironment(pid, eid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete environment", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleValidateEnvironment(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	eid := r.PathValue("environment_id")

	env, err := s.store.GetEnvironment(pid, eid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
		return
	}

	if len(env.Nodes) == 0 && env.ComposeFile == "" && env.ComposeContent == "" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "environment has no nodes or compose definition", "provide nodes or compose_file")
		return
	}

	writeJSON(w, map[string]any{
		"status": "valid",
		"checks": map[string]any{
			"backend":      env.Backend,
			"nodes":        len(env.Nodes),
			"has_compose":  env.ComposeFile != "" || env.ComposeContent != "",
			"ready_probes": len(env.Nodes),
		},
	})
}
