package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Run struct {
	ID          string      `json:"id"`
	TestID      string      `json:"test_id"`
	ProjectID   string      `json:"project_id"`
	Status      string      `json:"status"`
	Trigger     string      `json:"trigger"`
	Sequence    int         `json:"sequence"`
	Source      string      `json:"source,omitempty"`
	Description string      `json:"description,omitempty"`
	Summary     *RunSummary `json:"summary,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Duration    string      `json:"duration,omitempty"`
}

type RunSummary struct {
	TotalStates        int `json:"total_states"`
	MaxDepth           int `json:"max_depth"`
	FindingsDiscovered int `json:"findings_discovered"`
}

type RunCreateRequest struct {
	Source      string `json:"source,omitempty"`
	Description string `json:"description,omitempty"`
	IsEphemeral bool   `json:"is_ephemeral,omitempty"`
}

func (c *Client) ListRuns(ctx context.Context, projectID, testID string) ([]Run, error) {
	var out struct {
		Runs []Run `json:"runs"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs", projectID, testID), &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

func (c *Client) TriggerRun(ctx context.Context, projectID, testID string, req RunCreateRequest) (Run, error) {
	var r Run
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs", projectID, testID), req, &r); err != nil {
		return Run{}, err
	}
	return r, nil
}

func (c *Client) GetRun(ctx context.Context, projectID, testID, runID string) (Run, error) {
	var r Run
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs/%s", projectID, testID, runID), &r); err != nil {
		return Run{}, err
	}
	return r, nil
}

func (c *Client) CancelRun(ctx context.Context, projectID, testID, runID string) (Run, error) {
	var r Run
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs/%s/cancel", projectID, testID, runID), nil, &r); err != nil {
		return Run{}, err
	}
	return r, nil
}

// StreamRun streams live status and finding events for a run. Calls onEvent
// for each SSEEvent until the run completes or ctx is cancelled.
func (c *Client) StreamRun(ctx context.Context, projectID, testID, runID string, onEvent func(SSEEvent)) error {
	path := fmt.Sprintf("/api/v1/projects/%s/tests/%s/runs/%s/stream", projectID, testID, runID)
	return c.StreamSSE(ctx, path, onEvent)
}
