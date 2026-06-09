package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Finding struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TestID      string    `json:"test_id"`
	RunID       string    `json:"run_id"`
	Status      string    `json:"status"`
	AssertType  string    `json:"assert_type"`
	Property    string    `json:"property"`
	Message     string    `json:"message"`
	ReplayToken string    `json:"replay_token"`
	Occurrences int       `json:"occurrences"`
	Notes       string    `json:"notes,omitempty"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

func (c *Client) ListFindings(ctx context.Context, projectID string) ([]Finding, error) {
	var out struct {
		Findings []Finding `json:"findings"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/findings", projectID), &out); err != nil {
		return nil, err
	}
	return out.Findings, nil
}

func (c *Client) GetFinding(ctx context.Context, projectID, testID, findingID string) (Finding, error) {
	var f Finding
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/findings/%s", projectID, testID, findingID), &f); err != nil {
		return Finding{}, err
	}
	return f, nil
}

// ListTestFindings lists findings scoped to a specific test.
func (c *Client) ListTestFindings(ctx context.Context, projectID, testID string) ([]Finding, error) {
	var out struct {
		Findings []Finding `json:"findings"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/findings", projectID, testID), &out); err != nil {
		return nil, err
	}
	return out.Findings, nil
}
