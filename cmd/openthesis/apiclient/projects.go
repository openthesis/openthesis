package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Project struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Stats       ProjectStats `json:"stats,omitempty"`
}

type ProjectStats struct {
	Tests         int `json:"tests"`
	ActiveRuns    int `json:"active_runs"`
	TotalFindings int `json:"total_findings"`
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := c.get(ctx, "/api/v1/projects", &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func (c *Client) CreateProject(ctx context.Context, name, description string) (Project, error) {
	body := map[string]string{"name": name, "description": description}
	var p Project
	if err := c.post(ctx, "/api/v1/projects", body, &p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (c *Client) GetProject(ctx context.Context, id string) (Project, error) {
	var p Project
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s", id), &p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (c *Client) DeleteProject(ctx context.Context, id string) error {
	return c.delete(ctx, fmt.Sprintf("/api/v1/projects/%s?confirm=true", id))
}
