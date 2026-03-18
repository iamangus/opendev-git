package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	t.Run("relative path inside workspace", func(t *testing.T) {
		tools := New("/workspace")
		got := tools.resolvePath("foo/bar.go")
		want := "/workspace/foo/bar.go"
		if got != want {
			t.Errorf("resolvePath(%q) = %q, want %q", "foo/bar.go", got, want)
		}
	})

	t.Run("path traversal is rejected", func(t *testing.T) {
		tools := New("/workspace")
		got := tools.resolvePath("../etc/passwd")
		// Should fall back to workspace root, not escape it.
		if strings.Contains(got, "etc/passwd") {
			t.Errorf("path traversal not rejected: got %q", got)
		}
		if !strings.HasPrefix(got, "/workspace") {
			t.Errorf("resolved path outside workspace: %q", got)
		}
	})

	t.Run("workspace root itself is allowed", func(t *testing.T) {
		tools := New("/workspace")
		got := tools.resolvePath(".")
		if got != "/workspace" {
			t.Errorf("resolvePath('.') = %q, want '/workspace'", got)
		}
	})
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nline3\nline4\nline5\n"
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tools := New(dir)

	t.Run("full file", func(t *testing.T) {
		got := tools.readFile("test.txt", 0, 0)
		if got != content {
			t.Errorf("readFile full = %q, want %q", got, content)
		}
	})

	t.Run("line range", func(t *testing.T) {
		got := tools.readFile("test.txt", 2, 3)
		want := "line2\nline3"
		if got != want {
			t.Errorf("readFile(2,3) = %q, want %q", got, want)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		got := tools.readFile("nonexistent.txt", 0, 0)
		if !strings.HasPrefix(got, "error opening file") {
			t.Errorf("missing file should return error, got %q", got)
		}
	})
}

func TestListDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.go"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	tools := New(dir)
	got := tools.listDirectory(".")
	if !strings.Contains(got, "file.go") {
		t.Errorf("listDirectory missing file.go: %q", got)
	}
	if !strings.Contains(got, "subdir/") {
		t.Errorf("listDirectory missing subdir/: %q", got)
	}
}

func TestRunCommand(t *testing.T) {
	dir := t.TempDir()
	tools := New(dir)

	t.Run("successful command", func(t *testing.T) {
		got := tools.runCommand("echo hello")
		if strings.TrimSpace(got) != "hello" {
			t.Errorf("runCommand echo = %q, want 'hello'", got)
		}
	})

	t.Run("failed command", func(t *testing.T) {
		got := tools.runCommand("exit 1")
		if !strings.HasPrefix(got, "command error") {
			t.Errorf("failing command should return error prefix, got %q", got)
		}
	})
}

func TestExecute(t *testing.T) {
	dir := t.TempDir()
	tools := New(dir)
	ctx := context.Background()

	t.Run("unknown tool", func(t *testing.T) {
		result := tools.Execute(ctx, ToolCall{Name: "nonexistent"})
		if result.Error == "" {
			t.Error("expected error for unknown tool")
		}
	})

	t.Run("run_command missing arg", func(t *testing.T) {
		result := tools.Execute(ctx, ToolCall{Name: "run_command", Args: map[string]string{}})
		if result.Error == "" {
			t.Error("expected error when cmd arg missing")
		}
	})

	t.Run("list_directory defaults to workspace", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		result := tools.Execute(ctx, ToolCall{Name: "list_directory", Args: map[string]string{}})
		if result.Error != "" {
			t.Errorf("unexpected error: %s", result.Error)
		}
		if !strings.Contains(result.Output, "hello.txt") {
			t.Errorf("expected hello.txt in listing, got %q", result.Output)
		}
	})
}
