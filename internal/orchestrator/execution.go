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

const maxRetries = 3

// runExecution implements the in-progress phase: branch creation, code generation,
// test execution via code-mcp, and PR creation.
//
// Steps:
//  1. Ensure repo is present and up to date in code-mcp
//  2. Create branch via GitHub API
//  3. Create branch worktree in code-mcp
//  4. Set status:in-progress
//  5. Parse tasks from investigation comment
//  6. For each task: agent writes code via write MCP → run tests → retry on failure
//  7. Push branch to origin via code-mcp
//  8. Open PR
//
// 9. Async cleanup of worktree
func (o *Orchestrator) runExecution(ctx context.Context, owner, repo string, issue *github.Issue, investigationComment, defaultBranch string) error {
	number := issue.GetNumber()
	log.Printf("orchestrator: starting execution for #%d (%s/%s)", number, owner, repo)

	_, baseSHA, err := o.github.GetDefaultBranch(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get default branch: %w", err)
	}

	// Ensure the repo is present and up to date in code-mcp (idempotent).
	cloneURL := "https://github.com/" + owner + "/" + repo
	if err := o.codemcp.EnsureRepo(ctx, repo, cloneURL); err != nil {
		return fmt.Errorf("ensure repo in code-mcp: %w", err)
	}

	branchName := fmt.Sprintf("opendev-git-issue-%d", number)

	// Create the git ref on GitHub.
	if err := o.github.CreateBranch(ctx, owner, repo, branchName, baseSHA); err != nil {
		return fmt.Errorf("create branch %q: %w", branchName, err)
	}

	// Create the worktree in code-mcp.
	if err := o.codemcp.EnsureBranch(ctx, repo, branchName, defaultBranch); err != nil {
		return fmt.Errorf("create branch worktree in code-mcp: %w", err)
	}

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:in-progress"); err != nil {
		return fmt.Errorf("set in-progress status: %w", err)
	}

	tasks := parseTasks(investigationComment)
	if len(tasks) == 0 {
		tasks = []string{"Implement the requested changes from the issue"}
	}

	completedTasks := make([]string, 0, len(tasks))

	for i, task := range tasks {
		log.Printf("orchestrator: executing task %d/%d: %s", i+1, len(tasks), task)

		if err := o.executeTask(ctx, owner, repo, issue, branchName, task); err != nil {
			blockedMsg := fmt.Sprintf(
				"I was unable to complete task **%s** after %d attempts.\n\nError: %s\n\n"+
					"Please review and reply mentioning @opendev-git to retry.",
				task, maxRetries, err.Error(),
			)
			_ = o.github.PostComment(ctx, owner, repo, number, blockedMsg)
			_ = o.transitionStatus(ctx, owner, repo, number, "", "status:blocked")
			return fmt.Errorf("task %q failed: %w", task, err)
		}

		completedTasks = append(completedTasks, task)

		// Check off the task in the investigation comment (best-effort).
		_ = o.checkOffTask(ctx, owner, repo, number, task)
	}

	// Push branch to origin before opening the PR.
	if err := o.codemcp.PushBranch(ctx, repo, branchName); err != nil {
		return fmt.Errorf("push branch %q: %w", branchName, err)
	}

	// Open the PR.
	prBody := buildPRBody(number, completedTasks)
	prTitle := fmt.Sprintf("fix: resolve issue #%d", number)

	pr, err := o.github.CreatePR(ctx, owner, repo, prTitle, prBody, branchName, defaultBranch)
	if err != nil {
		return fmt.Errorf("create PR: %w", err)
	}

	log.Printf("orchestrator: PR #%d created: %s", pr.GetNumber(), pr.GetHTMLURL())

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:done"); err != nil {
		log.Printf("orchestrator: set done status: %v", err)
	}

	doneMsg := fmt.Sprintf(
		"✅ Implementation complete! PR #%d has been opened: %s",
		pr.GetNumber(), pr.GetHTMLURL(),
	)
	_ = o.github.PostComment(ctx, owner, repo, number, doneMsg)

	// Best-effort async cleanup of the worktree.
	go func() {
		if err := o.codemcp.DeleteBranch(context.Background(), repo, branchName); err != nil {
			log.Printf("orchestrator: cleanup worktree %q/%q: %v", repo, branchName, err)
		}
	}()

	return nil
}

// executeTask asks the agent to implement a single task via the write MCP endpoint,
// then runs tests via code-mcp. Retries up to maxRetries times on test failure.
func (o *Orchestrator) executeTask(ctx context.Context, owner, repo string, issue *github.Issue, branch, task string) error {
	writeMCP := []mcpclient.ServerConfig{{
		Name:      "code",
		URL:       o.codemcp.MCPWriteURL(repo, branch),
		Transport: "streamable-http",
	}}

	var lastTestOutput string

	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("orchestrator: task attempt %d/%d: %s", attempt, maxRetries, task)

		if err := o.generateCode(ctx, issue, branch, task, attempt, lastTestOutput, writeMCP); err != nil {
			return err
		}

		passed, output, err := o.codemcp.RunTests(ctx, repo, branch)
		if err != nil {
			return fmt.Errorf("run tests (attempt %d): %w", attempt, err)
		}

		if passed {
			return nil
		}

		log.Printf("orchestrator: tests failed (attempt %d): %s", attempt, output)
		lastTestOutput = output
	}

	return fmt.Errorf("tests did not pass after %d attempts", maxRetries)
}

// generateCode asks the agent to implement the task by writing files directly
// into the worktree via the write MCP endpoint.
func (o *Orchestrator) generateCode(ctx context.Context, issue *github.Issue, branch, task string, attempt int, previousTestOutput string, mcpServers []mcpclient.ServerConfig) error {
	retryContext := ""
	if previousTestOutput != "" {
		retryContext = fmt.Sprintf("\n\n## Previous Test Failure (attempt %d)\n%s\n\nPlease fix the above failures.", attempt-1, previousTestOutput)
	}

	implCtx := fmt.Sprintf(
		"You are implementing a GitHub issue. Use the MCP tools to write code directly into the repository worktree.\n\n"+
			"## Issue\n%s\n\n"+
			"## Task\n%s\n\n"+
			"## Branch\n%s\n\n"+
			"## Attempt\n%d of %d\n"+
			"%s\n\n"+
			"Use the create_file and search_and_replace MCP tools to write implementation and test files directly. "+
			"When you have finished writing all necessary files, respond with done:true.",
		buildIssueContext(issue), task, branch, attempt, maxRetries, retryContext,
	)

	resp, err := o.agent.Send(ctx, agent.Request{
		AgentName:  o.config.AgentExecution,
		Context:    implCtx,
		MCPServers: mcpServers,
	})
	if err != nil {
		return fmt.Errorf("agent execution send: %w", err)
	}

	log.Printf("orchestrator: execution agent completed for task %q (attempt %d), response length=%d", task, attempt, len(resp.Text))
	return nil
}

// parseTasks extracts unchecked checkbox items from the investigation comment.
func parseTasks(comment string) []string {
	var tasks []string
	for _, line := range strings.Split(comment, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [ ] ") {
			task := strings.TrimPrefix(trimmed, "- [ ] ")
			if task != "" {
				tasks = append(tasks, task)
			}
		}
	}
	return tasks
}

// checkOffTask updates the investigation comment to mark a task as done.
func (o *Orchestrator) checkOffTask(ctx context.Context, owner, repo string, issueNumber int, task string) error {
	comments, err := o.github.GetComments(ctx, owner, repo, issueNumber)
	if err != nil {
		return err
	}

	for _, c := range comments {
		body := c.GetBody()
		if !strings.Contains(body, "## Investigation Complete") {
			continue
		}
		old := "- [ ] " + task
		newMark := "- [x] " + task
		if !strings.Contains(body, old) {
			continue
		}
		updated := strings.Replace(body, old, newMark, 1)
		if err := o.github.UpdateComment(ctx, owner, repo, c.GetID(), updated); err != nil {
			log.Printf("orchestrator: update comment for task check-off: %v", err)
			return err
		}
		log.Printf("orchestrator: task checked off: %s", task)
		return nil
	}
	return nil
}

// buildPRBody constructs the PR description.
func buildPRBody(issueNumber int, tasks []string) string {
	var taskList strings.Builder
	for _, t := range tasks {
		taskList.WriteString("- [x] " + t + "\n")
	}

	return fmt.Sprintf(`## Summary
Automated implementation of issue #%d.

## Tasks Completed
%s
## Related Issue
Closes #%d`,
		issueNumber,
		taskList.String(),
		issueNumber,
	)
}

// Ensure github import is used (imported via orchestrator.go).
var _ = (*github.Issue)(nil)
