package agent

import (
	"testing"
)

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "clean JSON",
			input:   `{"findings": "all good", "risks": "none"}`,
			wantErr: false,
		},
		{
			name:    "preamble text before JSON",
			input:   `Great! I've completed the investigation. Here are my findings: {"findings": "found a bug", "risks": "low risk"}`,
			wantErr: false,
		},
		{
			name:    "markdown code fence with json",
			input:   "Here is the plan:\n```json\n{\"tasks\": [\"do thing\"], \"summary\": \"plan\"}\n```\nLet me know!",
			wantErr: false,
		},
		{
			name:    "markdown code fence without json tag",
			input:   "Results:\n```\n{\"findings\": \"x\", \"risks\": \"y\"}\n```",
			wantErr: false,
		},
		{
			name:    "preamble with nested JSON",
			input:   `Sure thing! Here it is: {"done": true, "files_created": ["a.go", "b.go"], "files_modified": [], "summary": "created files"}`,
			wantErr: false,
		},
		{
			name:    "code fence with preamble",
			input:   "Awesome! Here's my response:\n```json\n{\"done\": false, \"files_created\": [], \"files_modified\": [\"main.go\"], \"summary\": \"wip\"}\n```",
			wantErr: false,
		},
		{
			name:    "no JSON at all",
			input:   "I don't have any JSON for you",
			wantErr: true,
		},
		{
			name:    "unbalanced braces",
			input:   `Here: {"key": "value"`,
			wantErr: true,
		},
		{
			name:    "JSON with escaped quotes in strings",
			input:   `Result: {"findings": "the \"problem\" is here", "risks": "none"}`,
			wantErr: false,
		},
		{
			name:    "JSON with nested braces",
			input:   `Output: {"a": {"b": {"c": 1}}, "d": [1, 2, 3]}`,
			wantErr: false,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := extractJSON(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("extractJSON(%q) expected error, got nil (data=%s)", tt.name, data)
				}
				return
			}
			if err != nil {
				t.Errorf("extractJSON(%q) unexpected error: %v", tt.name, err)
				return
			}
			if data == nil {
				t.Errorf("extractJSON(%q) returned nil data without error", tt.name)
			}
		})
	}
}

func TestResponseUnmarshal(t *testing.T) {
	type testStruct struct {
		Findings string `json:"findings"`
		Risks    string `json:"risks"`
	}

	tests := []struct {
		name    string
		text    string
		want    testStruct
		wantErr bool
	}{
		{
			name:    "clean JSON",
			text:    `{"findings": "found it", "risks": "low"}`,
			want:    testStruct{Findings: "found it", Risks: "low"},
			wantErr: false,
		},
		{
			name:    "preamble before JSON",
			text:    `Great! Here is the result: {"findings": "done", "risks": "none"}`,
			want:    testStruct{Findings: "done", Risks: "none"},
			wantErr: false,
		},
		{
			name:    "code fence",
			text:    "```json\n{\"findings\": \"x\", \"risks\": \"y\"}\n```",
			want:    testStruct{Findings: "x", Risks: "y"},
			wantErr: false,
		},
		{
			name:    "no JSON",
			text:    "just plain text",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Response{Text: tt.text}
			var got testStruct
			err := r.Unmarshal(&got)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
