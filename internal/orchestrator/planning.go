package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

type planningResponse struct {
	Approved            bool    `json:"approved"`
	Confidence          float64 `json:"confidence"`
	ClarificationNeeded *string `json:"clarification_needed"`
}

// runPlanning evaluates the investigation results and decides whether to proceed.
//
// Steps:
//  1. Agent evaluates confidence in the proposed task list (with read MCP access)
//  2. If the run completes → set status:approved, proceed to execution
//  3. If the run is canceled (ask_user) → status:blocked was already set by the MCP handler
func (o *Orchestrator) runPlanning(ctx context.Context, owner, repo string, issue *github.Issue, investigationComment, defaultBranch string) error {
	number := issue.GetNumber()
	log.Printf("orchestrator: starting planning for #%d (%s/%s)", number, owner, repo)

	planCtx := fmt.Sprintf(
		"## Issue\n%s\n\n## Investigation Report\n%s",
		buildIssueContext(issue),
		investigationComment,
	)

	readMCP := []mcpclient.ServerConfig{{
		Name:      "code",
		URL:       o.codemcp.MCPReadURL(repo, defaultBranch),
		Transport: "streamable-http",
	}}

	// Start a session on the shared MCP manager.
	sessionID, cleanup := o.mcpManager.CreateSession(owner, repo, number, o.github, o, o.agent)
	defer cleanup()

	allServers := append([]mcpclient.ServerConfig{
		{
			Name:      "ask_user",
			URL:       o.mcpManager.MCPEndpoint(sessionID),
			Transport: "streamable-http",
		},
	}, readMCP...)

	runID, err := o.agent.StartRun(ctx, agent.Request{
		AgentName:    o.config.AgentPlanning,
		Context:      planCtx,
		MCPServers:   allServers,
		ResponseJSON: true,
	})
	if err != nil {
		return fmt.Errorf("planning agent start run: %w", err)
	}
	log.Printf("orchestrator: planning agent run started runID=%q issue=#%d", runID, number)

	o.mcpManager.SetRunID(sessionID, runID)

	resp, pollErr := o.agent.PollRun(ctx, runID)
	if pollErr != nil {
		// Canceled runs mean ask_user was called — status:blocked already set.
		log.Printf("orchestrator: planning agent run %q ended with error: %v", runID, pollErr)
		return fmt.Errorf("planning agent send: %w", pollErr)
	}

	log.Printf("orchestrator: planning phase completed for #%d, response length=%d", number, len(resp.Text))

	var result planningResponse
	if err := json.Unmarshal([]byte(resp.Text), &result); err != nil {
		return fmt.Errorf("unmarshal planning response: %w", err)
	}

	if result.ClarificationNeeded != nil && *result.ClarificationNeeded != "" {
		log.Printf("orchestrator: planning requires clarification for #%d", number)
		return fmt.Errorf("planning requires clarification")
	}

	if !result.Approved {
		log.Printf("orchestrator: planning not approved for #%d", number)
		return fmt.Errorf("planning not approved")
	}

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:approved"); err != nil {
		return fmt.Errorf("set approved status: %w", err)
	}
	return o.runExecution(ctx, owner, repo, issue, investigationComment, defaultBranch)
}
