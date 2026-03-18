package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/mcpclient"
)

// runInvestigation drives the investigation phase for an issue.
//
// Steps:
//  1. Set status:investigating
//  2. Determine the default branch and set up code-mcp repo/worktree for read access
//  3. Build initial context from issue title + body
//  4. Loop: send to agent (with read MCP) until done:true or budget exceeded
//     - If agent asks a question (not done) → post comment, set status:blocked
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

	// Build initial context.
	issueContext := buildIssueContext(issue)
	agentCtx := fmt.Sprintf(
		"You are investigating a GitHub issue. Use the available MCP tools to explore the codebase and propose a plan.\n\n%s",
		issueContext,
	)

	findings, proposedTasks, risks, err := o.runAgentLoop(ctx, "investigation", agentCtx, owner, repo, number, readMCP)
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

// runAgentLoop sends messages to the agent in a loop until the agent signals
// done:true or the tool budget is exhausted. Returns parsed findings/tasks/risks.
// mcpServers is forwarded to every agent.Request so the agent can use MCP tools.
func (o *Orchestrator) runAgentLoop(ctx context.Context, phase, initialContext, owner, repo string, issueNumber int, mcpServers []mcpclient.ServerConfig) (findings, proposedTasks, risks string, err error) {
	agentCtx := initialContext
	budget := o.config.ToolBudget

	for i := 0; i <= budget; i++ {
		resp, sendErr := o.agent.Send(ctx, agent.Request{
			Phase:      phase,
			Context:    agentCtx,
			MCPServers: mcpServers,
		})
		if sendErr != nil {
			return "", "", "", fmt.Errorf("agent send: %w", sendErr)
		}

		log.Printf("orchestrator: agent phase=%s done=%v", phase, resp.Done)

		if resp.Done {
			// Agent finished — parse its final text for structured output.
			findings, proposedTasks, risks = parseInvestigationResponse(resp.Text)
			return findings, proposedTasks, risks, nil
		}

		// Agent returned text with done=false → it's asking a question.
		blockedMsg := fmt.Sprintf(
			"I need some clarification before I can proceed:\n\n%s\n\n"+
				"Please reply mentioning @opendev-git to continue.",
			resp.Text,
		)
		if postErr := o.github.PostComment(ctx, owner, repo, issueNumber, blockedMsg); postErr != nil {
			log.Printf("orchestrator: post blocked comment: %v", postErr)
		}
		if transErr := o.transitionStatus(ctx, owner, repo, issueNumber, "", "status:blocked"); transErr != nil {
			log.Printf("orchestrator: transition to blocked: %v", transErr)
		}
		return "", "", "", fmt.Errorf("agent blocked waiting for user input")
	}

	// Budget exhausted — use whatever was last parsed.
	log.Printf("orchestrator: tool budget exhausted for issue #%d", issueNumber)
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
