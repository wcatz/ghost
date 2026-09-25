package memory

import (
	"context"
	"testing"
)

// TestUpsertFoldOnlyDoesNotInsertTheIncomingText is issue #544.
//
// _global accumulated 68 redundant rows in 19 clusters — one "gouroboros PR
// workflow" fact had nine reflection-sourced paraphrases. Every project scope
// had zero such clusters, because _global is written by promotion from every
// project's reflect, while project memories are written once by one project.
//
// Upsert's normal fold strengthens the target and then inserts the incoming
// text as its own row linked as a duplicate. That is right for a save: the
// caller explicitly asked for this text to be stored, so the new wording is
// kept. It is wrong for promotion, where the same fact arriving from another
// project's reflection is a fresh paraphrase of something _global already
// knows, and a row per paraphrase is pure bloat that still occupies a window
// slot and feeds resolve and supersede.
func TestUpsertFoldOnlyDoesNotInsertTheIncomingText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// The same sentence as a second project's reflect would re-emit it: same
	// words, different case and spacing. FoldOnly collapses this and only this
	// — see TestFoldOnlyEquivalentTable for the near-matches it must refuse.
	id, dupOf, score, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The  Gouroboros PR Workflow Pushes From The Laptop Clone",
		"reflection", 0.7, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}

	if dupOf != first {
		t.Errorf("duplicateOf = %q, want the existing memory %q — fold-only must still find it", dupOf, first)
	}
	if id != first {
		t.Errorf("id = %q, want the existing memory %q — fold-only must not mint a new row", id, first)
	}
	if score <= 0 {
		t.Errorf("score = %v, want the match score to be reported", score)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("project holds %d memories, want 1 — fold-only inserted the restatement: %+v", len(all), all)
	}
	if all[0].Content != "the gouroboros PR workflow pushes from the laptop clone" {
		t.Errorf("content = %q, want the original wording preserved", all[0].Content)
	}
}

// TestUpsertFoldOnlyStillStrengthens: folding is not discarding. The incoming
// text is evidence the fact is worth keeping, so the existing row must be
// strengthened exactly as the ordinary fold path strengthens it — otherwise
// promotion would silently lose the signal that made the duplicate worth
// noticing.
func TestUpsertFoldOnlyStillStrengthens(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	before, err := s.GetByIDs(ctx, []string{first})
	if err != nil || len(before) == 0 {
		t.Fatalf("GetByIDs before: %v", err)
	}

	// The same sentence, re-cased: near-identical, so it folds and strengthens.
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The Gouroboros PR Workflow Pushes From The Laptop Clone",
		"reflection", 0.7, nil, UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}

	after, err := s.GetByIDs(ctx, []string{first})
	if err != nil || len(after) == 0 {
		t.Fatalf("GetByIDs after: %v", err)
	}
	if after[0].Importance <= before[0].Importance {
		t.Errorf("importance = %v, want it strengthened above %v — fold-only must not discard the duplicate",
			after[0].Importance, before[0].Importance)
	}
	if after[0].AccessCount <= before[0].AccessCount {
		t.Errorf("access_count = %d, want it strengthened above %d", after[0].AccessCount, before[0].AccessCount)
	}
}

// TestUpsertFoldOnlyInsertsWhenThereIsNoDuplicate: fold-only changes what
// happens on a match, not on a miss. A genuinely new global memory must still
// be written, or promotion would silently drop facts _global has never seen.
func TestUpsertFoldOnlyInsertsWhenThereIsNoDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"cardano epoch boundaries are driven by the slot length", "reflection", 0.7, nil,
		UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert on a miss: %v", err)
	}
	if id == "" {
		t.Fatal("fold-only returned no id for a memory _global has never seen")
	}
	if dupOf != "" {
		t.Errorf("duplicateOf = %q, want empty on a miss", dupOf)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("project holds %d memories, want the new one written", len(all))
	}
}

// TestUpsertDefaultStillInsertsTheDuplicateRow: fold-only is opt-in and scoped
// to promotion. Ordinary saves must keep inserting the caller's text as its own
// row, because the caller explicitly asked for that text to be stored.
func TestUpsertDefaultStillInsertsTheDuplicateRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone", "reflection", 0.7, nil); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second, _, _, err := s.Upsert(ctx, testProject, "fact",
		"the gouroboros PR workflow pushes from the laptop clone too", "reflection", 0.7, nil)
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("project holds %d memories, want 2 — the default fold must keep the caller's wording", len(all))
	}
	if second == "" {
		t.Error("default fold returned no id for the inserted row")
	}
}

// TestUpsertFoldOnlyInsertsInsteadOfFoldingIntoResolvedRow is the case the
// default fold is allowed to get away with and FoldOnly is not. A default
// upsert that re-saves the text of a resolved memory strengthens it and leaves
// it resolved, which is what TestUnresolveOnWrite pins. FoldOnly strengthens the
// row and then returns WITHOUT storing the incoming wording — so if the target
// is a resolved _global row, loadGlobalMemories and GetTopMemories both exclude
// it and the promotion reports success while the memory is in no readable place
// at all. It has to fall through to the insert.
func TestUpsertFoldOnlyInsertsInsteadOfFoldingIntoResolvedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const content = "the gouroboros release checklist is documented in the ops runbook"
	// Exactly one row, and it is resolved. A live sibling would be a perfectly
	// good fold target, so the defect only appears when the resolved row is the
	// sole candidate the probe can return.
	resolvedID, err := s.Create(ctx, "_global", Memory{
		Category: "fact", Content: content, Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolvedID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	before := countMemories(t, s, "_global")
	// The same wording again: the probe will select the resolved row.
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "fact", content, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly upsert: %v", err)
	}

	after := countMemories(t, s, "_global")
	if after != before+1 {
		t.Fatalf("_global holds %d memories, want %d — folding into a resolved row stored nothing anywhere",
			after, before+1)
	}
	// And the live row must be findable, which is the whole point.
	rows, err := s.GetAll(ctx, "_global", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	live := 0
	for _, m := range rows {
		if m.Content == content {
			live++
		}
	}
	if live == 0 {
		t.Error("the promotion left no readable _global row behind")
	}
}

func countMemories(t *testing.T, s *Store, projectID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

// TestUpsertFoldOnlyKeepsContradictingInstruction is why FoldOnly needs a
// conflict check the default fold does not. The default fold keeps the
// caller's wording, so "never deploy staging" survives even when it is scored
// against "always deploy staging" and linked as a duplicate. FoldOnly commits
// only the strengthening update and returns: folding a contradiction there
// strengthens the instruction it contradicts and drops the one being promoted,
// so a promotion reports success while the memory is gone.
//
// Both halves are pinned: a contradicting pair must NOT fold, and a
// non-contradicting pair of similar length still must.
func TestUpsertFoldOnlyKeepsContradictingInstruction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const always = "always deploy staging before merging the release branch"
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "gotcha", Content: always, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	const never = "never deploy staging before merging the release branch"

	before := countMemories(t, s, "_global")
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "gotcha", never, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly upsert: %v", err)
	}
	after := countMemories(t, s, "_global")
	if after != before+1 {
		t.Fatalf("_global holds %d memories, want %d — a contradicting instruction was folded away",
			after, before+1)
	}

	// The control: a genuine restatement still folds, so the check is not just
	// refusing everything that scores well.
	// Same sentence, re-cased: near-identical, so it still folds. The ruling is
	// that FoldOnly collapses restatements, not that it refuses to fold.
	const restate = "Always Deploy Staging Before Merging The Release Branch"
	before = countMemories(t, s, "_global")
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "gotcha", restate, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly restatement: %v", err)
	}
	if got := countMemories(t, s, "_global"); got != before {
		t.Errorf("_global holds %d memories, want %d — a near-identical restatement stopped folding", got, before)
	}
}

// TestUpsertFoldOnlyKeepsContradictingNumber covers the numeric leg: a near
// match that differs only in a number is a different rule, not a rewording.
func TestUpsertFoldOnlyKeepsContradictingNumber(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const port80 = "the health check endpoint listens on port 80"
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "fact", Content: port80, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := countMemories(t, s, "_global")
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "fact",
		"the health check endpoint listens on port 81", "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly upsert: %v", err)
	}
	if got := countMemories(t, s, "_global"); got != before+1 {
		t.Errorf("_global holds %d memories, want %d — a different port folded into port 80", got, before+1)
	}
}

// TestFoldOnlyDoesNotCommitCallersTransaction covers Eu_S. FoldOnly's early
// return used to commit unconditionally, while the default path's commit is
// guarded by ownTx. An upsert that arrives inside a caller's transaction then
// ends that transaction: every later statement of the caller fails with
// "transaction has already been committed or rolled back", and the caller's own
// Rollback can no longer undo the partial work. ApplyReflection is exactly that
// caller — it wraps a savepoint around a _global upsert — so the fold path
// would have committed the savepoint's parent.
//
// The test asserts the transaction is still usable after the fold, which is the
// property the caller actually depends on.
func TestFoldOnlyDoesNotCommitCallersTransaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const content = "run the smoke test suite before tagging a release"
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "preference", Content: content, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	txCtx := withStoreTx(ctx, tx)

	if _, _, _, err := s.UpsertWithOptions(txCtx, "_global", "preference", content, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly inside a transaction: %v", err)
	}

	// The caller's transaction must still be open: this statement is the test.
	if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatalf("caller transaction was committed by FoldOnly: %v", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE project_id = '_global'`).Scan(&n); err != nil {
		t.Fatalf("caller cannot read after a FoldOnly upsert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestUpsertFoldOnlyKeepsContradictingInstructionAcrossCategories covers Eu_K.
// The contradiction guard was applied only to the same-category probe, but the
// cross-category probe runs whenever the same-category one misses — and a
// candidate that differs only in category is exactly the shape it exists for.
func TestUpsertFoldOnlyKeepsContradictingInstructionAcrossCategories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const port80 = "the health check endpoint listens on port 80"
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "fact", Content: port80, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := countMemories(t, s, "_global")
	// Same wording, different category, different port: the cross-category
	// probe's territory, and 7/9 of the tokens shared.
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "gotcha",
		"the health check endpoint listens on port 81", "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly upsert: %v", err)
	}
	if got := countMemories(t, s, "_global"); got != before+1 {
		t.Errorf("_global holds %d memories, want %d — a contradicting cross-category candidate was folded away",
			got, before+1)
	}
}
