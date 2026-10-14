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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", content, 0.6, []string{"worker"}),
	}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", staleContent, 0.6, []string{}),
	}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "convention", Content: content, Importance: 0.5, Tags: []string{}},
	}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "convention", Content: content, Importance: 0.9, Tags: []string{"deploy", "ci"},
		Scope: map[string]string{"environment": "production"},
	}}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "architecture", Content: content, Importance: 0.5, Tags: []string{}},
	}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "architecture", Content: content, Importance: 0.5, Tags: []string{}},
	}, "", nil); err != nil {
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

// A reuse that folds a consumed source into the reused row changes that row's
// provenance, and source IS a column memory_history records, so the pass files
// the version like any other write (#1283). Without it the change log and an
// as_of read still report the row as the agent save it was, while the live row
// has become the merge's successor.
func TestAReusedRowAbsorbingAConsumedSourceRecordsAProvenanceVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the queue backend is redis lists"
	reused, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the reused row: %v", err)
	}
	other, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a stale note the merge folds in", "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the other source: %v", err)
	}
	// Backdate the stamp so the provenance write is observable: the save and the
	// merge otherwise share a clock second, and a move to `datetime('now')` would
	// read as unmoved.
	setUpdatedAt(t, s, reused, "2020-01-01 00:00:00")
	before, err := s.MemoryHistory(ctx, reused, 10)
	if err != nil {
		t.Fatalf("MemoryHistory before the merge: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.5,
		ReplacesIDs: []string{reused, other},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if !rowExists(t, s, reused) || rowExists(t, s, other) {
		t.Fatalf("the merge did not reuse the row and take the other source (reused live=%v, other live=%v)",
			rowExists(t, s, reused), rowExists(t, s, other))
	}

	// The stamp moves the live row's updated_at in the same statement that files
	// the version, because every writer of the reflect phase must
	// (stampMovingPhases, history_compact.go): a reflect version whose writer left
	// the column alone would make the phase an unreliable witness for the anchor.
	updated := readUpdatedAt(t, s, reused)
	if updated == "2020-01-01 00:00:00" {
		t.Error("the reused row's updated_at is still the backdated value: the provenance stamp filed a version without moving the stamp column")
	}

	after, err := s.MemoryHistory(ctx, reused, 10)
	if err != nil {
		t.Fatalf("MemoryHistory after the merge: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("history versions = %d, want %d: the provenance change was not recorded",
			len(after), len(before)+1)
	}
	// MemoryHistory returns oldest first.
	newest := after[len(after)-1]
	if newest.Source != "reflection" {
		t.Errorf("the newest version's source = %q, want reflection: an as_of read after the merge still reports the agent save's provenance", newest.Source)
	}
	if newest.Phase != phaseReflect {
		t.Errorf("the newest version's phase = %q, want %q", newest.Phase, phaseReflect)
	}
	if newest.RecordedAt != updated {
		t.Errorf("the stamp version's recorded_at = %q, the row's updated_at = %q: a version filed by a stamp-moving writer must carry the time that writer moved the column to",
			newest.RecordedAt, updated)
	}
}

// A reuse is relabelled only when it CONSUMES a named source (#1283): an id no
// row holds, a manual row, and a row the pass keeps are all named by an emission
// but are never folded into the reused row, so stamping reflection provenance on
// it would announce a merge that did not happen.
func TestAReuseIsNotRelabelledByANamedSourceItDoesNotConsume(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the staging cluster answers on port 8443"
	save, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the row: %v", err)
	}
	manual, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a hand-written note", "manual", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the manual row: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.5,
		ReplacesIDs: []string{save, "NOSUCHID", manual},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if got := getOne(t, s, save); got.Source != "mcp" {
		t.Errorf("source = %q, want mcp: neither a missing id nor a manual row is consumed by the merge, so the reused row was not made a successor", got.Source)
	}
	if !rowExists(t, s, manual) {
		t.Error("the manual row was deleted")
	}
	hist, err := s.MemoryHistory(ctx, save, 10)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Errorf("history has %d version(s), want the save's own 1: a reuse that consumed nothing recorded a version", len(hist))
	}
}
