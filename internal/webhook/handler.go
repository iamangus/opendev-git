package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/config"
	githubclient "github.com/iamangus/opendev-git/internal/github"
	"github.com/iamangus/opendev-git/internal/orchestrator"
)

// Handler handles incoming GitHub webhook events.
type Handler struct {
	config       *config.Config
	orchestrator *orchestrator.Orchestrator
}

// NewHandler creates a new webhook Handler.
func NewHandler(cfg *config.Config, orch *orchestrator.Orchestrator) *Handler {
	return &Handler{
		config:       cfg,
		orchestrator: orch,
	}
}

// ServeHTTP processes incoming webhook POST requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("webhook: read body: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if !h.verifySignature(body, r.Header.Get("X-Hub-Signature-256")) {
		log.Printf("webhook: signature verification failed")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	log.Printf("webhook: received event type=%q", eventType)

	switch eventType {
	case "issues":
		h.handleIssuesEvent(body)
	case "issue_comment":
		h.handleIssueCommentEvent(body)
	default:
		log.Printf("webhook: unsupported event type %q — ignoring", eventType)
	}

	w.WriteHeader(http.StatusOK)
}

// issuesPayload is the minimal structure we need from a GitHub issues event.
type issuesPayload struct {
	Action       string        `json:"action"`
	Issue        *github.Issue `json:"issue"`
	Label        *github.Label `json:"label"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// issueCommentPayload is the minimal structure we need from a GitHub issue_comment event.
type issueCommentPayload struct {
	Action       string               `json:"action"`
	Issue        *github.Issue        `json:"issue"`
	Comment      *github.IssueComment `json:"comment"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

func (h *Handler) handleIssuesEvent(body []byte) {
	var payload issuesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("webhook: parse issues payload: %v", err)
		return
	}

	if payload.Issue == nil {
		return
	}

	log.Printf("webhook: received issues event action=%q issue=#%d", payload.Action, payload.Issue.GetNumber())

	// Only act on "labeled" events. GitHub fires a "labeled" event for every
	// label added to an issue — including labels applied at creation time — so
	// this single case handles both "new issue with trigger label" and
	// "trigger label added to existing issue". Listening to "opened" as well
	// would cause a duplicate run whenever a new issue is created with the
	// trigger label already attached.
	if payload.Action != "labeled" {
		log.Printf("webhook: issues action=%q — ignoring (only 'labeled' is handled)", payload.Action)
		return
	}

	// Only proceed if the label being added IS the designated trigger label.
	// This prevents status:* transitions we make ourselves from re-triggering
	// the workflow (the feedback loop bug).
	addedLabel := payload.Label.GetName()
	if addedLabel != h.config.DesignatedLabel {
		log.Printf("webhook: issues#labeled #%d — added label %q is not the designated trigger label, ignoring",
			payload.Issue.GetNumber(), addedLabel)
		return
	}

	owner := payload.Repository.Owner.Login
	repo := payload.Repository.Name
	if owner == "" {
		owner = h.config.RepoOwner
	}
	if repo == "" {
		repo = h.config.RepoName
	}

	issue := payload.Issue
	installationID := payload.Installation.ID

	log.Printf("webhook: dispatching HandleIssue for #%d (%s/%s)", issue.GetNumber(), owner, repo)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		gh, err := githubclient.NewClient(h.config, installationID)
		if err != nil {
			log.Printf("webhook: create github client for installation %d: %v", installationID, err)
			return
		}
		orch := h.orchestrator.WithGitHubClient(gh)
		if err := orch.HandleIssue(ctx, owner, repo, issue); err != nil {
			log.Printf("webhook: HandleIssue #%d: %v", issue.GetNumber(), err)
		}
	}()
}

func (h *Handler) handleIssueCommentEvent(body []byte) {
	var payload issueCommentPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("webhook: parse issue_comment payload: %v", err)
		return
	}

	if payload.Action != "created" {
		return
	}
	if payload.Issue == nil || payload.Comment == nil {
		return
	}

	log.Printf("webhook: received issue_comment event action=%q issue=#%d author=%q",
		payload.Action, payload.Issue.GetNumber(), payload.Comment.GetUser().GetLogin())

	// Ignore comments from bot accounts (including ourselves) to prevent
	// feedback loops where the bot's own ask_user message triggers a new run.
	if login := payload.Comment.GetUser().GetLogin(); strings.HasSuffix(login, "[bot]") {
		log.Printf("webhook: issue_comment #%d — ignoring bot comment from %q", payload.Issue.GetNumber(), login)
		return
	}

	commentBody := payload.Comment.GetBody()
	if !strings.Contains(commentBody, "@opendev-git") {
		log.Printf("webhook: issue_comment #%d — no @opendev-git mention, ignoring", payload.Issue.GetNumber())
		return
	}

	owner := payload.Repository.Owner.Login
	repo := payload.Repository.Name
	if owner == "" {
		owner = h.config.RepoOwner
	}
	if repo == "" {
		repo = h.config.RepoName
	}

	issue := payload.Issue
	comment := payload.Comment
	installationID := payload.Installation.ID

	log.Printf("webhook: dispatching HandleMention for #%d (%s/%s)", issue.GetNumber(), owner, repo)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		gh, err := githubclient.NewClient(h.config, installationID)
		if err != nil {
			log.Printf("webhook: create github client for installation %d: %v", installationID, err)
			return
		}
		orch := h.orchestrator.WithGitHubClient(gh)
		if err := orch.HandleMention(ctx, owner, repo, issue, comment); err != nil {
			log.Printf("webhook: HandleMention #%d: %v", issue.GetNumber(), err)
		}
	}()
}

// verifySignature checks the X-Hub-Signature-256 header using HMAC-SHA256.
func (h *Handler) verifySignature(body []byte, signature string) bool {
	if h.config.GitHubWebhookSecret == "" {
		return true
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sig := strings.TrimPrefix(signature, prefix)

	mac := hmac.New(sha256.New, []byte(h.config.GitHubWebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expected))
}
