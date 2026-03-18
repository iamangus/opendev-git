package agentapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// newTestAgentServer starts a mock agent service that replies with the given
// response JSON and returns its URL.
func newTestAgentServer(t *testing.T, statusCode int, resp agent.Response) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("mock agent: encode response: %v", err)
		}
	}))
}

func TestServeHTTPMethodNotAllowed(t *testing.T) {
	h := NewHandler(agent.NewClient("http://localhost"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/researcher/run", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestServeHTTPMissingMessage(t *testing.T) {
	srv := newTestAgentServer(t, http.StatusOK, agent.Response{Text: "hello"})
	defer srv.Close()

	h := NewHandler(agent.NewClient(srv.URL))

	body, _ := json.Marshal(RunRequest{Message: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/researcher/run", bytes.NewReader(body))
	req.SetPathValue("name", "researcher")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing message, got %d", rr.Code)
	}
}

func TestServeHTTPInvalidJSON(t *testing.T) {
	h := NewHandler(agent.NewClient("http://localhost"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/researcher/run", bytes.NewReader([]byte("not-json")))
	req.SetPathValue("name", "researcher")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

func TestServeHTTPSuccess(t *testing.T) {
	srv := newTestAgentServer(t, http.StatusOK, agent.Response{Text: "MCP is a protocol"})
	defer srv.Close()

	h := NewHandler(agent.NewClient(srv.URL))

	reqBody := RunRequest{Message: "What is the MCP protocol?"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/researcher/run", bytes.NewReader(body))
	req.SetPathValue("name", "researcher")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp RunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Agent != "researcher" {
		t.Errorf("agent = %q, want %q", resp.Agent, "researcher")
	}
	if resp.Response != "MCP is a protocol" {
		t.Errorf("response = %q, want %q", resp.Response, "MCP is a protocol")
	}
}

func TestServeHTTPWithMCPServers(t *testing.T) {
	// Capture the request body the mock agent receives to verify mcp_servers is forwarded.
	var captured agent.Request
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("mock agent: decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(agent.Response{Text: "ok"}); err != nil {
			t.Errorf("mock agent: encode response: %v", err)
		}
	}))
	defer mockSrv.Close()

	h := NewHandler(agent.NewClient(mockSrv.URL))

	reqBody := RunRequest{
		Message: "hello",
		MCPServers: []mcpclient.ServerConfig{
			{
				Name:      "my-server",
				URL:       "https://mcp.example.com/mcp",
				Transport: "streamable-http",
				Headers:   map[string]string{"Authorization": "Bearer token"},
			},
		},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coder/run", bytes.NewReader(body))
	req.SetPathValue("name", "coder")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(captured.MCPServers) != 1 {
		t.Fatalf("expected 1 MCP server forwarded, got %d", len(captured.MCPServers))
	}
	if captured.MCPServers[0].Name != "my-server" {
		t.Errorf("mcp server name = %q, want %q", captured.MCPServers[0].Name, "my-server")
	}
	if captured.MCPServers[0].Transport != "streamable-http" {
		t.Errorf("mcp server transport = %q, want %q", captured.MCPServers[0].Transport, "streamable-http")
	}
	if captured.MCPServers[0].Headers["Authorization"] != "Bearer token" {
		t.Errorf("mcp server auth header = %q, want %q", captured.MCPServers[0].Headers["Authorization"], "Bearer token")
	}
}

func TestServeHTTPAgentError(t *testing.T) {
	// Mock agent returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := NewHandler(agent.NewClient(srv.URL))

	body, _ := json.Marshal(RunRequest{Message: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/researcher/run", bytes.NewReader(body))
	req.SetPathValue("name", "researcher")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when agent errors, got %d", rr.Code)
	}
}

func TestRunRequestJSONSchema(t *testing.T) {
	raw := `{"message":"What is MCP?","mcp_servers":[{"name":"s","url":"https://example.com","transport":"sse","headers":{"X-Key":"val"}}]}`
	var req RunRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Message != "What is MCP?" {
		t.Errorf("message = %q", req.Message)
	}
	if len(req.MCPServers) != 1 {
		t.Fatalf("mcp_servers len = %d", len(req.MCPServers))
	}
	s := req.MCPServers[0]
	if s.Name != "s" || s.URL != "https://example.com" || s.Transport != "sse" || s.Headers["X-Key"] != "val" {
		t.Errorf("unexpected server config: %+v", s)
	}
}
