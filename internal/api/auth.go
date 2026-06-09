package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

type ctxKeyRequestID struct{}
type ctxKeyAPIKey struct{}

type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Prefix    string     `json:"prefix"`
	LastFour  string     `json:"last_four"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

var validAPIKeyTypes = []string{"live", "readonly"}

// authMiddleware checks the Authorization: Bearer <key> header.
// Routes under /api/ require a valid key; health and static files are exempt.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Inject a request ID into context for error tracing.
		rid := generateID("req")
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, rid))
		w.Header().Set("X-Request-ID", rid)

		// Bootstrap mode: if no keys are configured, allow all requests.
		// Once the first key is created, auth is enforced.
		if keys, err := s.store.ListAPIKeys(); err == nil && len(keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeAPIError(w, r, ErrCodeAuthKeyMissing, "authentication required", "set Authorization: Bearer <key> header")
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeAPIError(w, r, ErrCodeAuthKeyInvalid, "invalid authorization format", "use Authorization: Bearer <key>")
			return
		}
		rawKey := strings.TrimPrefix(auth, prefix)
		hash := hashAPIKey(rawKey)

		stored, err := s.store.FindAPIKeyByHash(hash)
		if err != nil {
			if errors.Is(err, errNotFound) {
				writeAPIError(w, r, ErrCodeAuthKeyInvalid, "invalid API key", "create a key at POST /api/v1/auth/keys")
				return
			}
			writeAPIError(w, r, ErrCodeInternalError, "failed to verify key", "")
			return
		}

		if stored.ExpiresAt != nil && time.Now().UTC().After(*stored.ExpiresAt) {
			writeAPIError(w, r, ErrCodeAuthKeyExpired, "API key has expired", "create a new key at POST /api/v1/auth/keys")
			return
		}
		if stored.Type == "readonly" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			writeAPIError(w, r, ErrCodeForbidden, "read-only key cannot modify resources", "use a live API key for write operations")
			return
		}

		r = r.WithContext(context.WithValue(r.Context(), ctxKeyAPIKey{}, stored.APIKey))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys()
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list keys", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(keys)
	keys, nextCursor := applyPagination(keys, page)
	writeListJSON(w, "keys", total, keys, nextCursor)
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string     `json:"name"`
		Type      string     `json:"type"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.Name == "" {
		writeAPIError(w, r, ErrCodeMissingField, "name is required", "")
		return
	}
	if body.Type == "" {
		body.Type = "live"
	}
	if body.Type == "api" {
		body.Type = "live"
	}
	if !slices.Contains(validAPIKeyTypes, body.Type) {
		writeAPIError(w, r, ErrCodeInvalidField, "invalid key type", "type must be one of: live, readonly")
		return
	}

	rawKey, err := generateRawKey(body.Type)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to generate key", "")
		return
	}

	hash := hashAPIKey(rawKey)
	now := time.Now().UTC()
	key := storedAPIKey{
		APIKey: APIKey{
			ID:        generateID("otk"),
			Name:      body.Name,
			Type:      body.Type,
			Prefix:    rawKey[:8],
			LastFour:  rawKey[len(rawKey)-4:],
			CreatedAt: now,
			ExpiresAt: body.ExpiresAt,
		},
		Hash: hash,
	}
	if err := s.store.SaveAPIKey(key); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save key", "")
		return
	}

	// Return the raw key exactly once.
	resp := map[string]any{
		"key":        rawKey,
		"id":         key.ID,
		"name":       key.Name,
		"type":       key.Type,
		"prefix":     key.Prefix,
		"last_four":  key.LastFour,
		"created_at": key.CreatedAt,
		"expires_at": key.ExpiresAt,
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, resp)
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	kid := r.PathValue("key_id")
	if _, err := s.store.GetAPIKey(kid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotFound, "key not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get key", "")
		return
	}
	if err := s.store.DeleteAPIKey(kid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete key", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func generateRawKey(keyType string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return "otk_" + keyType + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func hashAPIKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h)
}
