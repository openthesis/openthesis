package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Webhook struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WebhookDelivery struct {
	ID          string     `json:"id"`
	WebhookID   string     `json:"webhook_id"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	StatusCode  int        `json:"status_code,omitempty"`
	Attempts    int        `json:"attempts"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

var validWebhookEvents = map[string]struct{}{
	"run.started":     {},
	"run.completed":   {},
	"run.failed":      {},
	"finding.created": {},
	"test.started":    {},
	"test.stopped":    {},
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	hooks, err := s.store.ListWebhooks(pid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list webhooks", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(hooks)
	hooks, nextCursor := applyPagination(hooks, page)
	writeListJSON(w, "webhooks", total, hooks, nextCursor)
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
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
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Active *bool    `json:"active"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.URL == "" {
		writeAPIError(w, r, ErrCodeMissingField, "url is required", "")
		return
	}
	if !strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "http://") {
		writeAPIError(w, r, ErrCodeInvalidField, "url must start with http:// or https://", "")
		return
	}
	if len(body.Events) == 0 {
		writeAPIError(w, r, ErrCodeMissingField, "events is required", fmt.Sprintf("valid events: %v", validEventList()))
		return
	}
	for _, e := range body.Events {
		if _, ok := validWebhookEvents[e]; !ok {
			writeAPIError(w, r, ErrCodeInvalidField, fmt.Sprintf("unknown event: %s", e), fmt.Sprintf("valid events: %v", validEventList()))
			return
		}
	}

	active := true
	if body.Active != nil {
		active = *body.Active
	}

	now := time.Now().UTC()
	hook := Webhook{
		ID:        generateID("wh"),
		ProjectID: pid,
		URL:       body.URL,
		Events:    body.Events,
		Active:    active,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.SaveWebhook(hook); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save webhook", "")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, hook)
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	wid := r.PathValue("webhook_id")
	hook, err := s.store.GetWebhook(pid, wid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeWebhookNotFound, "webhook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get webhook", "")
		return
	}
	writeJSON(w, hook)
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	wid := r.PathValue("webhook_id")
	hook, err := s.store.GetWebhook(pid, wid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeWebhookNotFound, "webhook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get webhook", "")
		return
	}

	var body struct {
		URL    *string  `json:"url"`
		Events []string `json:"events"`
		Active *bool    `json:"active"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.URL != nil {
		hook.URL = *body.URL
	}
	if body.Events != nil {
		for _, e := range body.Events {
			if _, ok := validWebhookEvents[e]; !ok {
				writeAPIError(w, r, ErrCodeInvalidField, fmt.Sprintf("unknown event: %s", e), "")
				return
			}
		}
		hook.Events = body.Events
	}
	if body.Active != nil {
		hook.Active = *body.Active
	}
	hook.UpdatedAt = time.Now().UTC()

	if err := s.store.SaveWebhook(hook); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save webhook", "")
		return
	}
	writeJSON(w, hook)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	wid := r.PathValue("webhook_id")
	if _, err := s.store.GetWebhook(pid, wid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeWebhookNotFound, "webhook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get webhook", "")
		return
	}
	if err := s.store.DeleteWebhook(pid, wid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete webhook", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	wid := r.PathValue("webhook_id")
	if _, err := s.store.GetWebhook(pid, wid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeWebhookNotFound, "webhook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get webhook", "")
		return
	}
	deliveries, err := s.store.ListWebhookDeliveries(pid, wid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list deliveries", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(deliveries)
	deliveries, nextCursor := applyPagination(deliveries, page)
	writeListJSON(w, "deliveries", total, deliveries, nextCursor)
}

// dispatchWebhook sends an event to all matching active webhooks for a project.
func (s *Server) dispatchWebhook(pid, eventType string, payload any) {
	hooks, err := s.store.ListWebhooks(pid)
	if err != nil {
		return
	}
	for _, hook := range hooks {
		if !hook.Active {
			continue
		}
		subscribes := false
		for _, e := range hook.Events {
			if e == eventType {
				subscribes = true
				break
			}
		}
		if !subscribes {
			continue
		}
		go s.deliverWebhook(hook, eventType, payload)
	}
}

func (s *Server) deliverWebhook(hook Webhook, eventType string, payload any) {
	body, err := json.Marshal(map[string]any{
		"event":      eventType,
		"payload":    payload,
		"created_at": time.Now().UTC(),
	})
	if err != nil {
		return
	}

	sig := webhookSignature(hook.ID, body)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenThesis-Event", eventType)
	req.Header.Set("X-OpenThesis-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	delivery := WebhookDelivery{
		ID:        generateID("dlv"),
		WebhookID: hook.ID,
		EventType: eventType,
		Attempts:  1,
		CreatedAt: time.Now().UTC(),
	}
	if err != nil {
		delivery.Status = "failed"
		slog.Warn("webhook delivery failed", "url", hook.URL, "err", err)
	} else {
		resp.Body.Close()
		delivery.StatusCode = resp.StatusCode
		now := time.Now().UTC()
		delivery.DeliveredAt = &now
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			delivery.Status = "success"
		} else {
			delivery.Status = "failed"
		}
	}
	if err := s.store.SaveWebhookDelivery(hook.ProjectID, delivery); err != nil {
		slog.Warn("failed to persist webhook delivery", "project_id", hook.ProjectID, "webhook_id", hook.ID, "delivery_id", delivery.ID, "err", err)
	}
}

func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func validEventList() []string {
	out := make([]string, 0, len(validWebhookEvents))
	for k := range validWebhookEvents {
		out = append(out, k)
	}
	return out
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	wid := r.PathValue("webhook_id")
	hook, err := s.store.GetWebhook(pid, wid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeWebhookNotFound, "webhook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get webhook", "")
		return
	}
	go s.deliverWebhook(hook, "webhook.test", map[string]any{"ok": true})
	writeJSON(w, map[string]any{"status": "scheduled"})
}
