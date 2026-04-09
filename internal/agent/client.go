package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// Message mirrors the llm.Message type in opendev-agents so that conversation
// history can be passed back on resumption.
type Message struct {
	Role       string `json:"role"`
	Content    any    `json:"content,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ResponseSchema describes the JSON schema to enforce on the agent's response.
type ResponseSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

// Request is the payload sent to start an agent run.
type Request struct {
	AgentName      string                   `json:"agent_name"`
	Context        string                   `json:"context"`
	History        []Message                `json:"history,omitempty"`
	MCPServers     []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
	ResponseJSON   bool                     `json:"response_json,omitempty"`
	ResponseSchema *ResponseSchema          `json:"response_schema,omitempty"`
}

// Response is the result returned once a run completes.
type Response struct {
	Text string
}

// Unmarshal extracts JSON from r.Text and unmarshals it into v.
// It handles cases where the agent prepends conversational text before the
// JSON object or wraps it in markdown code fences (```json ... ```).
func (r *Response) Unmarshal(v any) error {
	data, err := extractJSON(r.Text)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

var codeFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*\n(.*?)\\s*```")

// extractJSON returns the bytes of the first valid JSON object in s.
// It strips markdown code fences and leading/trailing non-JSON text.
func extractJSON(s string) ([]byte, error) {
	s = strings.TrimSpace(s)

	if json.Valid([]byte(s)) {
		return []byte(s), nil
	}

	if m := codeFenceRe.FindStringSubmatch(s); len(m) == 2 {
		candidate := strings.TrimSpace(m[1])
		if json.Valid([]byte(candidate)) {
			return []byte(candidate), nil
		}
	}

	start := strings.Index(s, "{")
	if start == -1 {
		return nil, fmt.Errorf("no JSON object found in response")
	}

	end := findMatchingBrace(s, start)
	if end == -1 {
		return nil, fmt.Errorf("unbalanced braces in response")
	}

	candidate := s[start : end+1]
	if json.Valid([]byte(candidate)) {
		return []byte(candidate), nil
	}

	return nil, fmt.Errorf("extracted content is not valid JSON")
}

// findMatchingBrace returns the index of the closing } that matches the
// opening { at position start, respecting JSON strings and nesting.
func findMatchingBrace(s string, start int) int {
	depth := 0
	inString := false
	escape := false

	for i := start; i < len(s); i++ {
		ch := s[i]

		if escape {
			escape = false
			continue
		}

		if ch == '\\' && inString {
			escape = true
			continue
		}

		if ch == '"' {
			inString = !inString
			continue
		}

		if inString {
			continue
		}

		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// wireRunRequest is the JSON body sent to POST /api/v1/agents/{name}/run.
type wireRunRequest struct {
	Message        string                   `json:"message"`
	History        []Message                `json:"history,omitempty"`
	MCPServers     []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
	ResponseJSON   bool                     `json:"response_json,omitempty"`
	ResponseSchema *ResponseSchema          `json:"response_schema,omitempty"`
}

// wireRunResponse is the JSON body returned by POST /api/v1/agents/{name}/run (202).
type wireRunResponse struct {
	RunID string `json:"run_id"`
}

// wireRunStatus is the JSON body returned by GET /api/v1/runs/{id}.
type wireRunStatus struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

const pollInterval = 3 * time.Second

// Client is an HTTP client for the agent service (agentfoundry).
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new agent Client targeting baseURL.
// If apiKey is non-empty, it is sent as a Bearer token on every request.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// Send starts an async agent run and polls until the run reaches a terminal
// state (completed, failed, or canceled). It returns the final text response
// on success or an error otherwise.
func (c *Client) Send(ctx context.Context, req Request) (*Response, error) {
	runID, err := c.StartRun(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.PollRun(ctx, runID)
}

// StartRun submits an agent run request and returns the run ID immediately.
// Use PollRun to wait for the result.
func (c *Client) StartRun(ctx context.Context, req Request) (string, error) {
	return c.startRun(ctx, req)
}

// PollRun polls GET /api/v1/runs/{id} until the run reaches a terminal state.
func (c *Client) PollRun(ctx context.Context, runID string) (*Response, error) {
	return c.pollRun(ctx, runID)
}

// Cancel requests that the given run be terminated immediately.
// It is a best-effort call; errors are logged by the caller.
func (c *Client) Cancel(ctx context.Context, runID string) error {
	log.Printf("agent: canceling run %q", runID)
	url := c.baseURL + "/api/v1/runs/" + runID + "/cancel"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build cancel request: %w", err)
	}
	c.setAuth(httpReq)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}
	defer resp.Body.Close()
	// 200 OK = canceled, 409 Conflict = already terminal — both are acceptable.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel run %s returned %d: %s", runID, resp.StatusCode, string(body))
	}
	return nil
}

// startRun calls POST /api/v1/agents/{name}/run and returns the run ID.
func (c *Client) startRun(ctx context.Context, req Request) (string, error) {
	log.Printf("agent: starting run agent=%q", req.AgentName)
	body, err := json.Marshal(wireRunRequest{
		Message:        req.Context,
		History:        req.History,
		MCPServers:     req.MCPServers,
		ResponseJSON:   req.ResponseJSON,
		ResponseSchema: req.ResponseSchema,
	})
	if err != nil {
		return "", fmt.Errorf("marshal run request: %w", err)
	}

	url := c.baseURL + "/api/v1/agents/" + req.AgentName + "/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build run request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("start run: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read start-run response: %w", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("agent service returned %d: %s", resp.StatusCode, string(respBody))
	}

	var wire wireRunResponse
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return "", fmt.Errorf("unmarshal run response: %w", err)
	}
	if wire.RunID == "" {
		return "", fmt.Errorf("agent service returned empty run_id")
	}
	log.Printf("agent: run started runID=%q agent=%q", wire.RunID, req.AgentName)
	return wire.RunID, nil
}

// pollRun polls GET /api/v1/runs/{id} until the run reaches a terminal state.
// Transient network errors are silently retried.
func (c *Client) pollRun(ctx context.Context, runID string) (*Response, error) {
	url := c.baseURL + "/api/v1/runs/" + runID

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build poll request: %w", err)
		}
		c.setAuth(httpReq)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			// Transient network error — keep polling.
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		if resp.StatusCode != http.StatusOK {
			continue
		}

		var status wireRunStatus
		if err := json.Unmarshal(body, &status); err != nil {
			continue
		}

		switch status.Status {
		case "completed":
			log.Printf("agent: run %q completed", runID)
			return &Response{Text: status.Response}, nil
		case "failed":
			log.Printf("agent: run %q failed: %s", runID, status.Error)
			return nil, fmt.Errorf("agent run %s failed: %s", runID, status.Error)
		case "canceled":
			log.Printf("agent: run %q was canceled", runID)
			return nil, fmt.Errorf("agent run %s was canceled", runID)
		}
		// queued or running — keep polling
	}
}
