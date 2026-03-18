package codemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a REST client for the code-mcp management API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Client targeting the given code-mcp base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Minute},
	}
}

// EnsureRepo clones a repository into code-mcp if it is not already present.
// A 409 Conflict response is treated as success (repo already exists).
func (c *Client) EnsureRepo(ctx context.Context, name, cloneURL, token string) error {
	body := map[string]string{
		"url":  cloneURL,
		"name": name,
	}
	if token != "" {
		body["token"] = token
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal EnsureRepo body: %w", err)
	}

	resp, err := c.post(ctx, "/api/repos", data)
	if err != nil {
		return fmt.Errorf("EnsureRepo %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Already exists — that's fine.
		return nil
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("EnsureRepo %q: unexpected status %d: %s", name, resp.StatusCode, string(b))
	}
	return nil
}

// EnsureBranch creates a worktree for the given branch in an already-cloned repo.
// baseBranch is the branch to base the new branch off of.
func (c *Client) EnsureBranch(ctx context.Context, repo, branch, baseBranch string) error {
	body := map[string]string{
		"branch":     branch,
		"baseBranch": baseBranch,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal EnsureBranch body: %w", err)
	}

	resp, err := c.post(ctx, fmt.Sprintf("/api/repos/%s/branches", repo), data)
	if err != nil {
		return fmt.Errorf("EnsureBranch %q/%q: %w", repo, branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Already exists — that's fine.
		return nil
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("EnsureBranch %q/%q: unexpected status %d: %s", repo, branch, resp.StatusCode, string(b))
	}
	return nil
}

// DeleteBranch removes a branch worktree from code-mcp.
func (c *Client) DeleteBranch(ctx context.Context, repo, branch string) error {
	url := fmt.Sprintf("%s/api/repos/%s/branches/%s", c.baseURL, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build DeleteBranch request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteBranch %q/%q: %w", repo, branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Already gone — that's fine.
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DeleteBranch %q/%q: unexpected status %d: %s", repo, branch, resp.StatusCode, string(b))
	}
	return nil
}

// testRunResponse is the JSON body returned by the test/run endpoint.
type testRunResponse struct {
	Passed bool   `json:"passed"`
	Output string `json:"output"`
}

// RunTests runs the repo's configured test command against the given branch worktree.
// It returns whether the tests passed and the raw output.
func (c *Client) RunTests(ctx context.Context, repo, branch string) (bool, string, error) {
	path := fmt.Sprintf("/api/repos/%s/branches/%s/test/run", repo, branch)
	resp, err := c.post(ctx, path, nil)
	if err != nil {
		return false, "", fmt.Errorf("RunTests %q/%q: %w", repo, branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return false, "", fmt.Errorf("RunTests %q/%q: unexpected status %d: %s", repo, branch, resp.StatusCode, string(b))
	}

	var result testRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", fmt.Errorf("RunTests decode response: %w", err)
	}
	return result.Passed, result.Output, nil
}

// PushBranch pushes the branch worktree's commits to the remote origin.
func (c *Client) PushBranch(ctx context.Context, repo, branch string) error {
	path := fmt.Sprintf("/api/repos/%s/branches/%s/merge", repo, branch)
	resp, err := c.post(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("PushBranch %q/%q: %w", repo, branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PushBranch %q/%q: unexpected status %d: %s", repo, branch, resp.StatusCode, string(b))
	}
	return nil
}

// MCPReadURL returns the Streamable HTTP MCP endpoint URL for read-only access
// to the given repo+branch worktree.
func (c *Client) MCPReadURL(repo, branch string) string {
	return fmt.Sprintf("%s/%s/%s/read/mcp", c.baseURL, repo, branch)
}

// MCPWriteURL returns the Streamable HTTP MCP endpoint URL for read-write access
// to the given repo+branch worktree.
func (c *Client) MCPWriteURL(repo, branch string) string {
	return fmt.Sprintf("%s/%s/%s/write/mcp", c.baseURL, repo, branch)
}

// post is a helper that sends a POST request with a JSON body to the given path.
func (c *Client) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	url := c.baseURL + path

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build POST request to %s: %w", path, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}
