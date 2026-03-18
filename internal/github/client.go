package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/config"
)

// Client wraps the go-github client with helper methods.
type Client struct {
	gh *github.Client
}

// NewClient creates a GitHub client authenticated as a GitHub App installation.
// installationID is derived from the webhook payload at runtime; pass 0 to create
// an unauthenticated client (useful for testing) or use NewClientForInstallation
// once you have the installation ID from a webhook event.
func NewClient(cfg *config.Config, installationID int64) (*Client, error) {
	privateKeyBytes := []byte(cfg.GitHubPrivateKey)

	itr, err := ghinstallation.New(
		http.DefaultTransport,
		cfg.GitHubAppID,
		installationID,
		privateKeyBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("create ghinstallation transport: %w", err)
	}

	gh := github.NewClient(&http.Client{Transport: itr})
	return &Client{gh: gh}, nil
}

// GetIssue returns a single issue.
func (c *Client) GetIssue(ctx context.Context, owner, repo string, number int) (*github.Issue, error) {
	issue, _, err := c.gh.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", number, err)
	}
	return issue, nil
}

// PostComment posts a comment on an issue or PR.
func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	comment := &github.IssueComment{Body: github.Ptr(body)}
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, comment)
	if err != nil {
		return fmt.Errorf("post comment on issue %d: %w", number, err)
	}
	return nil
}

// AddLabel adds a label to an issue.
func (c *Client) AddLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label})
	if err != nil {
		return fmt.Errorf("add label %q to issue %d: %w", label, number, err)
	}
	return nil
}

// RemoveLabel removes a label from an issue. Ignores 404 (label not present).
func (c *Client) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label)
	if err != nil {
		// 404 means the label wasn't present — that's fine.
		if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("remove label %q from issue %d: %w", label, number, err)
	}
	return nil
}

// EnsureLabel creates the label in the repo if it doesn't already exist.
func (c *Client) EnsureLabel(ctx context.Context, owner, repo, name, color, description string) error {
	_, _, err := c.gh.Issues.GetLabel(ctx, owner, repo, name)
	if err == nil {
		return nil // already exists
	}
	ghErr, ok := err.(*github.ErrorResponse)
	if !ok || ghErr.Response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("check label %q: %w", name, err)
	}

	label := &github.Label{
		Name:        github.Ptr(name),
		Color:       github.Ptr(color),
		Description: github.Ptr(description),
	}
	_, _, err = c.gh.Issues.CreateLabel(ctx, owner, repo, label)
	if err != nil {
		return fmt.Errorf("create label %q: %w", name, err)
	}
	return nil
}

// GetComments returns all comments for an issue.
func (c *Client) GetComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []*github.IssueComment
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list comments for issue %d: %w", number, err)
		}
		all = append(all, comments...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// GetDefaultBranch returns the default branch name and its HEAD SHA.
func (c *Client) GetDefaultBranch(ctx context.Context, owner, repo string) (string, string, error) {
	repository, _, err := c.gh.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", "", fmt.Errorf("get repo: %w", err)
	}
	branchName := repository.GetDefaultBranch()

	branch, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branchName, 0)
	if err != nil {
		return "", "", fmt.Errorf("get branch %q: %w", branchName, err)
	}
	sha := branch.GetCommit().GetSHA()
	return branchName, sha, nil
}

// CreateBranch creates a new branch from the given base SHA.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, branch, baseSHA string) error {
	ref := github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: baseSHA,
	}
	_, _, err := c.gh.Git.CreateRef(ctx, owner, repo, ref)
	if err != nil {
		return fmt.Errorf("create branch %q: %w", branch, err)
	}
	return nil
}

// CreatePR opens a pull request.
func (c *Client) CreatePR(ctx context.Context, owner, repo, title, body, head, base string) (*github.PullRequest, error) {
	pr := &github.NewPullRequest{
		Title: github.Ptr(title),
		Body:  github.Ptr(body),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
	}
	created, _, err := c.gh.PullRequests.Create(ctx, owner, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("create PR: %w", err)
	}
	return created, nil
}

// CreateOrUpdateFile creates or updates a single file in the repository.
// Pass an empty sha when creating; pass the existing blob SHA when updating.
func (c *Client) CreateOrUpdateFile(ctx context.Context, owner, repo, path, message, branch string, content []byte, sha string) error {
	opts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(message),
		Content: content,
		Branch:  github.Ptr(branch),
	}
	if sha != "" {
		opts.SHA = github.Ptr(sha)
	}
	_, _, err := c.gh.Repositories.CreateFile(ctx, owner, repo, path, opts)
	if err != nil {
		return fmt.Errorf("create/update file %q: %w", path, err)
	}
	return nil
}

// UpdateComment edits the body of an existing issue comment.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	comment := &github.IssueComment{Body: github.Ptr(body)}
	_, _, err := c.gh.Issues.EditComment(ctx, owner, repo, commentID, comment)
	if err != nil {
		return fmt.Errorf("update comment %d: %w", commentID, err)
	}
	return nil
}

// GetFileSHA returns the blob SHA of a file on the given branch, or "" if not found.
func (c *Client) GetFileSHA(ctx context.Context, owner, repo, path, branch string) (string, error) {
	opts := &github.RepositoryContentGetOptions{Ref: branch}
	fileContent, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response.StatusCode == http.StatusNotFound {
			return "", nil
		}
		return "", fmt.Errorf("get file SHA for %q: %w", path, err)
	}
	if fileContent == nil {
		return "", nil
	}
	return fileContent.GetSHA(), nil
}
