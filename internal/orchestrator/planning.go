package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
)

// runPlanning evaluates the investigation results and decides whether to proceed.
//
// Steps:
//  1. Agent evaluates confidence in the proposed task list
//  2. If confident → set status:approved, proceed to execution
//  3. If not confident → post questions, set status:blocked
func (o *Orchestrator) runPlanning(ctx context.Context, owner, repo string, issue *github.Issue, investigationComment string) error {
	number := issue.GetNumber()

	planCtx := fmt.Sprintf(
		"You are reviewing an investigation report for a GitHub issue and deciding whether the plan is clear enough to implement.\n\n"+
			"## Original Issue\n%s\n\n"+
			"## Investigation Report\n%s\n\n"+
			"If the plan is clear and complete, respond with done:true. "+
			"If you need clarification, ask a question (do not set done:true).",
		buildIssueContext(issue),
		investigationComment,
	)

	resp, err := o.agent.Send(ctx, agent.Request{
		Phase:   "planning",
		Context: planCtx,
	})
	if err != nil {
		return fmt.Errorf("planning agent send: %w", err)
	}

	log.Printf("orchestrator: planning phase done=%v", resp.Done)

	if resp.Done {
		if err := o.transitionStatus(ctx, owner, repo, number, "", "status:approved"); err != nil {
			return fmt.Errorf("set approved status: %w", err)
		}
		return o.runExecution(ctx, owner, repo, issue, investigationComment)
	}

	// Agent has questions.
	if strings.TrimSpace(resp.Text) != "" {
		blockedMsg := fmt.Sprintf(
			"Before proceeding with implementation, I need some clarification:\n\n%s\n\n"+
				"Please reply mentioning @opendev-git to continue.",
			resp.Text,
		)
		if postErr := o.github.PostComment(ctx, owner, repo, number, blockedMsg); postErr != nil {
			log.Printf("orchestrator: post planning blocked comment: %v", postErr)
		}
	}

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:blocked"); err != nil {
		return fmt.Errorf("set blocked status: %w", err)
	}
	return nil
}
