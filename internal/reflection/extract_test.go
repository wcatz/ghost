package reflection

import (
	"encoding/json"
	"testing"
)

func TestValidCategory(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// All 8 valid categories pass through unchanged.
		{"architecture", "architecture", "architecture"},
		{"decision", "decision", "decision"},
		{"pattern", "pattern", "pattern"},
		{"convention", "convention", "convention"},
		{"gotcha", "gotcha", "gotcha"},
		{"dependency", "dependency", "dependency"},
		{"preference", "preference", "preference"},
		{"fact", "fact", "fact"},

		// Case/whitespace normalization.
		{"uppercase", "FACT", "fact"},
		{"mixed_case", "Pattern", "pattern"},
		{"leading_space", "  decision", "decision"},
		{"trailing_space", "gotcha  ", "gotcha"},

		// Mapped categories.
		{"observation_to_fact", "observation", "fact"},
		{"bug_to_gotcha", "bug", "gotcha"},
		{"approach_to_pattern", "approach", "pattern"},
		{"note_to_fact", "note", "fact"},
		{"config_to_convention", "config", "convention"},
		{"library_to_dependency", "library", "dependency"},
		{"design_to_architecture", "design", "architecture"},
		{"choice_to_decision", "choice", "decision"},
		{"workflow_to_pattern", "workflow", "pattern"},
		{"pref_to_preference", "pref", "preference"},

		// Unknown falls back to "fact".
		{"unknown_category", "nonsense", "fact"},
		{"learning_to_fact", "learning", "fact"},
		{"insight_to_fact", "insight", "fact"},
		{"rule_to_fact", "rule", "fact"},
		{"empty_string", "", "fact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validCategory(tt.input)
			if got != tt.want {
				t.Errorf("validCategory(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseExtractionResponse(t *testing.T) {
	t.Run("valid_json_array", func(t *testing.T) {
		input := `[
			{"category":"fact","content":"Go uses goroutines","importance":0.8,"tags":["go"]},
			{"category":"pattern","content":"table-driven tests","importance":0.9,"tags":["testing"]}
		]`
		memories := parseExtractionResponse(input)
		if len(memories) != 2 {
			t.Fatalf("expected 2 memories, got %d", len(memories))
		}
		if memories[0].Content != "Go uses goroutines" {
			t.Errorf("memories[0].Content = %q, want %q", memories[0].Content, "Go uses goroutines")
		}
		if memories[1].Category != "pattern" {
			t.Errorf("memories[1].Category = %q, want %q", memories[1].Category, "pattern")
		}
	})

	t.Run("markdown_fenced_json", func(t *testing.T) {
		input := "```json\n" + `[{"category":"fact","content":"fenced","importance":0.5,"tags":[]}]` + "\n```"
		memories := parseExtractionResponse(input)
		if len(memories) != 1 {
			t.Fatalf("expected 1 memory, got %d", len(memories))
		}
		if memories[0].Content != "fenced" {
			t.Errorf("Content = %q, want %q", memories[0].Content, "fenced")
		}
	})

	t.Run("malformed_json", func(t *testing.T) {
		memories := parseExtractionResponse("this is not json at all")
		if memories != nil {
			t.Errorf("expected nil for malformed JSON, got %v", memories)
		}
	})

	t.Run("empty_array", func(t *testing.T) {
		memories := parseExtractionResponse("[]")
		if len(memories) != 0 {
			t.Errorf("expected 0 memories, got %d", len(memories))
		}
	})
}

func TestParseReflectionResponse(t *testing.T) {
	t.Run("valid_json", func(t *testing.T) {
		input := `{
			"learned_context": "project uses Go",
			"memories": [
				{"category":"fact","content":"uses SQLite","importance":0.8,"tags":["db"]}
			]
		}`
		result := parseReflectionResponse(input)
		if result.LearnedContext != "project uses Go" {
			t.Errorf("LearnedContext = %q, want %q", result.LearnedContext, "project uses Go")
		}
		if len(result.Memories) != 1 {
			t.Fatalf("expected 1 memory, got %d", len(result.Memories))
		}
		if result.Memories[0].Content != "uses SQLite" {
			t.Errorf("Content = %q, want %q", result.Memories[0].Content, "uses SQLite")
		}
	})

	t.Run("markdown_fenced_json", func(t *testing.T) {
		input := "```json\n" + `{"learned_context":"fenced","memories":[]}` + "\n```"
		result := parseReflectionResponse(input)
		if result.LearnedContext != "fenced" {
			t.Errorf("LearnedContext = %q, want %q", result.LearnedContext, "fenced")
		}
	})

	t.Run("malformed_json_fallback", func(t *testing.T) {
		raw := "some free-form reflection text"
		result := parseReflectionResponse(raw)
		if result.LearnedContext != raw {
			t.Errorf("LearnedContext = %q, want raw text %q", result.LearnedContext, raw)
		}
		if len(result.Memories) != 0 {
			t.Errorf("expected 0 memories for fallback, got %d", len(result.Memories))
		}
	})
}

func TestParseReflectionResponse_ImportanceClamp(t *testing.T) {
	tests := []struct {
		name       string
		importance float32
		want       float32
	}{
		{"negative_clamped_to_zero", -0.5, 0},
		{"above_one_clamped_to_one", 1.5, 1},
		{"valid_unchanged", 0.7, 0.7},
		{"zero_unchanged", 0, 0},
		{"one_unchanged", 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := mustJSONStr(ReflectionResult{
				LearnedContext: "ctx",
				Memories: []ReflectMemory{
					{Category: "fact", Content: "test", Importance: tt.importance, Tags: []string{}},
				},
			})
			result := parseReflectionResponse(input)
			if len(result.Memories) != 1 {
				t.Fatalf("expected 1 memory, got %d", len(result.Memories))
			}
			got := result.Memories[0].Importance
			if got != tt.want {
				t.Errorf("Importance = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseReflectionResponse_NilTags(t *testing.T) {
	// JSON with null tags should be initialized to empty slice.
	input := `{
		"learned_context": "ctx",
		"memories": [
			{"category":"fact","content":"no tags","importance":0.5,"tags":null},
			{"category":"fact","content":"missing tags","importance":0.5}
		]
	}`
	result := parseReflectionResponse(input)
	if len(result.Memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(result.Memories))
	}
	for i, m := range result.Memories {
		if m.Tags == nil {
			t.Errorf("Memories[%d].Tags is nil, want empty slice", i)
		}
	}
}

// --- helpers ---

func mustJSONStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
