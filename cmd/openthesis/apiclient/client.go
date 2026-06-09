// Package apiclient provides a typed HTTP client for the OpenThesis API.
package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a typed HTTP client for the OpenThesis API.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// New creates a Client using the server URL and optional API key.
// Resolution order for server URL: explicit arg → OPENTHESIS_SERVER env → default.
func New(server, apiKey string) *Client {
	if server == "" {
		server = os.Getenv("OPENTHESIS_SERVER")
	}
	if server == "" {
		server = "http://localhost:8080"
	}
	return &Client{
		base:   server,
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var wrapped struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		if jsonErr := json.Unmarshal(respBody, &wrapped); jsonErr == nil && wrapped.Error.Message != "" {
			if wrapped.Error.Hint != "" {
				return fmt.Errorf("%s: %s (%s)", wrapped.Error.Code, wrapped.Error.Message, wrapped.Error.Hint)
			}
			return fmt.Errorf("%s: %s", wrapped.Error.Code, wrapped.Error.Message)
		}

		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		}
		if jsonErr := json.Unmarshal(respBody, &apiErr); jsonErr == nil && apiErr.Message != "" {
			if apiErr.Hint != "" {
				return fmt.Errorf("%s: %s (%s)", apiErr.Code, apiErr.Message, apiErr.Hint)
			}
			return fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// SSEEvent is a single server-sent event received from the API.
type SSEEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// StreamSSE connects to a text/event-stream endpoint and calls onEvent for
// each well-formed "data: ..." line. Blocks until ctx is cancelled or the
// server closes the stream. Uses a dedicated HTTP client with no timeout.
func (c *Client) StreamSSE(ctx context.Context, path string, onEvent func(SSEEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Use a client without a read timeout for SSE connections.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		// The server wraps each event as {"type":"...","data":{...}}.
		var evt SSEEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		onEvent(evt)
	}
	return scanner.Err()
}
