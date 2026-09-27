package reflection

import (
	"bytes"
	"context"
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

// TestDropForeignProjectMemories_NoOtherNames: guard is off when the caller
// wires no other-project names (zero value / ListProjectNames failure).
func TestDropForeignProjectMemories_NoOtherNames(t *testing.T) {
	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "anything about dingo", Importance: 0.7},
		},
	}
	dropForeignProjectMemories(result, ReflectionInput{ProjectName: "ghost"}, slog.Default())
	if len(result.Memories) != 1 {
		t.Fatalf("guard must be a no-op without OtherProjectNames, got %d", len(result.Memories))
	}
}

// TestSupersessionLapsesWhenAFilterRemovesItsSuccessor drives the whole tier: the
// operations are executed, the contamination filter then removes a merged
// survivor for naming a project the input corpus never mentioned, and the
// supersession that pointed at it has to lapse with it. Honouring it would delete
// the dropped row with nothing replacing it — the exact outcome the "target must
// be carried forward" rule exists to prevent, reached through a filter that runs
// after the operations are resolved.
func TestSupersessionLapsesWhenAFilterRemovesItsSuccessor(t *testing.T) {
	const (
		stale = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		fresh = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		other = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
	)
	in := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{ID: stale, Category: "gotcha", Content: "the ledger is reached from the office subnet"},
			{ID: fresh, Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
			{ID: other, Category: "fact", Content: "the bastion answers ping on 443"},
		},
	}
	llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":["drop ` + stale + ` reason: superseded by ` + fresh + `","merge ` +
		fresh + `,` + other + ` -> Production in region fsn1 is fronted by Cloudflare and the dingo bastion answers ping on 443"]}`})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) != 0 {
		t.Fatalf("the contaminating merge survived the filter: %+v", result.Memories)
	}
	if len(result.Supersessions) != 1 {
		t.Fatalf("supersessions = %+v, want the one stated claim", result.Supersessions)
	}
	if drops := AuditGuardedDrops(in, result); !auditContains(drops, in.ExistingMemories[0].Content) {
		t.Fatalf("a supersession outlived the successor the filter removed, so the stale row would be deleted: %+v", drops)
	}
}

// auditContains reports whether the audit would re-add this exact content.
func auditContains(drops []DroppedGuarded, content string) bool {
	for _, d := range drops {
		if d.Content == content {
			return true
		}
	}
	return false
}

// TestDropForeignProjectMemories_WordBoundary: "go" must not match inside
// "going"/"digongo" (whole-token only), but must match a standalone token.
func TestDropForeignProjectMemories_WordBoundary(t *testing.T) {
	input := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"go"},
	}

	result := &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "gotcha", Content: "careful when going to production", Importance: 0.7},
			{Category: "fact", Content: "build with the digongo helper", Importance: 0.6},
		},
	}
	dropForeignProjectMemories(result, input, slog.Default())
	if len(result.Memories) != 2 {
		t.Fatalf("substring must not trigger the guard, got %d survivors: %+v", len(result.Memories), result.Memories)
	}

	result = &ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "toolchain is written in go", Importance: 0.7},
		},
	}
	dropForeignProjectMemories(result, input, slog.Default())
	if len(result.Memories) != 0 {
		t.Fatalf("standalone foreign token must be dropped, got %d", len(result.Memories))
	}
}
