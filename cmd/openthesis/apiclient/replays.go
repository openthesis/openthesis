package apiclient

import (
	"context"
	"fmt"
	"time"
)

type Replay struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	FindingID   string     `json:"finding_id,omitempty"`
	ReplayToken string     `json:"replay_token,omitempty"`
	Status      string     `json:"status"`
	Mode        string     `json:"mode"`
	Debugger    *DebugInfo `json:"debugger,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

type DebugInfo struct {
	ConnectCommand string `json:"connect_command,omitempty"`
}

func (c *Client) CreateReplay(ctx context.Context, projectID, findingID, replayToken, mode string) (Replay, error) {
	body := map[string]string{
		"finding_id":   findingID,
		"replay_token": replayToken,
		"mode":         mode,
	}
	var r Replay
	if err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/replays", projectID), body, &r); err != nil {
		return Replay{}, err
	}
	return r, nil
}

func (c *Client) GetReplay(ctx context.Context, projectID, replayID string) (Replay, error) {
	var r Replay
	if err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%s/replays/%s", projectID, replayID), &r); err != nil {
		return Replay{}, err
	}
	return r, nil
}
