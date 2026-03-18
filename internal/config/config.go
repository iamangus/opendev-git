package config

import (
	"errors"
	"os"
	"strconv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port                string
	GitHubAppID         int64
	GitHubPrivateKey    string // PEM content
	GitHubWebhookSecret string
	AgentServiceURL     string
	DesignatedLabel     string // label that triggers investigation (default: "opendev-git")
	RepoOwner           string
	RepoName            string
	WorkspaceDir        string // local workspace dir for tool operations (default: /workspace)
	ToolBudget          int    // max tool calls per phase (default: 20)
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Port:            getEnv("PORT", "8080"),
		DesignatedLabel: getEnv("DESIGNATED_LABEL", "opendev-git"),
		WorkspaceDir:    getEnv("WORKSPACE_DIR", "/workspace"),
	}

	appIDStr := os.Getenv("GITHUB_APP_ID")
	if appIDStr == "" {
		return nil, errors.New("GITHUB_APP_ID is required")
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return nil, errors.New("GITHUB_APP_ID must be a valid integer")
	}
	cfg.GitHubAppID = appID

	cfg.GitHubPrivateKey = os.Getenv("GITHUB_PRIVATE_KEY")
	if cfg.GitHubPrivateKey == "" {
		return nil, errors.New("GITHUB_PRIVATE_KEY is required")
	}

	cfg.GitHubWebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	if cfg.GitHubWebhookSecret == "" {
		return nil, errors.New("GITHUB_WEBHOOK_SECRET is required")
	}

	cfg.AgentServiceURL = os.Getenv("AGENT_SERVICE_URL")
	if cfg.AgentServiceURL == "" {
		return nil, errors.New("AGENT_SERVICE_URL is required")
	}

	cfg.RepoOwner = os.Getenv("REPO_OWNER")
	cfg.RepoName = os.Getenv("REPO_NAME")

	toolBudgetStr := getEnv("TOOL_BUDGET", "20")
	toolBudget, err := strconv.Atoi(toolBudgetStr)
	if err != nil {
		return nil, errors.New("TOOL_BUDGET must be a valid integer")
	}
	cfg.ToolBudget = toolBudget

	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
