package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/tools"
)

// runInvestigation drives the investigation phase for an issue.
//
// Steps:
//  1. Set status:investigating
//  2. Build initial context from issue title + body
//  3. Loop: send to agent, execute tool calls, feed results back
//     - Stop when agent says done:true or tool budget exceeded
//     - If agent asks a question (no tool calls, not done) → post comment, set status:blocked
//  4. Post "## Investigation Complete" comment
//  5. Transition to planning phase
func (o *Orchestrator) runInvestigation(ctx context.Context, owner, repo string, issue *github.Issue) error {
	number := issue.GetNumber()

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:investigating"); err != nil {
		return fmt.Errorf("set investigating status: %w", err)
	}

	// Build initial context.
	issueContext := buildIssueContext(issue)

	agentCtx := fmt.Sprintf(
		"You are investigating a GitHub issue. Use the available tools to understand the codebase and propose a plan.\n\n%s",
		issueContext,
	)

	findings, proposedTasks, risks, err := o.runAgentLoop(ctx, "investigation", agentCtx, owner, repo, number)
	if err != nil {
		return err
	}

	// Build and post investigation comment.
	investigationBody := buildInvestigationComment(findings, proposedTasks, risks)
	if err := o.github.PostComment(ctx, owner, repo, number, investigationBody); err != nil {
		return fmt.Errorf("post investigation comment: %w", err)
	}

	// Move to planning.
	return o.runPlanning(ctx, owner, repo, issue, investigationBody)
}

// runAgentLoop sends messages to the agent in a loop, executing tool calls until
// the agent is done or the tool budget is exhausted. Returns parsed findings/tasks/risks.
func (o *Orchestrator) runAgentLoop(ctx context.Context, phase, initialContext, owner, repo string, issueNumber int) (findings, proposedTasks, risks string, err error) {
	agentCtx := initialContext
	budget := o.config.ToolBudget

	for i := 0; i <= budget; i++ {
		resp, sendErr := o.agent.Send(ctx, agent.Request{
			Phase:   phase,
			Context: agentCtx,
		})
		if sendErr != nil {
			return "", "", "", fmt.Errorf("agent send: %w", sendErr)
		}

		log.Printf("orchestrator: agent phase=%s done=%v tool_calls=%d", phase, resp.Done, len(resp.ToolCalls))

		if resp.Done {
			// Agent finished — parse its final text for structured output.
			findings, proposedTasks, risks = parseInvestigationResponse(resp.Text)
			return findings, proposedTasks, risks, nil
		}

		if len(resp.ToolCalls) > 0 {
			// Execute tool calls and append results.
			var sb strings.Builder
			sb.WriteString(agentCtx)
			sb.WriteString("\n\n### Agent Response\n")
			sb.WriteString(resp.Text)
			sb.WriteString("\n\n### Tool Results\n")

			for _, tc := range resp.ToolCalls {
				result := o.tools.Execute(ctx, tools.ToolCall{
					Name: tc.Name,
					Args: tc.Args,
				})
				sb.WriteString(fmt.Sprintf("\n#### %s\n", tc.Name))
				if result.Error != "" {
					sb.WriteString(fmt.Sprintf("Error: %s\n", result.Error))
				} else {
					sb.WriteString(result.Output)
					sb.WriteString("\n")
				}
			}
			agentCtx = sb.String()
			continue
		}

		// Agent returned text with no tool calls and done=false → it's asking a question.
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

	// Budget exhausted — use whatever the last response was.
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
