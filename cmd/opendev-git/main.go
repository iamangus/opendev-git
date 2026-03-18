package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/iamangus/opendev-git/internal/agent"
	"github.com/iamangus/opendev-git/internal/agentapi"
	"github.com/iamangus/opendev-git/internal/codemcp"
	"github.com/iamangus/opendev-git/internal/config"
	"github.com/iamangus/opendev-git/internal/orchestrator"
	"github.com/iamangus/opendev-git/internal/webhook"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	agentClient := agent.NewClient(cfg.AgentServiceURL)
	codeMCPClient := codemcp.NewClient(cfg.CodeMCPURL)

	// The orchestrator is initialized without a GitHub client; per-event goroutines
	// create a client with the correct installation ID and call WithGitHubClient.
	orch := orchestrator.New(cfg, nil, agentClient, codeMCPClient)

	webhookHandler := webhook.NewHandler(cfg, orch)

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhookHandler)
	mux.Handle("POST /api/v1/agents/{name}/run", agentapi.NewHandler(agentClient))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	addr := ":" + cfg.Port
	log.Printf("opendev-git listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}
