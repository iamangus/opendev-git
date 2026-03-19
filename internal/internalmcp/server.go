// Package internalmcp implements a minimal Streamable HTTP MCP server that
// exposes the ask_user tool to agents running in opendev-agents.
//
// When the agent calls ask_user, the handler:
//  1. Posts the question as a GitHub issue comment
//  2. Sets status:blocked on the issue
//  3. Cancels the active agent run via the agent client
//
// Usage:
//
//	srv, err := internalmcp.New(owner, repo, issueNumber, gh, labeler, canceler)
//	// Start the agent run, passing srv.MCPEndpoint() in the MCP server list.
//	runID, err := agentClient.StartRun(...)
//	srv.SetRunID(runID)
//	resp, err := agentClient.PollRun(ctx, runID)
//	srv.Close()
package internalmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GitHubPoster can post a comment on an issue.
type GitHubPoster interface {
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
}

// StatusTransitioner can change the status label on an issue.
type StatusTransitioner interface {
	// TransitionStatus removes all status:* labels and adds the given one.
	TransitionStatus(ctx context.Context, owner, repo string, number int, to string) error
}

// RunCanceler can cancel an agent run by ID.
type RunCanceler interface {
	Cancel(ctx context.Context, runID string) error
}

// Server is an ephemeral MCP server scoped to a single agent run.
type Server struct {
	owner       string
	repo        string
	issueNumber int

	mu    sync.RWMutex
	runID string

	gh       GitHubPoster
	labeler  StatusTransitioner
	canceler RunCanceler

	httpServer *http.Server
	addr       string // e.g. "http://127.0.0.1:PORT"
}

// New creates and starts a new ephemeral MCP server. The run ID can be set
// after the agent run is started via SetRunID. Call Close() when done.
func New(owner, repo string, issueNumber int, gh GitHubPoster, labeler StatusTransitioner, canceler RunCanceler) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("internalmcp: listen: %w", err)
	}

	s := &Server{
		owner:       owner,
		repo:        repo,
		issueNumber: issueNumber,
		gh:          gh,
		labeler:     labeler,
		canceler:    canceler,
		addr:        "http://" + ln.Addr().String(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("internalmcp: server error: %v", err)
		}
	}()

	return s, nil
}

// SetRunID sets (or updates) the run ID that will be canceled when ask_user is called.
// This must be called before the agent can actually invoke the ask_user tool.
func (s *Server) SetRunID(runID string) {
	s.mu.Lock()
	s.runID = runID
	s.mu.Unlock()
}

// URL returns the base URL of the MCP server (e.g. "http://127.0.0.1:PORT").
func (s *Server) URL() string {
	return s.addr
}

// MCPEndpoint returns the full URL of the MCP endpoint.
func (s *Server) MCPEndpoint() string {
	return s.addr + "/mcp"
}

// Close shuts down the server.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
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

// handleMCP handles all JSON-RPC messages sent to /mcp.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r.Context(), req)
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req jsonrpcRequest) {
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

func (s *Server) handleToolsList(w http.ResponseWriter, req jsonrpcRequest) {
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

func (s *Server) handleToolsCall(w http.ResponseWriter, ctx context.Context, req jsonrpcRequest) {
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

	// Cancel the agent run. This will cause PollRun in the orchestrator to
	// return an error, which propagates up and stops the workflow cleanly.
	if runID != "" {
		if err := s.canceler.Cancel(ctx, runID); err != nil {
			log.Printf("internalmcp: cancel run %s: %v", runID, err)
		}
	} else {
		log.Printf("internalmcp: ask_user called but run ID not yet set")
	}

	// Return a success response to the MCP call so the agent receives an
	// acknowledgement before its context is torn down.
	writeJSONRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "Question posted. Run will be paused until the user replies."},
		},
	})
}

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors still use 200
	_ = json.NewEncoder(w).Encode(resp)
}
