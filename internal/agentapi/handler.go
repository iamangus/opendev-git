package agentapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// RunRequest is the request body for POST /api/v1/agents/{name}/run.
type RunRequest struct {
	Message    string                   `json:"message"`
	MCPServers []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
}

// RunResponse is the response body for a successful agent run.
type RunResponse struct {
	Agent    string `json:"agent"`
	Response string `json:"response"`
}

// Handler handles agent run requests at POST /api/v1/agents/{name}/run.
type Handler struct {
	agentClient *agent.Client
}

// NewHandler creates a Handler backed by the provided agent client.
func NewHandler(client *agent.Client) *Handler {
	return &Handler{agentClient: client}
}

// ServeHTTP implements http.Handler. It expects to be registered with a pattern
// that exposes the agent name via r.PathValue("name"), e.g.:
//
//	mux.Handle("POST /api/v1/agents/{name}/run", agentapi.NewHandler(client))
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentName := r.PathValue("name")
	if agentName == "" {
		http.Error(w, "agent name required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("agentapi: read body: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var req RunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	agentReq := agent.Request{
		Phase:      agentName,
		Context:    req.Message,
		MCPServers: req.MCPServers,
	}

	resp, err := h.agentClient.Send(r.Context(), agentReq)
	if err != nil {
		log.Printf("agentapi: agent %q: %v", agentName, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	runResp := RunResponse{
		Agent:    agentName,
		Response: resp.Text,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(runResp); err != nil {
		log.Printf("agentapi: encode response: %v", err)
	}
}
