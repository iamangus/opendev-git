package orchestrator

import (
	"context"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/config"
	githubclient "github.com/iamangus/opendev-git/internal/github"
	"github.com/iamangus/opendev-git/internal/tools"
)

// Orchestrator drives the issue lifecycle from investigation through execution.
type Orchestrator struct {
	config *config.Config
	github *githubclient.Client
	tools  *tools.Tools
	agent  *agent.Client
}

// New creates an Orchestrator. The GitHub client can be nil at construction time
// and supplied per-event via WithGitHubClient.
func New(cfg *config.Config, gh *githubclient.Client, t *tools.Tools, a *agent.Client) *Orchestrator {
	return &Orchestrator{
		config: cfg,
		github: gh,
		tools:  t,
		agent:  a,
	}
}

// WithGitHubClient returns a shallow copy of the Orchestrator with the given GitHub client.
// Use this to create per-event orchestrators that have the correct installation auth.
func (o *Orchestrator) WithGitHubClient(gh *githubclient.Client) *Orchestrator {
	copy := *o
	copy.github = gh
	return &copy
}

// HandleIssue begins the investigation → planning → execution workflow for a new issue.
func (o *Orchestrator) HandleIssue(ctx context.Context, owner, repo string, issue *github.Issue) error {
	if err := o.runInvestigation(ctx, owner, repo, issue); err != nil {
		return err
	}
	return nil
}

// HandleMention resumes a blocked workflow when @opendev-git is mentioned in a comment.
func (o *Orchestrator) HandleMention(ctx context.Context, owner, repo string, issue *github.Issue, comment *github.IssueComment) error {
	// Find the investigation comment to resume planning/execution.
	comments, err := o.github.GetComments(ctx, owner, repo, issue.GetNumber())
	if err != nil {
		return err
	}

	investigationComment := findInvestigationComment(comments)
	if investigationComment == "" {
		// No investigation yet — run it.
		return o.runInvestigation(ctx, owner, repo, issue)
	}

	// Check current status from labels.
	if issueHasLabel(issue, "status:planned") || issueHasLabel(issue, "status:approved") {
		return o.runExecution(ctx, owner, repo, issue, investigationComment)
	}

	// Default: re-run planning.
	return o.runPlanning(ctx, owner, repo, issue, investigationComment)
}

// transitionStatus removes the old status label and adds the new one.
func (o *Orchestrator) transitionStatus(ctx context.Context, owner, repo string, number int, from, to string) error {
	statusLabels := []string{
		"status:investigating",
		"status:planned",
		"status:approved",
		"status:in-progress",
		"status:blocked",
		"status:done",
	}

	// Define colors for each status label.
	colors := map[string]string{
		"status:investigating": "fbca04",
		"status:planned":       "0075ca",
		"status:approved":      "0e8a16",
		"status:in-progress":   "e4e669",
		"status:blocked":       "d93f0b",
		"status:done":          "0e8a16",
	}

	// Ensure the new label exists.
	if err := o.github.EnsureLabel(ctx, owner, repo, to, colors[to], to); err != nil {
		return err
	}

	// Remove all existing status labels.
	for _, l := range statusLabels {
		if l == to {
			continue
		}
		_ = o.github.RemoveLabel(ctx, owner, repo, number, l)
	}

	return o.github.AddLabel(ctx, owner, repo, number, to)
}

// issueHasLabel reports whether the issue has the named label.
func issueHasLabel(issue *github.Issue, label string) bool {
	for _, l := range issue.Labels {
		if l.GetName() == label {
			return true
		}
	}
	return false
}
