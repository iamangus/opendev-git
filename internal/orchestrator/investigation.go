package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

type investigationResponse struct {
	Findings      string   `json:"findings"`
	ProposedTasks []string `json:"proposed_tasks"`
	Risks         string   `json:"risks"`
}

// runInvestigation drives the investigation phase for an issue.
//
// Steps:
//  1. Set status:investigating
//  2. Determine the default branch and set up code-mcp repo/worktree for read access
//  3. Build context from issue title, body, and prior comment history
//  4. Send to agent (with read MCP); agent runs until completion or is canceled
//     via ask_user (which posts a question, sets status:blocked, and cancels the run)
//  5. Post "## Investigation Complete" comment
//  6. Transition to planning phase
func (o *Orchestrator) runInvestigation(ctx context.Context, owner, repo string, issue *github.Issue) error {
	number := issue.GetNumber()
	log.Printf("orchestrator: starting investigation for #%d (%s/%s)", number, owner, repo)

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:investigating"); err != nil {
		return fmt.Errorf("set investigating status: %w", err)
	}

	// Get default branch so we can set up a code-mcp read worktree.
	defaultBranch, _, err := o.github.GetDefaultBranch(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get default branch: %w", err)
	}

	// Ensure code-mcp has the repo cloned/synced and a worktree for the default branch.
	cloneURL := "https://github.com/" + owner + "/" + repo
	if err := o.codemcp.EnsureRepo(ctx, repo, cloneURL); err != nil {
		return fmt.Errorf("ensure repo in code-mcp: %w", err)
	}
	if err := o.codemcp.EnsureBranch(ctx, repo, defaultBranch, defaultBranch); err != nil {
		return fmt.Errorf("ensure default branch worktree in code-mcp: %w", err)
	}

	// Build MCP server config for read access to the default branch.
	readMCP := []mcpclient.ServerConfig{{
		Name:      "code",
		URL:       o.codemcp.MCPReadURL(repo, defaultBranch),
		Transport: "streamable-http",
	}}

	// Fetch all comments so we can reconstruct conversation history on resumption.
	comments, err := o.github.GetComments(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("get issue comments: %w", err)
	}
	history := buildCommentHistory(comments)

	// Build context from issue title and body only.
	agentCtx := buildIssueContext(issue)

	resp, err := o.runAgentLoop(ctx, o.config.AgentInvestigation, agentCtx, history, owner, repo, number, readMCP)
	if err != nil {
		return err
	}

	// Build and post investigation comment.
	investigationBody := buildInvestigationComment(resp.Findings, resp.ProposedTasks, resp.Risks)
	log.Printf("orchestrator: investigation complete for #%d, posting comment", number)
	if err := o.github.PostComment(ctx, owner, repo, number, investigationBody); err != nil {
		return fmt.Errorf("post investigation comment: %w", err)
	}

	// Move to planning.
	return o.runPlanning(ctx, owner, repo, issue, investigationBody, defaultBranch)
}

// runAgentLoop starts an ephemeral internal MCP server exposing ask_user,
// submits an agent run with both the code MCP server(s) and ask_user,
// sets the run ID on the internal MCP server, then polls for completion.
//
// If the agent calls ask_user, the internal MCP handler:
//   - posts the question to the GitHub issue
//   - sets status:blocked
//   - cancels the run via the agent client
//
// The canceled run causes PollRun to return an error, which propagates cleanly
// up to the caller.
func (o *Orchestrator) runAgentLoop(ctx context.Context, agentName, initialContext string, history []agent.Message, owner, repo string, issueNumber int, mcpServers []mcpclient.ServerConfig) (*investigationResponse, error) {
	log.Printf("orchestrator: runAgentLoop agent=%q issue=#%d (%s/%s)", agentName, issueNumber, owner, repo)

	// 1. Create a session on the shared MCP manager (no new port opened).
	sessionID, cleanup := o.mcpManager.CreateSession(owner, repo, issueNumber, o.github, o, o.agent)
	defer cleanup()

	// 2. Prepend ask_user to the MCP server list.
	allServers := append([]mcpclient.ServerConfig{
		{
			Name:      "ask_user",
			URL:       o.mcpManager.MCPEndpoint(sessionID),
			Transport: "streamable-http",
		},
	}, mcpServers...)

	// 3. Start the run — get the run ID immediately.
	runID, err := o.agent.StartRun(ctx, agent.Request{
		AgentName:    agentName,
		Context:      initialContext,
		History:      history,
		MCPServers:   allServers,
		ResponseJSON: true,
		ResponseSchema: &agent.ResponseSchema{
			Name:   "investigation_result",
			Strict: true,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"findings":       map[string]any{"type": "string"},
					"proposed_tasks": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"risks":          map[string]any{"type": "string"},
				},
				"required":             []string{"findings", "proposed_tasks", "risks"},
				"additionalProperties": false,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agent start run: %w", err)
	}
	log.Printf("orchestrator: agent run started runID=%q agent=%q issue=#%d", runID, agentName, issueNumber)

	// 4. Tell the session which run to cancel if ask_user is called.
	o.mcpManager.SetRunID(sessionID, runID)

	// 5. Poll until the run reaches a terminal state.
	resp, pollErr := o.agent.PollRun(ctx, runID)
	if pollErr != nil {
		// Canceled runs mean ask_user was called — status:blocked already set.
		log.Printf("orchestrator: agent run %q ended with error: %v", runID, pollErr)
		return nil, fmt.Errorf("agent send: %w", pollErr)
	}

	log.Printf("orchestrator: agent %q completed", agentName)

	var result investigationResponse
	if err := json.Unmarshal([]byte(resp.Text), &result); err != nil {
		return nil, fmt.Errorf("unmarshal investigation response: %w", err)
	}

	return &result, nil
}

// buildIssueContext constructs a context string from an issue.
func buildIssueContext(issue *github.Issue) string {
	return fmt.Sprintf("## Issue #%d: %s\n\n%s",
		issue.GetNumber(),
		issue.GetTitle(),
		issue.GetBody(),
	)
}

// buildCommentHistory converts GitHub issue comments into agent.Message history
// so that the agent has full conversational context when a run is resumed.
//
// Comments posted by GitHub Apps (bot accounts, identified by ending in "[bot]")
// are treated as assistant messages; all others are treated as user messages.
func buildCommentHistory(comments []*github.IssueComment) []agent.Message {
	if len(comments) == 0 {
		return nil
	}
	msgs := make([]agent.Message, 0, len(comments))
	for _, c := range comments {
		role := "user"
		if login := c.GetUser().GetLogin(); strings.HasSuffix(login, "[bot]") {
			role = "assistant"
		}
		msgs = append(msgs, agent.Message{
			Role:    role,
			Content: c.GetBody(),
		})
	}
	return msgs
}

// buildInvestigationComment formats the investigation results as a GitHub comment.
func buildInvestigationComment(findings string, proposedTasks []string, risks string) string {
	if findings == "" {
		findings = "(see above)"
	}
	if len(proposedTasks) == 0 {
		proposedTasks = []string{"Implement the requested changes"}
	}
	if risks == "" {
		risks = "None identified"
	}

	var taskList strings.Builder
	for _, task := range proposedTasks {
		taskList.WriteString(fmt.Sprintf("- [ ] %s\n", task))
	}

	return fmt.Sprintf(`## Investigation Complete

### Findings
%s

### Proposed Tasks
%s
### Risks
%s`, findings, taskList.String(), risks)
}

// findInvestigationComment returns the body of the first "## Investigation Complete" comment.
func findInvestigationComment(comments []*github.IssueComment) string {
	for _, c := range comments {
		if strings.Contains(c.GetBody(), "## Investigation Complete") {
			return c.GetBody()
		}
	}
	return ""
}

// findPlanComment returns the body of the first "## Plan" comment posted by planning.
func findPlanComment(comments []*github.IssueComment) string {
	for _, c := range comments {
		if strings.Contains(c.GetBody(), "## Plan") {
			return c.GetBody()
		}
	}
	return ""
}
