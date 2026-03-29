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

type planningResponse struct {
	Tasks   []string `json:"tasks"`
	Summary string   `json:"summary"`
}

// runPlanning receives the investigation results and produces a concrete
// implementation plan. It does not ask for clarification — that is the
// investigation phase's responsibility.
//
// Steps:
//  1. Agent reads codebase and investigation report, produces ordered task list
//  2. Post "## Plan" comment with tasks as checkboxes
//  3. Set status:approved and proceed to execution
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

	runID, err := o.agent.StartRun(ctx, agent.Request{
		AgentName:    o.config.AgentPlanning,
		Context:      planCtx,
		MCPServers:   readMCP,
		ResponseJSON: true,
		ResponseSchema: &agent.ResponseSchema{
			Name:   "planning_result",
			Strict: true,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tasks":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"summary": map[string]any{"type": "string"},
				},
				"required":             []string{"tasks", "summary"},
				"additionalProperties": false,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("planning agent start run: %w", err)
	}
	log.Printf("orchestrator: planning agent run started runID=%q issue=#%d", runID, number)

	resp, pollErr := o.agent.PollRun(ctx, runID)
	if pollErr != nil {
		log.Printf("orchestrator: planning agent run %q ended with error: %v", runID, pollErr)
		return fmt.Errorf("planning agent: %w", pollErr)
	}

	var result planningResponse
	if err := resp.Unmarshal(&result); err != nil {
		return fmt.Errorf("unmarshal planning response: %w", err)
	}

	if len(result.Tasks) == 0 {
		result.Tasks = []string{"Implement the requested changes"}
	}

	planComment := buildPlanComment(result.Summary, result.Tasks)
	log.Printf("orchestrator: planning complete for #%d, posting plan comment", number)
	if err := o.github.PostComment(ctx, owner, repo, number, planComment); err != nil {
		return fmt.Errorf("post plan comment: %w", err)
	}

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:approved"); err != nil {
		return fmt.Errorf("set approved status: %w", err)
	}
	return o.runExecution(ctx, owner, repo, issue, planComment, defaultBranch)
}

// buildPlanComment formats the planning results as a GitHub comment with
// checkbox tasks that execution can parse and check off.
func buildPlanComment(summary string, tasks []string) string {
	var taskList strings.Builder
	for _, t := range tasks {
		taskList.WriteString(fmt.Sprintf("- [ ] %s\n", t))
	}

	return fmt.Sprintf(`## Plan

%s

### Tasks
%s`, summary, taskList.String())
}
