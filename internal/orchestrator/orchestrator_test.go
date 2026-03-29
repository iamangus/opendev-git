package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseTasks(t *testing.T) {
	comment := `## Investigation Complete

### Findings
Found some code.

### Proposed Tasks
- [ ] Add logging
- [ ] Fix the nil pointer dereference
- [x] Already done task

### Risks
None`

	tasks := parseTasks(comment)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %v", len(tasks), tasks)
	}
	if tasks[0] != "Add logging" {
		t.Errorf("tasks[0] = %q, want %q", tasks[0], "Add logging")
	}
	if tasks[1] != "Fix the nil pointer dereference" {
		t.Errorf("tasks[1] = %q, want %q", tasks[1], "Fix the nil pointer dereference")
	}
}

func TestParseTasksEmpty(t *testing.T) {
	tasks := parseTasks("No tasks here")
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestBuildInvestigationComment(t *testing.T) {
	comment := buildInvestigationComment("Found X", "Risk Y")
	if !strings.Contains(comment, "## Investigation Complete") {
		t.Error("missing ## Investigation Complete")
	}
	if !strings.Contains(comment, "Found X") {
		t.Error("missing findings")
	}
	if !strings.Contains(comment, "Risk Y") {
		t.Error("missing risks")
	}
}

func TestBuildInvestigationCommentDefaults(t *testing.T) {
	comment := buildInvestigationComment("", "")
	if !strings.Contains(comment, "## Investigation Complete") {
		t.Error("missing ## Investigation Complete")
	}
	if !strings.Contains(comment, "(none)") {
		t.Error("expected default findings placeholder")
	}
	if !strings.Contains(comment, "None identified") {
		t.Error("expected default risks placeholder")
	}
}

func TestInvestigationResponseJSON(t *testing.T) {
	jsonStr := `{
		"findings": "The codebase uses X pattern",
		"risks": "May break Z"
	}`

	var resp investigationResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if resp.Findings != "The codebase uses X pattern" {
		t.Errorf("findings = %q, want 'The codebase uses X pattern'", resp.Findings)
	}
	if resp.Risks != "May break Z" {
		t.Errorf("risks = %q, want 'May break Z'", resp.Risks)
	}
}

func TestPlanningResponseJSON(t *testing.T) {
	jsonStr := `{
		"tasks": ["Refactor module X", "Add integration tests"],
		"summary": "Two-step plan to improve reliability"
	}`

	var resp planningResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(resp.Tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(resp.Tasks))
	}
	if resp.Tasks[0] != "Refactor module X" {
		t.Errorf("tasks[0] = %q, want 'Refactor module X'", resp.Tasks[0])
	}
	if resp.Summary != "Two-step plan to improve reliability" {
		t.Errorf("summary = %q, want 'Two-step plan to improve reliability'", resp.Summary)
	}
}

func TestExecutionResponseJSON(t *testing.T) {
	jsonStr := `{
		"done": true,
		"files_created": ["main.go", "main_test.go"],
		"files_modified": ["config.go"],
		"summary": "Implemented the feature"
	}`

	var resp executionResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !resp.Done {
		t.Error("expected done to be true")
	}
	if len(resp.FilesCreated) != 2 {
		t.Errorf("expected 2 files created, got %d", len(resp.FilesCreated))
	}
	if len(resp.FilesModified) != 1 {
		t.Errorf("expected 1 file modified, got %d", len(resp.FilesModified))
	}
	if resp.Summary != "Implemented the feature" {
		t.Errorf("summary = %q, want 'Implemented the feature'", resp.Summary)
	}
}

func TestBuildPRBody(t *testing.T) {
	body := buildPRBody(42, []string{"Add logging", "Fix bug"})
	if !strings.Contains(body, "Closes #42") {
		t.Error("missing Closes #42")
	}
	if !strings.Contains(body, "Add logging") {
		t.Error("missing task 'Add logging'")
	}
	if !strings.Contains(body, "## Summary") {
		t.Error("missing ## Summary")
	}
	if !strings.Contains(body, "## Tasks Completed") {
		t.Error("missing ## Tasks Completed")
	}
}

func TestFindInvestigationComment(t *testing.T) {
	from := `## Investigation Complete

### Findings
Found something

### Proposed Tasks
- [ ] Fix it

### Risks
None`
	tasks := parseTasks(from)
	if len(tasks) != 1 || tasks[0] != "Fix it" {
		t.Errorf("unexpected tasks from investigation comment: %v", tasks)
	}
}
