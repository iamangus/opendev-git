package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ToolCall represents a request to execute a tool.
type ToolCall struct {
	Name string
	Args map[string]string
}

// ToolResult holds the output of a tool execution.
type ToolResult struct {
	Name   string
	Output string
	Error  string
}

// Tools provides filesystem and shell operations scoped to a workspace directory.
type Tools struct {
	workspaceDir string
}

// New creates a Tools instance rooted at workspaceDir.
func New(workspaceDir string) *Tools {
	return &Tools{workspaceDir: workspaceDir}
}

// Execute dispatches a ToolCall to the appropriate implementation.
func (t *Tools) Execute(ctx context.Context, call ToolCall) ToolResult {
	result := ToolResult{Name: call.Name}

	switch call.Name {
	case "search_code":
		query := call.Args["query"]
		if query == "" {
			result.Error = "search_code requires 'query' argument"
			return result
		}
		result.Output = t.searchCode(query)

	case "read_file":
		path := call.Args["path"]
		if path == "" {
			result.Error = "read_file requires 'path' argument"
			return result
		}
		start, _ := strconv.Atoi(call.Args["start"])
		end, _ := strconv.Atoi(call.Args["end"])
		result.Output = t.readFile(path, start, end)

	case "list_directory":
		path := call.Args["path"]
		if path == "" {
			path = "."
		}
		result.Output = t.listDirectory(path)

	case "read_git_log":
		path := call.Args["path"]
		n, _ := strconv.Atoi(call.Args["n"])
		if n == 0 {
			n = 20
		}
		result.Output = t.readGitLog(path, n)

	case "run_command":
		cmd := call.Args["cmd"]
		if cmd == "" {
			result.Error = "run_command requires 'cmd' argument"
			return result
		}
		result.Output = t.runCommand(cmd)

	default:
		result.Error = fmt.Sprintf("unknown tool: %s", call.Name)
	}

	return result
}

// searchCode runs ripgrep (or grep as fallback) in the workspace directory.
func (t *Tools) searchCode(query string) string {
	dir := t.workspaceDir

	// Prefer ripgrep, fall back to grep.
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		rgPath = ""
	}

	var cmd *exec.Cmd
	if rgPath != "" {
		cmd = exec.Command("rg", "--line-number", "--with-filename", "-e", query, dir)
	} else {
		cmd = exec.Command("grep", "-rn", "--include=*", query, dir)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 1 from grep/rg just means no matches — not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "(no matches found)"
		}
		return fmt.Sprintf("search error: %v\n%s", err, string(out))
	}
	if len(out) == 0 {
		return "(no matches found)"
	}
	return string(out)
}

// readFile reads a file relative to the workspace. start/end are 1-based line numbers (0 = no limit).
func (t *Tools) readFile(path string, start, end int) string {
	fullPath := t.resolvePath(path)
	f, err := os.Open(fullPath)
	if err != nil {
		return fmt.Sprintf("error opening file: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err)
	}

	if start == 0 && end == 0 {
		return string(data)
	}

	lines := strings.Split(string(data), "\n")
	lo := start - 1
	if lo < 0 {
		lo = 0
	}
	hi := end
	if hi == 0 || hi > len(lines) {
		hi = len(lines)
	}
	if lo >= len(lines) {
		return "(line range out of bounds)"
	}
	return strings.Join(lines[lo:hi], "\n")
}

// listDirectory lists entries in a directory relative to the workspace.
func (t *Tools) listDirectory(path string) string {
	fullPath := t.resolvePath(path)
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return fmt.Sprintf("error listing directory: %v", err)
	}

	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			sb.WriteString(e.Name() + "/\n")
		} else {
			sb.WriteString(e.Name() + "\n")
		}
	}
	return sb.String()
}

// readGitLog returns the last n git log entries for the workspace (or a subpath).
func (t *Tools) readGitLog(path string, n int) string {
	args := []string{"log", fmt.Sprintf("-n%d", n), "--oneline"}
	if path != "" {
		args = append(args, "--", path)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = t.workspaceDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git log error: %v\n%s", err, string(out))
	}
	if len(out) == 0 {
		return "(no commits found)"
	}
	return string(out)
}

// runCommand executes a shell command inside the workspace directory.
// Commands are run via /bin/sh -c to support pipes and redirection.
func (t *Tools) runCommand(cmd string) string {
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = t.workspaceDir

	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf

	if err := c.Run(); err != nil {
		return fmt.Sprintf("command error (%v):\n%s", err, buf.String())
	}
	return buf.String()
}

// resolvePath makes path absolute, rooted at workspaceDir.
// It rejects paths that escape the workspace directory.
func (t *Tools) resolvePath(path string) string {
	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Clean(filepath.Join(t.workspaceDir, path))
	}

	// Ensure the resolved path is inside the workspace.
	workspace := filepath.Clean(t.workspaceDir)
	if !strings.HasPrefix(abs, workspace+string(filepath.Separator)) && abs != workspace {
		// Path escapes workspace; return workspace root as a safe fallback.
		return workspace
	}
	return abs
}
