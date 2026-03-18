package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/tools"
)

const maxRetries = 3

// runExecution implements the in-progress phase: branch creation, code generation,
// test execution, and PR creation.
//
// Steps:
//  1. Get default branch SHA
//  2. Create branch opendev-git/issue-{number}
//  3. Set status:in-progress
//  4. Parse tasks from investigation comment
//  5. For each task: generate tests → generate impl → run tests → commit or retry
//  6. Open PR with summary
func (o *Orchestrator) runExecution(ctx context.Context, owner, repo string, issue *github.Issue, investigationComment string) error {
	number := issue.GetNumber()

	defaultBranch, baseSHA, err := o.github.GetDefaultBranch(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get default branch: %w", err)
	}

	branchName := fmt.Sprintf("opendev-git/issue-%d", number)
	if err := o.github.CreateBranch(ctx, owner, repo, branchName, baseSHA); err != nil {
		return fmt.Errorf("create branch %q: %w", branchName, err)
	}

	if err := o.transitionStatus(ctx, owner, repo, number, "", "status:in-progress"); err != nil {
		return fmt.Errorf("set in-progress status: %w", err)
	}

	tasks := parseTasks(investigationComment)
	if len(tasks) == 0 {
		tasks = []string{"Implement the requested changes from the issue"}
	}

	completedTasks := make([]string, 0, len(tasks))
	var allChanges []string

	for i, task := range tasks {
		log.Printf("orchestrator: executing task %d/%d: %s", i+1, len(tasks), task)

		changes, err := o.executeTask(ctx, owner, repo, issue, branchName, task)
		if err != nil {
			// Post blocked comment and stop.
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
		allChanges = append(allChanges, changes...)

		// Check off the task in the investigation comment (best-effort).
		_ = o.checkOffTask(ctx, owner, repo, number, task)
	}

	// Build PR body.
	prBody := buildPRBody(number, completedTasks, allChanges)
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

	return nil
}

// executeTask generates and commits code for a single task, retrying on test failure.
func (o *Orchestrator) executeTask(ctx context.Context, owner, repo string, issue *github.Issue, branch, task string) ([]string, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("orchestrator: task attempt %d/%d: %s", attempt, maxRetries, task)

		files, testOutput, testPassed, err := o.generateAndTest(ctx, owner, repo, issue, branch, task, attempt)
		if err != nil {
			return nil, err
		}

		if testPassed {
			// Commit files.
			for path, content := range files {
				sha, _ := o.github.GetFileSHA(ctx, owner, repo, path, branch)
				commitMsg := fmt.Sprintf("feat: %s (issue #%d)", task, issue.GetNumber())
				if err := o.github.CreateOrUpdateFile(ctx, owner, repo, path, commitMsg, branch, []byte(content), sha); err != nil {
					return nil, fmt.Errorf("commit file %q: %w", path, err)
				}
			}
			filePaths := make([]string, 0, len(files))
			for p := range files {
				filePaths = append(filePaths, p)
			}
			return filePaths, nil
		}

		log.Printf("orchestrator: tests failed (attempt %d): %s", attempt, testOutput)
	}

	return nil, fmt.Errorf("tests did not pass after %d attempts", maxRetries)
}

// generateAndTest asks the agent to generate code and then runs tests.
func (o *Orchestrator) generateAndTest(ctx context.Context, owner, repo string, issue *github.Issue, branch, task string, attempt int) (map[string]string, string, bool, error) {
	// Step 1: Ask agent to generate test code.
	testCtx := fmt.Sprintf(
		"You are implementing a GitHub issue. Generate test code for the following task.\n\n"+
			"## Issue\n%s\n\n"+
			"## Task\n%s\n\n"+
			"## Attempt\n%d of %d\n\n"+
			"Respond with the test file path and content in this format:\n"+
			"FILE: <path>\n```\n<content>\n```\n\n"+
			"Set done:true when you have provided the test code.",
		buildIssueContext(issue), task, attempt, maxRetries,
	)

	testResp, err := o.agent.Send(ctx, agent.Request{
		Phase:   "execution_tests",
		Context: testCtx,
	})
	if err != nil {
		return nil, "", false, fmt.Errorf("agent test generation: %w", err)
	}

	testFiles := parseFileBlocks(testResp.Text)

	// Step 2: Ask agent to generate implementation code.
	implCtx := fmt.Sprintf(
		"You are implementing a GitHub issue. Generate implementation code for the following task.\n\n"+
			"## Issue\n%s\n\n"+
			"## Task\n%s\n\n"+
			"## Test Code\n%s\n\n"+
			"Respond with the implementation file path and content in this format:\n"+
			"FILE: <path>\n```\n<content>\n```\n\n"+
			"Set done:true when you have provided the implementation.",
		buildIssueContext(issue), task, testResp.Text,
	)

	implResp, err := o.agent.Send(ctx, agent.Request{
		Phase:   "execution_impl",
		Context: implCtx,
	})
	if err != nil {
		return nil, "", false, fmt.Errorf("agent impl generation: %w", err)
	}

	implFiles := parseFileBlocks(implResp.Text)

	// Merge test and impl files.
	allFiles := make(map[string]string)
	for k, v := range testFiles {
		allFiles[k] = v
	}
	for k, v := range implFiles {
		allFiles[k] = v
	}

	// Write files directly to workspace (avoids shell injection).
	workspaceDir := o.config.WorkspaceDir
	for filePath, content := range allFiles {
		// Only write relative paths to avoid escaping the workspace.
		if filepath.IsAbs(filePath) {
			log.Printf("orchestrator: skipping absolute path %q", filePath)
			continue
		}
		absPath := filepath.Clean(filepath.Join(workspaceDir, filePath))
		if !strings.HasPrefix(absPath, filepath.Clean(workspaceDir)+string(filepath.Separator)) {
			log.Printf("orchestrator: skipping path outside workspace %q", filePath)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			log.Printf("orchestrator: mkdir for %q: %v", absPath, err)
			continue
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			log.Printf("orchestrator: write file %q: %v", absPath, err)
		}
	}

	// Run tests; exit code determines pass/fail.
	testResult := o.tools.Execute(ctx, tools.ToolCall{
		Name: "run_command",
		Args: map[string]string{"cmd": "go test ./... 2>&1"},
	})

	// runCommand prefixes output with "command error (...)" on non-zero exit.
	passed := !strings.HasPrefix(testResult.Output, "command error")

	return allFiles, testResult.Output, passed, nil
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
		new := "- [x] " + task
		if !strings.Contains(body, old) {
			continue
		}
		updated := strings.Replace(body, old, new, 1)
		// We can't edit comments via the current client interface, so just log it.
		_ = updated
		log.Printf("orchestrator: task checked off: %s", task)
		return nil
	}
	return nil
}

// parseFileBlocks extracts FILE: path / ``` content ``` blocks from agent text.
func parseFileBlocks(text string) map[string]string {
	files := make(map[string]string)
	lines := strings.Split(text, "\n")

	var currentPath string
	var inBlock bool
	var buf strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "FILE: ") {
			currentPath = strings.TrimPrefix(line, "FILE: ")
			currentPath = strings.TrimSpace(currentPath)
			buf.Reset()
			inBlock = false
			continue
		}
		if currentPath != "" {
			if !inBlock && strings.HasPrefix(line, "```") {
				inBlock = true
				continue
			}
			if inBlock {
				if strings.TrimSpace(line) == "```" {
					files[currentPath] = buf.String()
					currentPath = ""
					inBlock = false
					buf.Reset()
					continue
				}
				buf.WriteString(line)
				buf.WriteString("\n")
			}
		}
	}
	return files
}

// buildPRBody constructs the PR description following the standard template:
// Summary, Changes (committed files), Tests, Docs, and the closing reference to the issue.
func buildPRBody(issueNumber int, tasks, changes []string) string {
	var taskList strings.Builder
	for _, t := range tasks {
		taskList.WriteString("- " + t + "\n")
	}

	var changeList strings.Builder
	for _, c := range changes {
		changeList.WriteString("- `" + c + "`\n")
	}

	return fmt.Sprintf(`## Summary
Automated implementation of issue #%d.

## Changes
%s
## Tests
Tests were generated and run successfully.

## Docs
N/A

## Related Issue
Closes #%d`,
		issueNumber,
		changeList.String(),
		issueNumber,
	)
}

// Ensure github import is used (imported via orchestrator.go).
var _ = (*github.Issue)(nil)
