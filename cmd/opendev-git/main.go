package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/codemcp"
	"github.com/iamangus/opendev-git/internal/config"
	githubclient "github.com/iamangus/opendev-git/internal/github"
	"github.com/iamangus/opendev-git/internal/internalmcp"
	"github.com/iamangus/opendev-git/internal/orchestrator"
	"github.com/iamangus/opendev-git/internal/webhook"
)

// activeStatusLabels are the labels that indicate an issue is mid-workflow and
// should be resumed if opendev-git restarts.
var activeStatusLabels = []string{
	"status:investigating",
	"status:planned",
	"status:in-progress",
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	agentClient := agent.NewClient(cfg.AgentServiceURL)
	codeMCPClient := codemcp.NewClient(cfg.CodeMCPURL)

	mcpManager := internalmcp.NewManager(cfg.InternalMCPURL)

	// The orchestrator is initialized without a GitHub client; per-event goroutines
	// create a client with the correct installation ID and call WithGitHubClient.
	orch := orchestrator.New(cfg, nil, agentClient, codeMCPClient, mcpManager)

	webhookHandler := webhook.NewHandler(cfg, orch)

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhookHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mcpManager.Register(mux)

	// Recover any in-flight issues that were left mid-workflow from a previous run.
	if cfg.RepoOwner != "" && cfg.RepoName != "" {
		go recoverActiveIssues(cfg, orch)
	} else {
		log.Println("startup recovery skipped: REPO_OWNER/REPO_NAME not configured")
	}

	addr := ":" + cfg.Port
	log.Printf("opendev-git listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// recoverActiveIssues queries GitHub for any open issues that carry an active
// status label and resumes orchestration for each one. This handles the case
// where opendev-git crashed or was restarted mid-workflow.
func recoverActiveIssues(cfg *config.Config, orch *orchestrator.Orchestrator) {
	// Give the HTTP server a moment to come up before making outbound calls.
	time.Sleep(5 * time.Second)

	ctx := context.Background()

	gh, err := githubclient.NewClient(cfg, 0)
	if err != nil {
		log.Printf("startup recovery: create github client: %v", err)
		return
	}

	seen := make(map[int]bool)

	for _, label := range activeStatusLabels {
		issues, err := gh.ListOpenIssuesByLabel(ctx, cfg.RepoOwner, cfg.RepoName, label)
		if err != nil {
			log.Printf("startup recovery: list issues with label %q: %v", label, err)
			continue
		}

		for _, issue := range issues {
			num := issue.GetNumber()
			if seen[num] {
				continue
			}
			seen[num] = true

			log.Printf("startup recovery: resuming issue #%d (label: %s)", num, label)

			issueCopy := issue
			orchCopy := orch.WithGitHubClient(gh)

			go func() {
				resumeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				if err := orchCopy.HandleIssue(resumeCtx, cfg.RepoOwner, cfg.RepoName, issueCopy); err != nil {
					log.Printf("startup recovery: HandleIssue #%d: %v", issueCopy.GetNumber(), err)
				}
			}()
		}
	}
}
