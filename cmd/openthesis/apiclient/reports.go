package apiclient

import (
	"context"
	"fmt"
)

func (c *Client) PostReport(ctx context.Context, projectID, testID string, body, out any) error {
	return c.post(ctx, fmt.Sprintf("/api/v1/projects/%s/tests/%s/reports", projectID, testID), body, out)
}
