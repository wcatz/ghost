package reflection

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestDropForeignProjectMemories_DropsUnknownProject: a memory naming a known
// other project that never appears in the input corpus is contaminated —
// consolidation never saw that project's data.
func TestDropForeignProjectMemories_DropsUnknownProject(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))

	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "ghost search uses hybrid RRF fusion", Importance: 0.8},
			{Category: "gotcha", Content: "dingo block producers need KES rotation", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo", "infra"},
		ExistingMemories: []memory.Memory{
			{Content: "ghost ranking has two paths"},
		},
	}

	dropForeignProjectMemories(result, input, lg)

	if len(result.Memories) != 1 {
		t.Fatalf("expected 1 survivor, got %d: %+v", len(result.Memories), result.Memories)
	}
	if !strings.Contains(result.Memories[0].Content, "hybrid RRF") {
		t.Errorf("unrelated memory must survive; got %q", result.Memories[0].Content)
	}
	out := buf.String()
	if !strings.Contains(out, "dingo") || !strings.Contains(out, "absent from the input") {
		t.Fatalf("drop was not surfaced to the log: %q", out)
	}
}

// TestDropForeignProjectMemories_AllowsTraceableNames: a foreign name that
// already appears in the input corpus is a legitimate cross-reference, not
// contamination.
func TestDropForeignProjectMemories_AllowsTraceableNames(t *testing.T) {
	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "dingo pairs with ghost for linking", Importance: 0.7},
		},
	}
	input := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{Content: "prior note already mentioned dingo integration"},
		},
	}

	dropForeignProjectMemories(result, input, slog.Default())

	if len(result.Memories) != 1 {
		t.Fatalf("traceable cross-reference must survive, got %d memories", len(result.Memories))
	}
}
