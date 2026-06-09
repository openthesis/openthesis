package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Environment struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Backend     string    `json:"backend"`
	ComposeFile string    `json:"compose_file,omitempty"`
	TestDir     string    `json:"test_dir,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type EnvCreateRequest struct {
	Name           string `json:"name"`
	Backend        string `json:"backend"`
	ComposeFile    string `json:"compose_file,omitempty"`
	ComposeContent string `json:"compose_file_content,omitempty"`
	TestDir        string `json:"test_dir,omitempty"`
}

func (c *Client) ListEnvironments(ctx context.Context, projectID string) ([]Environment, error) {
	var out struct {
		Environments []Environment `json:"environments"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/environments", projectID), &out); err != nil {
		return nil, err
	}
	return out.Environments, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, projectID string, req EnvCreateRequest) (Environment, error) {
	var e Environment
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/environments", projectID), req, &e); err != nil {
		return Environment{}, err
	}
	return e, nil
}

func (c *Client) GetEnvironment(ctx context.Context, projectID, envID string) (Environment, error) {
	var e Environment
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/environments/%s", projectID, envID), &e); err != nil {
		return Environment{}, err
	}
	return e, nil
}
