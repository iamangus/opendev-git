package orchestrator

import (
	"context"
	"fmt"
	"log"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/internalmcp"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// runPlanning evaluates the investigation results and decides whether to proceed.
//
// Steps:
//  1. Agent evaluates confidence in the proposed task list (with read MCP access)
//  2. If the run completes → set status:approved, proceed to execution
//  3. If the run is canceled (ask_user) → status:blocked was already set by the MCP handler
func (o *Orchestrator) runPlanning(ctx context.Context, owner, repo string, issue *github.Issue, investigationComment, defaultBranch string) error {
	number := issue.GetNumber()

	// Fetch comment history so the agent can see prior conversation on resumption.
	comments, err := o.github.GetComments(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("get issue comments for planning: %w", err)
	}
	history := buildCommentHistory(comments)

	planCtx := fmt.Sprintf(
		"You are reviewing an investigation report for a GitHub issue and deciding whether the plan is clear enough to implement.\n\n"+
			"## Original Issue\n%s\n\n"+
			"## Investigation Report\n%s\n\n"+
			"Use the available MCP tools to explore the codebase if you need more context. "+
			"If the plan is clear and complete, respond with your approval. "+
			"If you need clarification from the user, use the ask_user tool.",
		buildIssueContext(issue),
		investigationComment,
	)

	readMCP := []mcpclient.ServerConfig{{
		Name:      "code",
		URL:       o.codemcp.MCPReadURL(repo, defaultBranch),
		Transport: "streamable-http",
	}}

	// Start the ephemeral internal MCP server.
	mcpSrv, err := internalmcp.New(owner, repo, number, o.github, o, o.agent)
	if err != nil {
		return fmt.Errorf("start internal MCP server for planning: %w", err)
	}
	defer mcpSrv.Close()

	allServers := append([]mcpclient.ServerConfig{
		{
			Name:      "ask_user",
			URL:       mcpSrv.MCPEndpoint(),
			Transport: "streamable-http",
		},
	}, readMCP...)

	runID, err := o.agent.StartRun(ctx, agent.Request{
		Phase:      "planning",
		Context:    planCtx,
		History:    history,
		MCPServers: allServers,
	})
	if err != nil {
		return fmt.Errorf("planning agent start run: %w", err)
	}

	mcpSrv.SetRunID(runID)

	resp, pollErr := o.agent.PollRun(ctx, runID)
	if pollErr != nil {
		// Canceled runs mean ask_user was called — status:blocked already set.
		return fmt.Errorf("planning agent send: %w", pollErr)
	}

	log.Printf("orchestrator: planning phase completed, response length=%d", len(resp.Text))

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:approved"); err != nil {
		return fmt.Errorf("set approved status: %w", err)
	}
	return o.runExecution(ctx, owner, repo, issue, investigationComment, defaultBranch)
}
