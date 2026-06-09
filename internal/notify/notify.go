// Package notify delivers webhook and email notifications for OpenThesis run events.
// All sends are non-fatal: errors are logged as warnings and do not fail the run.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// Config holds notification delivery settings loaded from testconfig.
type Config struct {
	// WebhookURL is the HTTP POST target. Empty means notifications are disabled.
	WebhookURL string `json:"webhook_url"`

	// OnEvents is the list of event types to deliver.
	// Valid values: "new_violation", "ongoing_violation", "resolved", "run_complete".
	// Empty means all events.
	OnEvents []string `json:"on"`

	// SlackFormat wraps the payload in {"text": "..."} with a Markdown message
	// suitable for Slack incoming webhooks.
	SlackFormat bool `json:"slack_format"`

	// Email holds optional SMTP email delivery settings.
	// If nil or To is empty, email delivery is disabled.
	Email *EmailConfig `json:"email,omitempty"`
}

// EmailConfig configures SMTP email delivery for violation and run-complete events.
type EmailConfig struct {
	// SMTPHost is the mail server hostname, e.g. "smtp.gmail.com".
	SMTPHost string `json:"smtp_host"`
	// SMTPPort is 587 (STARTTLS) or 465 (TLS). 587 is recommended.
	SMTPPort int `json:"smtp_port"`
	// Username is the SMTP auth username (usually the From address).
	Username string `json:"username"`
	// Password is the SMTP auth password or app-specific password.
	Password string `json:"password"`
	// From is the sender address, e.g. "openthesis@example.com".
	From string `json:"from"`
	// To is the list of recipient addresses.
	To []string `json:"to"`
}

// ViolationEvent describes a single violation notification.
type ViolationEvent struct {
	RunID       string
	Property    string
	Message     string
	Step        uint64
	Seed        uint64
	Status      string // "new", "ongoing", "resolved"
	ArtifactDir string
}

// SendViolation POSTs a violation event to the configured webhook.
// The event type is derived from ev.Status: "new" → "new_violation",
// "ongoing" → "ongoing_violation", "resolved" → "resolved".
// Non-fatal: logs a warning on failure, does not return an error that stops the run.
func SendViolation(ctx context.Context, cfg Config, ev ViolationEvent) error {
	if cfg.WebhookURL == "" {
		return nil
	}

	// Determine the event type name.
	eventType := ev.Status + "_violation"
	if ev.Status == "resolved" {
		eventType = "resolved"
	}

	if !wantsEvent(cfg, eventType) {
		return nil
	}

	payload := map[string]any{
		"event":        eventType,
		"run_id":       ev.RunID,
		"property":     ev.Property,
		"message":      ev.Message,
		"step":         ev.Step,
		"seed":         ev.Seed,
		"status":       ev.Status,
		"artifact_dir": ev.ArtifactDir,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	}

	var body []byte
	var err error
	if cfg.SlackFormat {
		text := fmt.Sprintf("*[%s]* `%s`\n%s\nRun: `%s`  Step: %d  Seed: %d",
			eventType, ev.Property, ev.Message, ev.RunID, ev.Step, ev.Seed)
		if ev.ArtifactDir != "" {
			text += fmt.Sprintf("\nArtifact: `%s`", ev.ArtifactDir)
		}
		body, err = json.Marshal(map[string]string{"text": text})
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		slog.Warn("notify: marshal payload failed", "err", err)
		return fmt.Errorf("notify: marshal: %w", err)
	}

	if sendErr := postJSON(ctx, cfg.WebhookURL, body); sendErr != nil {
		slog.Warn("notify: webhook delivery failed", "event", eventType, "url", cfg.WebhookURL, "err", sendErr)
		return sendErr
	}

	slog.Info("notify: webhook delivered", "event", eventType, "property", ev.Property)

	// Email delivery (non-fatal).
	if emailErr := SendViolationEmail(ctx, cfg, ev); emailErr != nil {
		slog.Warn("notify: email delivery failed", "event", eventType, "err", emailErr)
	}

	return nil
}

// SendRunComplete POSTs a run_complete event to the configured webhook.
// Non-fatal: logs a warning on failure.
func SendRunComplete(ctx context.Context, cfg Config, runID string, states uint64, violations int) error {
	if cfg.WebhookURL == "" {
		return nil
	}

	if !wantsEvent(cfg, "run_complete") {
		return nil
	}

	payload := map[string]any{
		"event":      "run_complete",
		"run_id":     runID,
		"states":     states,
		"violations": violations,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	}

	var body []byte
	var err error
	if cfg.SlackFormat {
		status := "PASS"
		if violations > 0 {
			status = fmt.Sprintf("FAIL (%d violations)", violations)
		}
		text := fmt.Sprintf("*run_complete* `%s`  %s  %d states", runID, status, states)
		body, err = json.Marshal(map[string]string{"text": text})
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		slog.Warn("notify: marshal run_complete payload failed", "err", err)
		return fmt.Errorf("notify: marshal: %w", err)
	}

	if sendErr := postJSON(ctx, cfg.WebhookURL, body); sendErr != nil {
		slog.Warn("notify: webhook run_complete delivery failed", "url", cfg.WebhookURL, "err", sendErr)
		return sendErr
	}

	slog.Info("notify: run_complete webhook delivered", "run_id", runID, "violations", violations)

	// Email delivery (non-fatal).
	if emailErr := SendRunCompleteEmail(ctx, cfg, runID, states, violations); emailErr != nil {
		slog.Warn("notify: email run_complete delivery failed", "err", emailErr)
	}

	return nil
}

// SendViolationEmail sends a plain-text violation email via SMTP.
// Returns nil if email delivery is disabled (cfg.Email is nil or To is empty).
// Non-fatal: caller should log the returned error as a warning.
func SendViolationEmail(_ context.Context, cfg Config, ev ViolationEvent) error {
	if cfg.Email == nil || len(cfg.Email.To) == 0 {
		return nil
	}
	ec := cfg.Email

	subject := fmt.Sprintf("[OpenThesis] New violation: %s", ev.Property)
	body := strings.Join([]string{
		"Violation detected in run " + ev.RunID,
		"",
		"Property:  " + ev.Property,
		"Message:   " + ev.Message,
		fmt.Sprintf("Step:      %d", ev.Step),
		fmt.Sprintf("Seed:      %d", ev.Seed),
		"Status:    " + ev.Status,
		"Artifact:  " + ev.ArtifactDir,
		"",
		"To investigate: openthesis find",
	}, "\r\n")

	return sendSMTP(ec, subject, body)
}

// SendRunCompleteEmail sends a plain-text run-complete email via SMTP.
// Returns nil if email delivery is disabled.
func SendRunCompleteEmail(_ context.Context, cfg Config, runID string, states uint64, violations int) error {
	if cfg.Email == nil || len(cfg.Email.To) == 0 {
		return nil
	}
	ec := cfg.Email

	result := "PASS"
	if violations > 0 {
		result = fmt.Sprintf("FAIL (%d violation(s))", violations)
	}
	subject := fmt.Sprintf("[OpenThesis] Run complete: %s - %s", runID, result)
	body := strings.Join([]string{
		"Run complete: " + runID,
		"",
		"Result:     " + result,
		fmt.Sprintf("States:     %d", states),
		fmt.Sprintf("Violations: %d", violations),
		"",
		"To view results: openthesis triage",
	}, "\r\n")

	return sendSMTP(ec, subject, body)
}

// sendSMTP delivers a plain-text email using the given EmailConfig.
// Port 587 uses STARTTLS via smtp.SendMail; port 465 uses implicit TLS.
func sendSMTP(ec *EmailConfig, subject, body string) error {
	if ec.SMTPHost == "" {
		return fmt.Errorf("notify: smtp_host is required")
	}
	port := ec.SMTPPort
	if port == 0 {
		port = 587
	}

	toHeader := strings.Join(ec.To, ", ")
	msg := []byte(
		"From: " + ec.From + "\r\n" +
			"To: " + toHeader + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			body,
	)

	addr := fmt.Sprintf("%s:%d", ec.SMTPHost, port)
	auth := smtp.PlainAuth("", ec.Username, ec.Password, ec.SMTPHost)

	if port == 465 {
		// Implicit TLS (SMTPS).
		tlsCfg := &tls.Config{ServerName: ec.SMTPHost}
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("notify: smtp dial tls: %w", err)
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, ec.SMTPHost)
		if err != nil {
			return fmt.Errorf("notify: smtp new client: %w", err)
		}
		defer client.Close()
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notify: smtp auth: %w", err)
		}
		if err := client.Mail(ec.From); err != nil {
			return fmt.Errorf("notify: smtp MAIL FROM: %w", err)
		}
		for _, to := range ec.To {
			if err := client.Rcpt(to); err != nil {
				return fmt.Errorf("notify: smtp RCPT TO %s: %w", to, err)
			}
		}
		wc, err := client.Data()
		if err != nil {
			return fmt.Errorf("notify: smtp DATA: %w", err)
		}
		if _, err := wc.Write(msg); err != nil {
			wc.Close()
			return fmt.Errorf("notify: smtp write: %w", err)
		}
		if err := wc.Close(); err != nil {
			return fmt.Errorf("notify: smtp data close: %w", err)
		}
		return client.Quit()
	}

	// Port 587 (or other): STARTTLS via smtp.SendMail.
	if err := smtp.SendMail(addr, auth, ec.From, ec.To, msg); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	return nil
}

// wantsEvent returns true if the event type should be delivered.
// An empty OnEvents list means all events are delivered.
func wantsEvent(cfg Config, eventType string) bool {
	if len(cfg.OnEvents) == 0 {
		return true
	}
	for _, e := range cfg.OnEvents {
		if e == eventType {
			return true
		}
	}
	return false
}

// postJSON makes a POST request with the given JSON body and a 10-second timeout.
func postJSON(ctx context.Context, url string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify: POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: POST %s: unexpected status %d", url, resp.StatusCode)
	}

	return nil
}
