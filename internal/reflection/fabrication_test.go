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

// TestBuildReflectionPrompt_AsksForPerIdOperations is the prompt half of #639.
// The output contract only fixes identity churn if the model is told to name
// the id it is acting on: a harness that answers in free text resets every
// memory's id, and with it the row's embedding, links and age. The prompt has
// to state the four operations, and the verbatim one has to be the default for
// a memory that is already right.
func TestBuildReflectionPrompt_AsksForPerIdOperations(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName: "ghost",
		ExistingMemories: []memory.Memory{
			{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "gotcha", Content: "port 2222 not 22"},
		},
	})
	for _, want := range []string{
		`"ops"`,
		"keep <id>",
		"merge <id>",
		"rewrite <id>",
		"drop <id> reason:",
		"superseded by <id>",
		"obsolete",
		"rejected and the original memories are kept",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The id is the whole contract: without it in the input list the model
	// cannot name what it is acting on.
	if !strings.Contains(prompt, "D20E133860CC4AFE38B485AD5371BA59") {
		t.Error("prompt does not render the input memory's id")
	}
	// The retired free-text contract must not survive in the prompt.
	for _, unwanted := range []string{`"memories"`, "COMPLETE consolidated memory set"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("prompt still asks for the free-text contract: %q", unwanted)
		}
	}
}

// TestBuildReflectionPrompt_RewriteCannotChangeASpecific keeps the prompt from
// asking for something the grounding check always refuses. It used to describe a
// rewrite as correcting "a wrong number, a wrong name, a wrong host" while the
// rule two lines below rejected any identifier absent from the source — and the
// corrected value is by definition absent from it, so every such rewrite was
// discarded. The prompt now says plainly that a rewrite fixes a claim, never a
// specific, and why.
func TestBuildReflectionPrompt_RewriteCannotChangeASpecific(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName: "ghost",
		ExistingMemories: []memory.Memory{
			{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "gotcha", Content: "port 2222 not 22"},
		},
	})
	for _, want := range []string{
		"replace ONE memory whose CLAIM is wrong",
		"must not change a specific",
		"a rewrite cannot fix a wrong number",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"a wrong number, a wrong name, a wrong host",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("prompt still asks for a rewrite the grounding check rejects: %q", unwanted)
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
		ExistingMemories: []memory.Memory{{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "decision", Content: "chose X"}},
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
// --allow-drops an input no surviving memory explains is DELETED, so the
// retention sentences would tell the harness that unexplained output is free
// exactly where it costs a memory — and eval/cycle runs every reflect with that
// flag, so the eval harness would measure a lazier consolidator than production
// uses. Each branch also asserts the OTHER branch's sentence is absent, since a
// prompt containing both contradicts itself.
//
// What the two branches are about changed with the per-id pass-through: an
// unnamed id is carried through by the tier either way, so --allow-drops is now
// about the DROPS, not the omissions, and saying otherwise would be a promise the
// apply cannot keep.
func TestBuildReflectionPrompt_AllowDropsInvertsTheContract(t *testing.T) {
	memories := []memory.Memory{{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "gotcha", Content: "port 2222 not 22"}}

	retained := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: memories})
	dropping := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: memories, AllowDrops: true})

	for _, want := range []string{
		"Dropping is not deletion",
		"kept verbatim",
		"undone by that re-add",
		"a memory you do not name is carried through unchanged",
		"puts that row back verbatim",
		"State an obsolete drop only when a surviving memory really does replace it",
	} {
		if !strings.Contains(retained, want) {
			t.Errorf("retention prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"DELETES every input",
		"a drop nothing explains is a real deletion",
		"There is no protected category",
		"the input is DELETED, and yours is the last version of it",
	} {
		if strings.Contains(retained, unwanted) {
			t.Errorf("retention prompt leaks the --allow-drops wording %q", unwanted)
		}
	}

	for _, want := range []string{
		"DELETES every input",
		"There is no protected category",
		"a drop nothing explains is a real deletion",
		"a real deletion",
		"the input is deleted even though you mentioned it",
		// The two operations that REPLACE a row carry the same warning as a
		// merge, and under --allow-drops the re-add never happens — the row is
		// simply deleted. This is the path eval/cycle measures on, so a prompt
		// promising a re-add there is telling the grader a consolidation is
		// cheaper than it is (#549).
		"the input is DELETED, and yours is the last version of it",
	} {
		if !strings.Contains(dropping, want) {
			t.Errorf("--allow-drops prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"Dropping is not deletion",
		"EVERY category is protected",
		"undone by that re-add",
		"puts that row back verbatim",
		// The mode-dependent tail spliced into the obsolete bullet, into the
		// "drop stale situational memories" rule and into #674's "Repository
		// facts" rule is staleTail, so this is the wording a leaked DEFAULT
		// tail would carry. obsoleteTail's own text
		// cannot do this job in either direction: it is byte-identical in both
		// modes, so it is present whether or not a tail leaked — its presence
		// detects nothing, and its absence would say nothing about the mode
		// either. (It IS in the prompt, asserted in the retention branch above,
		// which is the other half of why a literal drawn from it is a no-op here.)
		"since a drop nothing explains is undone by the verbatim re-add",
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
