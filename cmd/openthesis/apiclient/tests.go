package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Test struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	EnvironmentID string     `json:"environment_id"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Status        string     `json:"status"`
	Schedule      Schedule   `json:"schedule"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
}

type Schedule struct {
	Mode        string `json:"mode"`
	Parallelism int    `json:"parallelism,omitempty"`
	Cron        string `json:"cron,omitempty"`
}

type TestCreateRequest struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	EnvironmentID string   `json:"environment_id"`
	Schedule      Schedule `json:"schedule"`
}

func (c *Client) ListTests(ctx context.Context, projectID string) ([]Test, error) {
	var out struct {
		Tests []Test `json:"tests"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests", projectID), &out); err != nil {
		return nil, err
	}
	return out.Tests, nil
}

func (c *Client) CreateTest(ctx context.Context, projectID string, req TestCreateRequest) (Test, error) {
	var t Test
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests", projectID), req, &t); err != nil {
		return Test{}, err
	}
	return t, nil
}

func (c *Client) GetTest(ctx context.Context, projectID, testID string) (Test, error) {
	var t Test
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s", projectID, testID), &t); err != nil {
		return Test{}, err
	}
	return t, nil
}

func (c *Client) StartTest(ctx context.Context, projectID, testID string) (Test, error) {
	var t Test
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/start", projectID, testID), nil, &t); err != nil {
		return Test{}, err
	}
	return t, nil
}

func (c *Client) StopTest(ctx context.Context, projectID, testID string) (Test, error) {
	var t Test
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/stop", projectID, testID), nil, &t); err != nil {
		return Test{}, err
	}
	return t, nil
}
