package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// ToolCall represents a tool the agent wants to invoke.
type ToolCall struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// Request is the payload sent to the agent service.
type Request struct {
	Phase      string                   `json:"phase"`
	Context    string                   `json:"context"`
	MCPServers []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
}

// Response is the payload returned by the agent service.
type Response struct {
	Text      string     `json:"text"`
	Done      bool       `json:"done"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Client is an HTTP client for the agent service.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new agent Client targeting baseURL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

// Send sends a request to the agent service and returns its response.
func (c *Client) Send(ctx context.Context, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal agent request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/agent", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build agent HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agent HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read agent response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent service returned %d: %s", resp.StatusCode, string(respBody))
	}

	var agentResp Response
	if err := json.Unmarshal(respBody, &agentResp); err != nil {
		return nil, fmt.Errorf("unmarshal agent response: %w", err)
	}
	return &agentResp, nil
}
