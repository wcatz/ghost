package memory

import (
	"context"
	"testing"
)

// retainedMemory is exactly what the drop-guard hands back to the replace:
// reflection.RetainGuardedDrops re-emits the input memory field-for-field, and
// cmd/ghost converts a reflection.ReflectMemory to a memory.Memory carrying only
// category, content, importance and tags — no Source, no Scope, no Provenance.
func retainedMemory(category, content string, importance float32, tags []string) Memory {
	return Memory{Category: category, Content: content, Importance: importance, Tags: tags}
}

// TestReplaceNonManualRetainedRowKeepsCreatedAtAndSource is issue #623.
//
// Retention (the #549 drop guard, now every category) re-emits a memory the
// consolidator left out byte-identically, so it reaches ReplaceNonManual's
// exact-content reuse path. That UPDATE stamped created_at = datetime('now')
// and source = 'reflection' onto the row, so omitting a stale memory wrote it
// back looking brand new: its 30/45-day decay restarted on every applied
// reflect, so an architecture/decision/pattern memory the model kept dropping
// never aged out, and its mcp provenance was overwritten with 'reflection'.
func TestReplaceNonManualRetainedRowKeepsCreatedAtAndSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the ingestion worker retries a dead-lettered event three times before parking it"
	confidence := 0.8
	id, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.6,
		Tags: []string{"worker"}, Agent: "claude-code", SessionID: "sess-1", SourceRef: "PR-623",
		Confidence: &confidence,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, id, 100)
	staleCreatedAt := memoryCreatedAt(t, s, id)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", content, 0.6, []string{"worker"}),
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	got := rows[0]
	if got.CreatedAt != staleCreatedAt {
		t.Errorf("created_at = %q, want the stored %q — retention re-stamped the row as new, "+
			"restarting the decay clock of a memory the consolidator omitted as stale",
			got.CreatedAt, staleCreatedAt)
	}
	if got.Source != "mcp" {
		t.Errorf("source = %q, want mcp — retention overwrote the provenance of a memory it did not rewrite", got.Source)
	}
	if got.Agent != "claude-code" || got.SessionID != "sess-1" || got.SourceRef != "PR-623" ||
		got.Confidence == nil || *got.Confidence != confidence {
		t.Errorf("provenance = (%q, %q, %q, %v), want (claude-code, sess-1, PR-623, 0.8)",
			got.Agent, got.SessionID, got.SourceRef, got.Confidence)
	}
}

// TestReplaceNonManualRetainedRowStillDecays is the ranking half of #623, and
// the reason created_at matters: it is the age term in DecayRankingSQL, so
// re-stamping a retained row is what let an omitted stale memory keep scoring
// as fresh. A 100-day-old architecture memory at importance 0.6 scores
// 0.6 * 0.31 ≈ 0.19, so a fresh 0.5 memory must outrank it.
func TestReplaceNonManualRetainedRowStillDecays(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const staleContent = "the ledger store keeps its write-ahead log on a dedicated volume"
	staleID, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: staleContent, Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create stale: %v", err)
	}
	setCreatedAtDaysAgo(t, s, staleID, 100)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", staleContent, 0.6, []string{}),
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: "the scheduler drains its work queue in batches of 64",
		Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("top memories = %d, want 2", len(top))
	}
	if top[0].ID == staleID {
		t.Errorf("a 100-day-old retained memory outranks a fresh one: %+v — retention "+
			"restarted its decay", top)
	}
}

// TestReplaceNonManualRewrittenRowStillRefreshesCreatedAt is the other side of
// #623's decision: a row the replace genuinely rewrote still counts as
// refreshed knowledge (#279). Content is byte-identical here, so the reuse
// path still runs, but the category moves — and a category change is a real
// rewrite: it is half of what DecayRankingSQL decays on, so a recategorized
// row is not the same knowledge with a new age.
func TestReplaceNonManualRewrittenRowStillRefreshesCreatedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "identical content survives"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: content, Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, id, 100)

	before, err := s.CurrentTimestamp(ctx)
	if err != nil {
		t.Fatalf("CurrentTimestamp: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "convention", Content: content, Importance: 0.5, Tags: []string{}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	got := rows[0]
	if got.Category != "convention" {
		t.Fatalf("category = %q, want convention — the rewrite must still be applied in place", got.Category)
	}
	if got.CreatedAt < before {
		t.Errorf("created_at = %q, want >= pre-replace %q — a rewritten memory is refreshed knowledge",
			got.CreatedAt, before)
	}
	if got.Source != "reflection" {
		t.Errorf("source = %q, want reflection", got.Source)
	}
}

// TestReplaceNonManualUnchangedReuseStillAppliesRewriteFields guards the other
// side of the #623 split: keeping created_at must not turn the unchanged branch
// into a no-op. The consolidator may reweight, retag or narrow the scope of a
// memory whose text and category it left exactly as they were, and those
// decisions have to land — only the age and the source are preserved.
func TestReplaceNonManualUnchangedReuseStillAppliesRewriteFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the deploy pipeline requires a manual approval gate"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "convention", Content: content, Source: "mcp", Importance: 0.4, Tags: []string{},
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, id, 100)
	createdAt := memoryCreatedAt(t, s, id)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "convention", Content: content, Importance: 0.9, Tags: []string{"deploy", "ci"},
		Scope: map[string]string{"environment": "production"},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	got := rows[0]
	if got.Importance != 0.9 {
		t.Errorf("importance = %v, want 0.9", got.Importance)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "deploy" || got.Tags[1] != "ci" {
		t.Errorf("tags = %v, want [deploy ci]", got.Tags)
	}
	if got.Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production", got.Scope)
	}
	if got.CreatedAt != createdAt {
		t.Errorf("created_at = %q, want the stored %q", got.CreatedAt, createdAt)
	}
}
