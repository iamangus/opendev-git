#!/usr/bin/env bash
set -euo pipefail

export PORT="8080"
export GITHUB_APP_ID="123456"
export GITHUB_PRIVATE_KEY="$(cat /path/to/private-key.pem)"
export GITHUB_WEBHOOK_SECRET="your-webhook-secret"
export AGENT_SERVICE_URL="http://localhost:9090"
export CODE_MCP_URL="http://localhost:8081"
export DESIGNATED_LABEL="opendev-git"
export REPO_OWNER=""
export REPO_NAME=""
export TOOL_BUDGET="20"

go run ./cmd/opendev-git
