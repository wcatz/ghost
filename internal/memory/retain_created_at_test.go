package memory

import (
	"context"
	"testing"
)

// retainedMemory is exactly what the drop-guard hands back to the replace:
// reflection.RetainGuardedDrops re-emits the input memory field-for-field, and
// cmd/ghost's reflectMemoriesToMemory then adds ProjectID and a hardcoded
// Source of 'reflection' — never a Scope or any Provenance. Source is left off
// here because ReplaceNonManual does not read it: both the insert and the
// rewrite path hardcode 'reflection', so the stored source survives only
// because the unchanged path no longer assigns it.
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

// TestReplaceNonManualReusePickPrefersMatchingCategory is the second half of
// #623, and a real bug the first half left open: the reuse bucket is keyed by
// content alone, so when a project holds the same text in two categories — a
// shape Upsert creates deliberately, keeping the incoming category on a linked
// copy so nothing the caller saved is lost — the emitted memory can be handed
// the other-category row. reusePreservesAge is then evaluated against that
// row, sees a category change, and takes the rewrite branch: created_at and
// source are re-stamped, which is exactly the bug #623 fixes, still happening
// for that memory. Before the pick is deliberate, which same-content row
// SQLite returns first is incidental — the candidate query has no ORDER BY and
// the predicate is served by idx_memories_project_cat or
// idx_memories_project_source.
func TestReplaceNonManualReusePickPrefersMatchingCategory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the same text stored under two different categories"
	gotchaID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: content, Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create gotcha: %v", err)
	}
	archID, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create architecture: %v", err)
	}
	// The gotcha row is the OLDER one, so it also comes first in the candidate
	// query's `ORDER BY created_at, id`. That is what makes the test
	// discriminate: without a category preference the scan order alone decides,
	// so a gotcha row is claimed for an architecture emission.
	setCreatedAtDaysAgo(t, s, gotchaID, 100)
	setCreatedAtDaysAgo(t, s, archID, 10)
	archCreatedAt := memoryCreatedAt(t, s, archID)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "architecture", Content: content, Importance: 0.5, Tags: []string{}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{archID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("the architecture row was deleted and the gotcha row recategorized instead — " +
			"reuse picked the other-category row for an architecture emission")
	}
	if rows[0].CreatedAt != archCreatedAt {
		t.Errorf("created_at = %q, want the stored %q — the same-category row was "+
			"re-stamped because reuse claimed a row from another category", rows[0].CreatedAt, archCreatedAt)
	}
	if rows[0].Source != "mcp" {
		t.Errorf("source = %q, want mcp", rows[0].Source)
	}
}

// TestReplaceNonManualReusePickIsDeterministic pins the other half: with
// byte-identical duplicates in one category — every re-save of the same fact
// through ghost_memory_save inserts another row — only one row is reused and
// the rest are deleted, so which row's created_at survives must be a decision
// and not an accident. Ordering by created_at then id means the oldest row,
// the one the age actually belongs to, is the one that keeps it, and the same
// corpus consolidates to the same ages on every run.
func TestReplaceNonManualReusePickIsDeterministic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the same fact re-saved three times"
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.5,
		})
		if err != nil {
			t.Fatalf("create duplicate %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// Backdate so the OLDEST row is the LAST one inserted. Insertion order is
	// what an unordered scan returns rows in, so this is what makes the test
	// discriminate: picking by scan order keeps the newest duplicate's age,
	// picking by created_at keeps the real one.
	for i, id := range ids {
		setCreatedAtDaysAgo(t, s, id, (i+1)*10)
	}
	oldestID := ids[len(ids)-1]
	oldest := memoryCreatedAt(t, s, oldestID)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "architecture", Content: content, Importance: 0.5, Tags: []string{}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	survivors, err := s.GetAll(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(survivors) != 1 {
		t.Fatalf("survivors = %d (%v), want 1 — the duplicates are deduped, not all kept", len(survivors), memoryIDs(survivors))
	}
	if survivors[0].ID != oldestID {
		t.Errorf("survivor = %s, want the oldest row %s — the reuse pick is incidental, so the "+
			"same corpus consolidates to a different age on each run", survivors[0].ID, oldestID)
	}
	if survivors[0].CreatedAt != oldest {
		t.Errorf("created_at = %q, want the oldest row's %q", survivors[0].CreatedAt, oldest)
	}
}
