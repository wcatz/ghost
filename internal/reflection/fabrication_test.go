package reflection

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSHALikeToken(t *testing.T) {
	tests := []struct {
		tok  string
		want bool
	}{
		{"fdf4583", true},
		{"0a1f004", true},
		{"d6b63b7", true},
		{"a1b2c3d4e5f6", true},
		{"defaced", false},   // pure letters: English word
		{"deadbeef", false},  // pure letters
		{"764824073", false}, // pure digits: a number, not a hash
		{"1234567", false},   // too short
		{"8080", false},      // too short
		{"not-a-sha", false}, // non-hex chars
		{"g1234567", false},  // g is not hex
	}
	for _, tt := range tests {
		if got := shaLikeToken(tt.tok); got != tt.want {
			t.Errorf("shaLikeToken(%q) = %v, want %v", tt.tok, got, tt.want)
		}
	}
}

func TestDropFabricatedMemories_DropsUnknownSHA(t *testing.T) {
	result := &ReflectionResult{
		LearnedContext: "ctx",
		Memories: []ReflectMemory{
			{Category: "fact", Content: "Graph expansion removed in 0a1f004", Importance: 0.7},
			{Category: "fact", Content: "Regression fixed in fdf4583", Importance: 0.7},
			{Category: "fact", Content: "Port moved from 22 to 2222", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "Graph expansion removed in 0a1f004"},
		},
	}
	dropFabricatedMemories(result, input)
	if len(result.Memories) != 2 {
		t.Fatalf("expected 2 surviving memories, got %d", len(result.Memories))
	}
	for _, m := range result.Memories {
		if strings.Contains(m.Content, "fdf4583") {
			t.Errorf("memory with untraceable SHA survived: %q", m.Content)
		}
	}
}

func TestDropFabricatedMemories_KeepsKnownSHA(t *testing.T) {
	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "Regression fixed in d6b63b7", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "Regression fixed in d6b63b7"},
		},
	}
	dropFabricatedMemories(result, input)
	if len(result.Memories) != 1 {
		t.Fatalf("expected traceable SHA to survive, got %d memories", len(result.Memories))
	}
}

func TestDropFabricatedMemories_SHAFromCommitsIsKnown(t *testing.T) {
	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "Shipped in 3329c5a", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		LastCommits: []string{"3329c5a fix(resolve): retire MCP sampling"},
	}
	dropFabricatedMemories(result, input)
	if len(result.Memories) != 1 {
		t.Fatalf("expected commit-derived SHA to survive, got %d memories", len(result.Memories))
	}
}

func TestDropFabricatedMemories_IgnoresNonSHATokens(t *testing.T) {
	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "The repo was defaced by a deadbeef constant, port 8080, network magic 764824073", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "old content"},
		},
	}
	dropFabricatedMemories(result, input)
	if len(result.Memories) != 1 {
		t.Fatalf("expected non-SHA tokens to survive, got %d memories", len(result.Memories))
	}
}

func TestBuildReflectionPrompt_ForbidsFabrication(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName: "ghost",
	})
	for _, want := range []string{
		"Anti-fabrication",
		"Project-scoping",
		"the ONLY source of truth",
		"not traceable to the input data",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
