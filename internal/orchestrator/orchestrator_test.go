package orchestrator

import (
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
	comment := buildInvestigationComment("Found X", "- [ ] Fix X", "Risk Y")
	if !strings.Contains(comment, "## Investigation Complete") {
		t.Error("missing ## Investigation Complete")
	}
	if !strings.Contains(comment, "Found X") {
		t.Error("missing findings")
	}
	if !strings.Contains(comment, "- [ ] Fix X") {
		t.Error("missing task")
	}
	if !strings.Contains(comment, "Risk Y") {
		t.Error("missing risks")
	}
}

func TestBuildInvestigationCommentDefaults(t *testing.T) {
	comment := buildInvestigationComment("", "", "")
	if !strings.Contains(comment, "## Investigation Complete") {
		t.Error("missing ## Investigation Complete")
	}
	// Defaults should be applied.
	if !strings.Contains(comment, "- [ ]") {
		t.Error("expected default task placeholder")
	}
}

func TestParseInvestigationResponse(t *testing.T) {
	text := `## Investigation Complete

### Findings
The codebase uses X pattern.

### Proposed Tasks
- [ ] Refactor Y
- [ ] Add tests

### Risks
May break Z`

	findings, tasks, risks := parseInvestigationResponse(text)
	if !strings.Contains(findings, "X pattern") {
		t.Errorf("findings = %q, want to contain 'X pattern'", findings)
	}
	if !strings.Contains(tasks, "Refactor Y") {
		t.Errorf("tasks = %q, want to contain 'Refactor Y'", tasks)
	}
	if !strings.Contains(risks, "May break Z") {
		t.Errorf("risks = %q, want to contain 'May break Z'", risks)
	}
}

func TestParseInvestigationResponseNoSections(t *testing.T) {
	text := "The issue is about X and should be fixed by doing Y."
	findings, tasks, risks := parseInvestigationResponse(text)
	if findings != text {
		t.Errorf("findings = %q, want %q", findings, text)
	}
	if tasks != "" {
		t.Errorf("tasks should be empty, got %q", tasks)
	}
	if risks != "" {
		t.Errorf("risks should be empty, got %q", risks)
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
	// Verify that parseTasks correctly extracts the task from an investigation comment body.
	tasks := parseTasks(from)
	if len(tasks) != 1 || tasks[0] != "Fix it" {
		t.Errorf("unexpected tasks from investigation comment: %v", tasks)
	}
}
