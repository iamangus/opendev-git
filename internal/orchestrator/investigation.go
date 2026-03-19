package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/internalmcp"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

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

	// Build initial context.
	issueContext := buildIssueContext(issue)
	agentCtx := fmt.Sprintf(
		"You are investigating a GitHub issue. Use the available MCP tools to explore the codebase and propose a plan.\n\n%s",
		issueContext,
	)

	findings, proposedTasks, risks, err := o.runAgentLoop(ctx, "investigation", agentCtx, history, owner, repo, number, readMCP)
	if err != nil {
		return err
	}

	// Build and post investigation comment.
	investigationBody := buildInvestigationComment(findings, proposedTasks, risks)
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
func (o *Orchestrator) runAgentLoop(ctx context.Context, phase, initialContext string, history []agent.Message, owner, repo string, issueNumber int, mcpServers []mcpclient.ServerConfig) (findings, proposedTasks, risks string, err error) {
	// 1. Start the ephemeral internal MCP server (run ID not yet known).
	mcpSrv, err := internalmcp.New(owner, repo, issueNumber, o.github, o, o.agent)
	if err != nil {
		return "", "", "", fmt.Errorf("start internal MCP server: %w", err)
	}
	defer mcpSrv.Close()

	// 2. Prepend ask_user to the MCP server list.
	allServers := append([]mcpclient.ServerConfig{
		{
			Name:      "ask_user",
			URL:       mcpSrv.MCPEndpoint(),
			Transport: "streamable-http",
		},
	}, mcpServers...)

	// 3. Start the run — get the run ID immediately.
	runID, err := o.agent.StartRun(ctx, agent.Request{
		Phase:      phase,
		Context:    initialContext,
		History:    history,
		MCPServers: allServers,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("agent start run: %w", err)
	}

	// 4. Tell the internal MCP server which run to cancel if ask_user is called.
	mcpSrv.SetRunID(runID)

	// 5. Poll until the run reaches a terminal state.
	resp, pollErr := o.agent.PollRun(ctx, runID)
	if pollErr != nil {
		// Canceled runs mean ask_user was called — status:blocked already set.
		return "", "", "", fmt.Errorf("agent send: %w", pollErr)
	}

	log.Printf("orchestrator: agent phase=%s completed", phase)

	findings, proposedTasks, risks = parseInvestigationResponse(resp.Text)
	return findings, proposedTasks, risks, nil
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
func buildInvestigationComment(findings, proposedTasks, risks string) string {
	if findings == "" {
		findings = "(see above)"
	}
	if proposedTasks == "" {
		proposedTasks = "- [ ] Implement the requested changes"
	}
	if risks == "" {
		risks = "None identified"
	}
	return fmt.Sprintf(`## Investigation Complete

### Findings
%s

### Proposed Tasks
%s

### Risks
%s`, findings, proposedTasks, risks)
}

// parseInvestigationResponse extracts findings, tasks, and risks from the agent's
// final text. It looks for the section headers used in buildInvestigationComment.
func parseInvestigationResponse(text string) (findings, proposedTasks, risks string) {
	sections := map[string]*string{
		"### Findings":       &findings,
		"### Proposed Tasks": &proposedTasks,
		"### Risks":          &risks,
	}

	lines := strings.Split(text, "\n")
	var current *string
	var buf strings.Builder

	for _, line := range lines {
		if ptr, ok := sections[strings.TrimSpace(line)]; ok {
			if current != nil {
				*current = strings.TrimSpace(buf.String())
			}
			current = ptr
			buf.Reset()
			continue
		}
		if current != nil {
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	if current != nil {
		*current = strings.TrimSpace(buf.String())
	}

	// If the agent didn't use structured sections, use the whole text as findings.
	if findings == "" && proposedTasks == "" {
		findings = text
	}
	return
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
