package reflection

import (
	"bytes"
	"log/slog"
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
	dropFabricatedMemories(result, input, nil)
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
	dropFabricatedMemories(result, input, nil)
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
	dropFabricatedMemories(result, input, nil)
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
	dropFabricatedMemories(result, input, nil)
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

// TestBuildReflectionPrompt_ProtectsEveryCategory pins the prompt half of the
// drop guard. AuditGuardedDrops retained every category from #549, so a prompt
// still naming only gotcha/dependency/preference/convention would leave the
// model compressing away memories the apply then re-adds verbatim beside its own
// rewrites. It also has to say that omitting is not deletion, or "drop stale
// ones" reads as permission to lose them.
func TestBuildReflectionPrompt_ProtectsEveryCategory(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName:      "ghost",
		ExistingMemories: []memory.Memory{{Category: "decision", Content: "chose X"}},
	})
	for _, want := range []string{
		"EVERY category is protected",
		"architecture, decision, pattern",
		"Dropping is not deletion",
		"kept verbatim",
		"restate the specifics",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The old rule's own wording is what narrows the guard to four categories; if
	// it came back, the assertions above could still pass on the same sentence.
	if strings.Contains(prompt, "whose category is gotcha") {
		t.Error("prompt still protects only four categories by name")
	}
}

// TestBuildReflectionPrompt_AllowDropsInvertsTheContract: the guarantee is
// two-sided, and stating the wrong side is worse than saying nothing. Under
// --allow-drops an unreferenced input is DELETED, so the retention sentences
// would tell the model omission is free exactly where it costs a memory — and
// eval/cycle runs every reflect with that flag, so the eval harness would
// measure a lazier consolidator than production uses. Each branch also asserts
// the OTHER branch's sentence is absent, since a prompt containing both
// contradicts itself.
func TestBuildReflectionPrompt_AllowDropsInvertsTheContract(t *testing.T) {
	memories := []memory.Memory{{Category: "gotcha", Content: "port 2222 not 22"}}

	retained := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: memories})
	dropping := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: memories, AllowDrops: true})

	for _, want := range []string{
		"Dropping is not deletion",
		"kept verbatim",
		"does not remove it",
		"undone by the verbatim re-add",
	} {
		if !strings.Contains(retained, want) {
			t.Errorf("retention prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"DELETES any input",
		"omitting one deletes it",
		"a real deletion",
		"There is no protected category",
	} {
		if strings.Contains(retained, unwanted) {
			t.Errorf("retention prompt leaks the --allow-drops wording %q", unwanted)
		}
	}

	for _, want := range []string{
		"DELETES any input",
		"There is no protected category",
		"omitting one deletes it",
		"a real deletion",
		"the input is deleted even though you mentioned it",
	} {
		if !strings.Contains(dropping, want) {
			t.Errorf("--allow-drops prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"Dropping is not deletion",
		"EVERY category is protected",
		"kept verbatim",
		"undone by the verbatim re-add",
	} {
		if strings.Contains(dropping, unwanted) {
			t.Errorf("--allow-drops prompt still promises retention: %q", unwanted)
		}
	}
}

// TestDropFabricatedMemories_LogsDrop pins the surfacing fix: a dropped memory
// used to vanish with no trace, making a fabricated-output run look identical
// to a clean one. It passes an explicit logger, so it also pins that drops go
// to the caller's configured sink rather than the package-global default.
func TestDropFabricatedMemories_LogsDrop(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))

	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "gotcha", Content: "the fix landed in fdf4583", Importance: 0.7},
		},
	}
	dropFabricatedMemories(result, ReflectionInput{}, lg)

	if len(result.Memories) != 0 {
		t.Fatalf("expected the fabricated memory to be dropped, got %d", len(result.Memories))
	}
	out := buf.String()
	if !strings.Contains(out, "fdf4583") || !strings.Contains(out, "fabricated") {
		t.Fatalf("drop was not surfaced to the log: %q", out)
	}
}
