// Package internalmcp implements a session-based MCP handler hosted on
// opendev-git's own HTTP server. This eliminates the need for ephemeral
// listener ports and allows all MCP traffic to flow through the existing
// reverse proxy on 443.
//
// Each agent run gets a unique session ID. The route /{sessionID}/mcp is
// registered on the main ServeMux. When the agent calls ask_user, the handler:
//  1. Posts the question as a GitHub issue comment
//  2. Sets status:blocked on the issue
//  3. Cancels the active agent run via the agent client
//
// Usage:
//
//	mgr := internalmcp.NewManager(cfg.InternalMCPURL)
//	mgr.Register(mux)                     // once at startup, on the main mux
//
//	sessionID, cleanup := mgr.CreateSession(owner, repo, number, gh, labeler, canceler)
//	defer cleanup()
//	endpoint := mgr.MCPEndpoint(sessionID) // pass to opendev-agents start-run request
//	runID, err := agentClient.StartRun(...)
//	mgr.SetRunID(sessionID, runID)         // link the canceler after run starts
package internalmcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

// GitHubPoster can post a comment on an issue.
type GitHubPoster interface {
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
}

// StatusTransitioner can change the status label on an issue.
type StatusTransitioner interface {
	TransitionStatus(ctx context.Context, owner, repo string, number int, to string) error
}

// RunCanceler can cancel an agent run by ID.
type RunCanceler interface {
	Cancel(ctx context.Context, runID string) error
}

// session holds the per-run state for one active ask_user MCP session.
type session struct {
	owner       string
	repo        string
	issueNumber int

	mu    sync.RWMutex
	runID string

	gh       GitHubPoster
	labeler  StatusTransitioner
	canceler RunCanceler
}

// Manager hosts MCP sessions on the main HTTP server, keyed by session ID.
// Each session corresponds to one agent run. Register it once on startup;
// create/destroy individual sessions per run.
type Manager struct {
	baseURL string // e.g. "https://opendev-git.srvd.dev" — no trailing slash

	mu       sync.RWMutex
	sessions map[string]*session
}

// NewManager creates a Manager. baseURL is the externally-reachable base URL
// of opendev-git (no trailing slash), e.g. "https://opendev-git.srvd.dev" or
// "http://127.0.0.1:8080".
func NewManager(baseURL string) *Manager {
	return &Manager{
		baseURL:  strings.TrimRight(baseURL, "/"),
		sessions: make(map[string]*session),
	}
}

// Register attaches the MCP route to mux. Call this once at startup with the
// main ServeMux before the HTTP server starts listening.
func (m *Manager) Register(mux *http.ServeMux) {
	// Go 1.22+ pattern: method + path with wildcard.
	mux.HandleFunc("POST /{sessionID}/mcp", m.handleMCP)
}

// CreateSession registers a new session and returns its ID and a cleanup func.
// The cleanup func removes the session from the manager; always defer it.
func (m *Manager) CreateSession(owner, repo string, issueNumber int, gh GitHubPoster, labeler StatusTransitioner, canceler RunCanceler) (sessionID string, cleanup func()) {
	sessionID = newSessionID()
	s := &session{
		owner:       owner,
		repo:        repo,
		issueNumber: issueNumber,
		gh:          gh,
		labeler:     labeler,
		canceler:    canceler,
	}
	m.mu.Lock()
	m.sessions[sessionID] = s
	m.mu.Unlock()

	log.Printf("internalmcp: session created id=%q issue=%s/%s#%d", sessionID, owner, repo, issueNumber)

	cleanup = func() {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		log.Printf("internalmcp: session cleaned up id=%q", sessionID)
	}
	return sessionID, cleanup
}

// SetRunID links a run ID to an existing session so that ask_user calls know
// which run to cancel. Call this immediately after StartRun returns.
func (m *Manager) SetRunID(sessionID, runID string) {
	m.mu.RLock()
	s := m.sessions[sessionID]
	m.mu.RUnlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.runID = runID
	s.mu.Unlock()
	log.Printf("internalmcp: session %q linked to run %q", sessionID, runID)
}

// MCPEndpoint returns the full URL that opendev-agents should POST to for this
// session, e.g. "https://opendev-git.srvd.dev/abc123/mcp".
func (m *Manager) MCPEndpoint(sessionID string) string {
	return m.baseURL + "/" + sessionID + "/mcp"
}

// handleMCP dispatches an incoming JSON-RPC request to the correct session.
func (m *Manager) handleMCP(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	log.Printf("internalmcp: incoming request sessionID=%q method=POST", sessionID)

	m.mu.RLock()
	s := m.sessions[sessionID]
	m.mu.RUnlock()

	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		log.Printf("internalmcp: session %q — initialize", sessionID)
		handleInitialize(w, req)
	case "tools/list":
		log.Printf("internalmcp: session %q — tools/list", sessionID)
		handleToolsList(w, req)
	case "tools/call":
		log.Printf("internalmcp: session %q — tools/call", sessionID)
		s.handleToolsCall(w, r.Context(), req)
	default:
		log.Printf("internalmcp: session %q — unknown method %q", sessionID, req.Method)
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// --- JSON-RPC types ---

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonrpcError `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func handleInitialize(w http.ResponseWriter, req jsonrpcRequest) {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "opendev-git-internal",
			"version": "1.0.0",
		},
	}
	writeJSONRPCResult(w, req.ID, result)
}

func handleToolsList(w http.ResponseWriter, req jsonrpcRequest) {
	tools := []map[string]any{
		{
			"name":        "ask_user",
			"description": "Post a question to the GitHub issue and pause execution until the user replies. Call this when you need clarification before proceeding.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "The question to ask the user.",
					},
				},
				"required": []string{"question"},
			},
		},
	}
	writeJSONRPCResult(w, req.ID, map[string]any{"tools": tools})
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type askUserArgs struct {
	Question string `json:"question"`
}

func (s *session) handleToolsCall(w http.ResponseWriter, ctx context.Context, req jsonrpcRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	if params.Name != "ask_user" {
		writeJSONRPCError(w, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}

	var args askUserArgs
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid arguments: "+err.Error())
		return
	}

	question := strings.TrimSpace(args.Question)
	if question == "" {
		writeJSONRPCError(w, req.ID, -32602, "question must not be empty")
		return
	}

	s.mu.RLock()
	runID := s.runID
	s.mu.RUnlock()

	log.Printf("internalmcp: ask_user called for %s/%s#%d runID=%q question=%q",
		s.owner, s.repo, s.issueNumber, runID, question)

	// Post question to GitHub.
	comment := fmt.Sprintf(
		"I need some clarification before I can proceed:\n\n%s\n\nPlease reply mentioning @opendev-git to continue.",
		question,
	)
	if err := s.gh.PostComment(ctx, s.owner, s.repo, s.issueNumber, comment); err != nil {
		log.Printf("internalmcp: post comment: %v", err)
		// Non-fatal — still set status and cancel.
	}

	// Set status:blocked.
	if err := s.labeler.TransitionStatus(ctx, s.owner, s.repo, s.issueNumber, "status:blocked"); err != nil {
		log.Printf("internalmcp: transition to blocked: %v", err)
	}

	// Cancel the agent run.
	if runID != "" {
		if err := s.canceler.Cancel(ctx, runID); err != nil {
			log.Printf("internalmcp: cancel run %s: %v", runID, err)
		}
	} else {
		log.Printf("internalmcp: ask_user called but run ID not yet set")
	}

	writeJSONRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "Question posted. Run will be paused until the user replies."},
		},
	})
}

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: message}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
