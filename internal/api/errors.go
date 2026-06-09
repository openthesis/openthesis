package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
)

const (
	ErrCodeAuthKeyMissing         = "auth_key_missing"
	ErrCodeAuthKeyInvalid         = "auth_key_invalid"
	ErrCodeAuthKeyExpired         = "auth_key_expired"
	ErrCodeForbidden              = "forbidden"
	ErrCodeNotFound               = "not_found"
	ErrCodeProjectNotFound        = "project_not_found"
	ErrCodeEnvironmentNotFound    = "environment_not_found"
	ErrCodeTestNotFound           = "test_not_found"
	ErrCodeRunNotFound            = "run_not_found"
	ErrCodeFindingNotFound        = "finding_not_found"
	ErrCodeReplayNotFound         = "replay_not_found"
	ErrCodeNotebookNotFound       = "notebook_not_found"
	ErrCodeWebhookNotFound        = "webhook_not_found"
	ErrCodeValidationError        = "validation_error"
	ErrCodeMissingField           = "missing_field"
	ErrCodeInvalidField           = "invalid_field"
	ErrCodeSlugConflict           = "slug_conflict"
	ErrCodeSlugInvalid            = "slug_invalid"
	ErrCodeTestAlreadyRunning     = "test_already_running"
	ErrCodeRunNotRunning          = "run_not_running"
	ErrCodeRunAlreadyComplete     = "run_already_complete"
	ErrCodeReplayExpired          = "replay_expired"
	ErrCodeNotebookExpired        = "notebook_expired"
	ErrCodeEnvironmentNotReady    = "environment_not_ready"
	ErrCodeBackendUnavailable     = "backend_unavailable"
	ErrCodeSnapshotMissing        = "snapshot_missing"
	ErrCodeResourceLimitExceeded  = "resource_limit_exceeded"
	ErrCodeComposeParseFailed     = "compose_parse_error"
	ErrCodeImagePullFailed        = "image_pull_failed"
	ErrCodeBinaryNotFound         = "binary_not_found"
	ErrCodeTestDirNotFound        = "test_dir_not_found"
	ErrCodeReplayTokenInvalid     = "replay_token_invalid"
	ErrCodeReplayTokenExpired     = "replay_token_expired"
	ErrCodeFindingNotReproducible = "finding_not_reproducible"
	ErrCodeInternalError          = "internal_error"
	ErrCodeMethodNotAllowed       = "method_not_allowed"
	ErrCodeConflict               = "conflict"
	ErrCodeBadRequest             = "bad_request"
)

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	Docs      string `json:"docs,omitempty"`
	RequestID string `json:"request_id"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

func errorStatus(code string) int {
	switch code {
	case ErrCodeAuthKeyMissing:
		return http.StatusUnauthorized
	case ErrCodeAuthKeyInvalid, ErrCodeAuthKeyExpired:
		return http.StatusUnauthorized
	case ErrCodeForbidden:
		return http.StatusForbidden
	case ErrCodeProjectNotFound, ErrCodeEnvironmentNotFound, ErrCodeTestNotFound,
		ErrCodeRunNotFound, ErrCodeFindingNotFound, ErrCodeReplayNotFound,
		ErrCodeNotebookNotFound, ErrCodeWebhookNotFound, ErrCodeNotFound:
		return http.StatusNotFound
	case ErrCodeSlugConflict, ErrCodeConflict, ErrCodeTestAlreadyRunning,
		ErrCodeRunAlreadyComplete:
		return http.StatusConflict
	case ErrCodeValidationError, ErrCodeMissingField, ErrCodeInvalidField,
		ErrCodeSlugInvalid, ErrCodeComposeParseFailed, ErrCodeReplayTokenInvalid,
		ErrCodeBadRequest:
		return http.StatusBadRequest
	case ErrCodeReplayTokenExpired, ErrCodeReplayExpired, ErrCodeNotebookExpired,
		ErrCodeSnapshotMissing:
		return http.StatusGone
	case ErrCodeRunNotRunning, ErrCodeEnvironmentNotReady, ErrCodeFindingNotReproducible:
		return http.StatusUnprocessableEntity
	case ErrCodeResourceLimitExceeded:
		return http.StatusTooManyRequests
	case ErrCodeBackendUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeAPIError(w http.ResponseWriter, r *http.Request, code, message, hint string) {
	reqID := requestID(r)
	status := errorStatus(code)

	resp := errorResponse{
		Error: apiError{
			Code:      code,
			Message:   message,
			Hint:      hint,
			RequestID: reqID,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to write error response", "err", err)
	}
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	if id, ok := r.Context().Value(ctxKeyRequestID{}).(string); ok {
		return id
	}
	return generateID("req")
}

func generateID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return prefix + "_fallback"
	}
	return prefix + "_" + hex.EncodeToString(b)
}
