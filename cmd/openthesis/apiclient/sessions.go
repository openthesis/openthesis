package apiclient

import (
	"context"
	"fmt"
)

// Session is a debug session created for a specific run.
type Session struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	ProjectID   string `json:"project_id"`
	TestID      string `json:"test_id"`
	Mode        string `json:"mode"`
	CurrentStep int    `json:"current_step"`
	MaxStep     int    `json:"max_step"`
}

// ShrinkResult is returned by ShrinkSession after binary-search minimization.
type ShrinkResult struct {
	OriginalSteps int `json:"original_steps"`
	MinimalSteps  int `json:"minimal_steps"`
}

// CreateSession creates a debug session for the given run.
// The session allows replaying and shrinking the run's execution.
func (c *Client) CreateSession(ctx context.Context, projectID, testID, runID string) (Session, error) {
	var sess Session
	body := map[string]string{"mode": "notebook"}
	if err := c.post(ctx,
		fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs/%s/sessions", projectID, testID, runID),
		body, &sess,
	); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ShrinkSession runs binary-search counterexample minimization on the session.
// It returns the original and minimal step counts after minimization.
func (c *Client) ShrinkSession(ctx context.Context, sessionID string) (ShrinkResult, error) {
	var result ShrinkResult
	if err := c.post(ctx,
		fmt.Sprintf("/api/v1/sessions/%s/shrink", sessionID),
		nil, &result,
	); err != nil {
		return ShrinkResult{}, err
	}
	return result, nil
}
