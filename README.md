# opendev-git

opendev-git is an autonomous agentic coding system written in Go that monitors a GitHub repository for issues and implements them end-to-end — investigating, planning, writing code, running tests, and opening a pull request — without human intervention.

## How it works

When a GitHub issue is labeled with the designated label (default: `opendev-git`) or a comment mentions `@opendev-git`, the system runs a three-phase workflow:

1. **Investigation** — The issue is sent to an external AI agent service, which uses [MCP (Model Context Protocol)](https://modelcontextprotocol.io/) tools to read the codebase and produce a structured findings report with proposed tasks and risks.
2. **Planning** — The agent reviews its investigation report and confirms it has enough confidence to proceed. If not, it posts clarifying questions and marks the issue `status:blocked`.
3. **Execution** — A Git branch is created, the agent writes code via the write MCP endpoint, tests are run (up to 3 retries per task on failure), the branch is pushed, and a pull request is opened automatically.

Status labels are managed throughout:

```
status:investigating → status:planned → status:approved → status:in-progress → status:done
                                                                              ↘ status:blocked
```

The system also exposes a REST API endpoint (`POST /api/v1/agents/{name}/run`) for manually invoking the agent with a message and optional MCP server configurations.

## Architecture

```
opendev-git/
├── cmd/opendev-git/        # Application entry point
├── internal/
│   ├── agent/              # HTTP client for the external AI agent service
│   ├── agentapi/           # REST API handler for manual agent invocation
│   ├── codemcp/            # HTTP client for the code-mcp service
│   ├── config/             # Environment variable configuration
│   ├── github/             # GitHub App API client
│   ├── mcpclient/          # MCP server config pool
│   ├── orchestrator/       # Core workflow (investigation, planning, execution)
│   ├── tools/              # Local filesystem and shell tool implementations
│   └── webhook/            # GitHub webhook receiver and router
```

### External service dependencies

opendev-git requires two external services at runtime:

- **Agent service** — An AI agent backend that receives natural language context and returns structured responses. Called with a `phase` (`investigation`, `planning`, or `execution`) and an array of MCP server configurations.
- **code-mcp service** — A codebase MCP server that manages Git repo clones and worktrees, runs tests, and exposes read/write HTTP MCP endpoints for the agent to use.

## Prerequisites

- Go 1.25+
- A **GitHub App** with:
  - An App ID and a private key (`.pem` file)
  - A webhook secret
  - Webhook events enabled for `issues` and `issue_comment`
  - Installed on the target repository with read/write permissions for issues, pull requests, and contents
- A running **agent service**
- A running **code-mcp service**

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `PORT` | No | `8080` | HTTP listen port |
| `GITHUB_APP_ID` | Yes | — | GitHub App numeric ID |
| `GITHUB_PRIVATE_KEY` | Yes | — | PEM content of the GitHub App private key |
| `GITHUB_WEBHOOK_SECRET` | Yes | — | HMAC-SHA256 secret for verifying webhook payloads. If empty, signature verification is bypassed. |
| `AGENT_SERVICE_URL` | Yes | — | Base URL of the AI agent service |
| `CODE_MCP_URL` | Yes | — | Base URL of the code-mcp service |
| `DESIGNATED_LABEL` | No | `opendev-git` | Issue label that triggers the workflow |
| `REPO_OWNER` | No | (from webhook) | GitHub repository owner; falls back to the webhook payload value |
| `REPO_NAME` | No | (from webhook) | GitHub repository name; falls back to the webhook payload value |
| `WORKSPACE_DIR` | No | `/workspace` | Local workspace root for tool operations |
| `TOOL_BUDGET` | No | `20` | Maximum agent loop iterations per investigation phase |

## Running

Copy `start.sh` and fill in your values:

```bash
export PORT="8080"
export GITHUB_APP_ID="123456"
export GITHUB_PRIVATE_KEY="$(cat /path/to/private-key.pem)"
export GITHUB_WEBHOOK_SECRET="your-webhook-secret"
export AGENT_SERVICE_URL="http://localhost:9090"
export CODE_MCP_URL="http://localhost:8081"
export DESIGNATED_LABEL="opendev-git"
export TOOL_BUDGET="20"

go run ./cmd/opendev-git
```

Or build a binary first:

```bash
go build -o opendev-git ./cmd/opendev-git
./opendev-git
```

Point your GitHub App's webhook URL at `http://<host>:<PORT>/webhook`.

A health check endpoint is available at `GET /healthz`.

## API

### Manual agent invocation

```
POST /api/v1/agents/{name}/run
```

**Request body:**
```json
{
  "message": "Explain the webhook handler",
  "mcp_servers": [
    {
      "name": "my-repo",
      "url": "http://localhost:8081/mcp/read/my-repo/main",
      "transport": "streamable-http"
    }
  ]
}
```

**Response:**
```json
{
  "agent": "my-agent-name",
  "response": "The webhook handler lives in internal/webhook/handler.go ..."
}
```

## Development

### Running tests

```bash
go test ./...
```

Tests use only the Go standard library (`testing`, `net/http/httptest`) with no external frameworks.

### Test coverage

| Package | Tests |
|---|---|
| `internal/webhook` | HMAC signature verification, HTTP method enforcement, payload parsing |
| `internal/orchestrator` | Task parsing, comment formatting, PR body generation, investigation response parsing |
| `internal/agentapi` | Request validation, round-trip forwarding, error propagation |
| `internal/tools` | Path traversal protection, file reading, directory listing, command execution |
