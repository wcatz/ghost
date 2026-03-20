package telegram

import (
	"encoding/json"
	"testing"

	gh "github.com/wcatz/ghost/internal/github"
)

func TestFormatToolInput(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    string
		want     string
	}{
		// bash: description + command (command truncated at 80 chars)
		{
			name:     "bash with description and short command",
			toolName: "bash",
			input:    `{"description":"List files","command":"ls -la"}`,
			want:     "List files\n$ ls -la",
		},
		{
			name:     "bash with description and long command truncated at 80",
			toolName: "bash",
			input:    `{"description":"Run build","command":"12345678901234567890123456789012345678901234567890123456789012345678901234567890_extra"}`,
			want:     "Run build\n$ 12345678901234567890123456789012345678901234567890123456789012345678901234567890...",
		},
		{
			name:     "bash with description only",
			toolName: "bash",
			input:    `{"description":"Install dependencies"}`,
			want:     "Install dependencies",
		},
		{
			name:     "bash with command only",
			toolName: "bash",
			input:    `{"command":"go test ./..."}`,
			want:     "go test ./...",
		},

		// file_write: shows path
		{
			name:     "file_write shows path",
			toolName: "file_write",
			input:    `{"path":"/home/user/main.go","content":"package main"}`,
			want:     "/home/user/main.go",
		},
		{
			name:     "file_write empty path falls through to JSON",
			toolName: "file_write",
			input:    `{"content":"hello"}`,
			want:     `{"content":"hello"}`,
		},

		// file_edit: shows path + old_string preview
		{
			name:     "file_edit shows path and old_string",
			toolName: "file_edit",
			input:    `{"path":"/home/user/main.go","old_string":"fmt.Println","new_string":"log.Println"}`,
			want:     "/home/user/main.go\n- fmt.Println",
		},
		{
			name:     "file_edit shows path only when no old_string",
			toolName: "file_edit",
			input:    `{"path":"/home/user/main.go","new_string":"log.Println"}`,
			want:     "/home/user/main.go",
		},
		{
			name:     "file_edit truncates long old_string at 60 chars",
			toolName: "file_edit",
			input:    `{"path":"x.go","old_string":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_extra_stuff"}`,
			want:     "x.go\n- aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa...",
		},

		// memory_save: shows content
		{
			name:     "memory_save shows content",
			toolName: "memory_save",
			input:    `{"content":"Ghost uses SQLite with FTS5"}`,
			want:     "Ghost uses SQLite with FTS5",
		},

		// memory_search: shows query
		{
			name:     "memory_search shows query",
			toolName: "memory_search",
			input:    `{"query":"architecture decisions"}`,
			want:     "architecture decisions",
		},

		// Unknown tool: compact JSON fallback
		{
			name:     "unknown tool shows compact JSON",
			toolName: "some_tool",
			input:    `{"key": "value", "num": 42}`,
			want:     `{"key":"value","num":42}`,
		},

		// Invalid JSON falls back to raw string
		{
			name:     "invalid JSON returns raw input",
			toolName: "bash",
			input:    `not json`,
			want:     "not json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatToolInput(tt.toolName, json.RawMessage(tt.input))
			if got != tt.want {
				t.Errorf("formatToolInput(%q, ...) =\n  %q\nwant:\n  %q", tt.toolName, got, tt.want)
			}
		})
	}
}

func TestFormatToolInput_JSONFallbackTruncation(t *testing.T) {
	// Build input with a very long value to trigger the 200-char truncation.
	longVal := make([]byte, 300)
	for i := range longVal {
		longVal[i] = 'x'
	}
	input := `{"data":"` + string(longVal) + `"}`
	got := formatToolInput("unknown_tool", json.RawMessage(input))

	if len(got) > 204 { // 200 + len("...")
		t.Errorf("formatToolInput fallback should truncate to ~203 chars, got %d", len(got))
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("formatToolInput fallback should end with '...', got suffix %q", got[len(got)-3:])
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{name: "short string unchanged", s: "hello", max: 10, want: "hello"},
		{name: "exact length unchanged", s: "hello", max: 5, want: "hello"},
		{name: "long string truncated with ellipsis", s: "hello world", max: 5, want: "hello\u2026"},
		{name: "empty string unchanged", s: "", max: 10, want: ""},
		{name: "max zero truncates", s: "hello", max: 0, want: "\u2026"},
		{name: "single char max", s: "hello", max: 1, want: "h\u2026"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

func TestPriorityEmoji(t *testing.T) {
	tests := []struct {
		name     string
		priority int
		want     string
	}{
		{name: "P0 is red circle", priority: gh.P0, want: "\U0001f534"},
		{name: "P1 is orange circle", priority: gh.P1, want: "\U0001f7e0"},
		{name: "P2 is yellow circle", priority: gh.P2, want: "\U0001f7e1"},
		{name: "P3 is blue circle", priority: gh.P3, want: "\U0001f535"},
		{name: "P4 is white circle", priority: gh.P4, want: "\u26aa"},
		{name: "unknown priority is white circle", priority: 99, want: "\u26aa"},
		{name: "negative priority is white circle", priority: -1, want: "\u26aa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := priorityEmoji(tt.priority)
			if got != tt.want {
				t.Errorf("priorityEmoji(%d) = %q, want %q", tt.priority, got, tt.want)
			}
		})
	}
}

func TestParseAllowedIDs(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want []int64
	}{
		{name: "empty string returns nil", s: "", want: nil},
		{name: "single ID", s: "12345", want: []int64{12345}},
		{name: "multiple IDs", s: "111,222,333", want: []int64{111, 222, 333}},
		{name: "IDs with spaces", s: " 111 , 222 , 333 ", want: []int64{111, 222, 333}},
		{name: "invalid entry skipped", s: "111,abc,333", want: []int64{111, 333}},
		{name: "all invalid returns empty slice", s: "abc,def", want: []int64{}},
		{name: "negative IDs parsed", s: "-100,200", want: []int64{-100, 200}},
		{name: "large IDs", s: "9876543210", want: []int64{9876543210}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseAllowedIDs(tt.s)

			// nil vs empty slice distinction.
			if tt.want == nil {
				if got != nil {
					t.Errorf("ParseAllowedIDs(%q) = %v, want nil", tt.s, got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("ParseAllowedIDs(%q) returned %d IDs, want %d\ngot:  %v\nwant: %v", tt.s, len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseAllowedIDs(%q)[%d] = %d, want %d", tt.s, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestGhAPIToHTML(t *testing.T) {
	tests := []struct {
		name        string
		apiURL      string
		subjectType string
		want        string
	}{
		{
			name:        "pull request URL converted",
			apiURL:      "https://api.github.com/repos/owner/repo/pulls/123",
			subjectType: "PullRequest",
			want:        "https://github.com/owner/repo/pull/123",
		},
		{
			name:        "issue URL converted",
			apiURL:      "https://api.github.com/repos/owner/repo/issues/456",
			subjectType: "Issue",
			want:        "https://github.com/owner/repo/issues/456",
		},
		{
			name:        "empty URL returns empty",
			apiURL:      "",
			subjectType: "PullRequest",
			want:        "",
		},
		{
			name:        "non-API URL returns empty",
			apiURL:      "https://github.com/owner/repo/pull/123",
			subjectType: "PullRequest",
			want:        "",
		},
		{
			name:        "commit URL converted",
			apiURL:      "https://api.github.com/repos/owner/repo/commits/abc123",
			subjectType: "Commit",
			want:        "https://github.com/owner/repo/commits/abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ghAPIToHTML(tt.apiURL, tt.subjectType)
			if got != tt.want {
				t.Errorf("ghAPIToHTML(%q, %q) = %q, want %q", tt.apiURL, tt.subjectType, got, tt.want)
			}
		})
	}
}
