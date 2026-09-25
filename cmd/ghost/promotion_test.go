package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// fakePromoter records what promotion tried to write and can be told to fail
// specific writes, so the recovery path is reachable without a database.
type fakePromoter struct {
	ensureErr error
	failOn    map[string]bool // content → fail the Upsert
	ensured   int
	upserted  []string
	foldOnly  []bool // opts.FoldOnly per UpsertWithOptions call, in order
}

func (f *fakePromoter) EnsureProject(ctx context.Context, id, path, name string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured++
	return nil
}

func (f *fakePromoter) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts memory.UpsertOptions) (string, string, float64, error) {
	if f.failOn[content] {
		return "", "", 0, errors.New("injected upsert failure")
	}
	f.upserted = append(f.upserted, projectID+":"+content)
	f.foldOnly = append(f.foldOnly, opts.FoldOnly)
	return "new-id", "", 0, nil
}

// TestApplyPromotionWritesNothingUnlessAsked: with promotion off the
// candidates are part of projectMems and are applied there, so promotion must
// not touch _global at all — otherwise the same memory lands in both places
// and the default silently becomes "promoted".
func TestApplyPromotionWritesNothingUnlessAsked(t *testing.T) {
	f := &fakePromoter{}
	promoted, failed := applyPromotion(context.Background(), f, globals("a", "b"), false)

	if promoted != 0 || failed != nil {
		t.Errorf("promoted=%d failed=%v, want 0 and nil", promoted, failed)
	}
	if f.ensured != 0 || len(f.upserted) != 0 {
		t.Errorf("wrote to the store without being asked: ensured=%d upserted=%v", f.ensured, f.upserted)
	}
}

// TestApplyPromotionWritesEveryCandidate: the opt-in has to actually promote.
// This is the behaviour the nesting bug broke — but the bug was in where this
// is called, not in this function, so the guard against *that* is that the
// call site is unconditional. What this covers is the write itself.
func TestApplyPromotionWritesEveryCandidate(t *testing.T) {
	f := &fakePromoter{}
	promoted, failed := applyPromotion(context.Background(), f, globals("a", "b"), true)

	if promoted != 2 {
		t.Errorf("promoted = %d, want 2", promoted)
	}
	if len(failed) != 0 {
		t.Errorf("failed = %v, want none", failed)
	}
	if f.ensured != 1 {
		t.Errorf("EnsureProject called %d times, want 1", f.ensured)
	}
	if len(f.upserted) != 2 {
		t.Errorf("upserted = %v, want both candidates", f.upserted)
	}
	for _, u := range f.upserted {
		if u[:len("_global")] != "_global" {
			t.Errorf("candidate written to %q — promotion must target _global", u)
		}
	}
}

// TestApplyPromotionReturnsTheOnesItCouldNotWrite is the memory-loss guard.
//
// With promotion on, global candidates are deliberately kept OUT of
// projectMems, so ReplaceNonManual has already deleted them from the project.
// If a promotion write then fails and the failure is only logged, the memory
// exists in neither place — silently gone. Returning it is what lets the
// caller put it back, so the worst case is "not promoted" rather than "lost".
func TestApplyPromotionReturnsTheOnesItCouldNotWrite(t *testing.T) {
	f := &fakePromoter{failOn: map[string]bool{"second": true}}
	promoted, failed := applyPromotion(context.Background(), f, globals("first", "second", "third"), true)

	if promoted != 2 {
		t.Errorf("promoted = %d, want 2 (the other two must still go through)", promoted)
	}
	if len(failed) != 1 {
		t.Fatalf("failed = %v, want exactly the one that could not be written", failed)
	}
	if failed[0].Content != "second" {
		t.Errorf("failed[0] = %q, want %q — the caller re-inserts these by content", failed[0].Content, "second")
	}
}

// TestApplyPromotionReturnsEverythingWhenTheBucketCannotBeOpened: if
// EnsureProject fails, nothing was written anywhere, so every candidate is
// unaccounted for and all of them must come back for re-insertion.
func TestApplyPromotionReturnsEverythingWhenTheBucketCannotBeOpened(t *testing.T) {
	f := &fakePromoter{ensureErr: fmt.Errorf("_global unavailable")}
	promoted, failed := applyPromotion(context.Background(), f, globals("a", "b", "c"), true)

	if promoted != 0 {
		t.Errorf("promoted = %d, want 0", promoted)
	}
	if len(failed) != 3 {
		t.Errorf("failed = %d, want all 3 — none of them were written anywhere", len(failed))
	}
	if len(f.upserted) != 0 {
		t.Errorf("upserted despite EnsureProject failing: %v", f.upserted)
	}
}

// TestApplyPromotionEmptyRoundIsANoop keeps the guard total: no candidates
// means no bucket creation either, so a round that produced nothing does not
// create _global as a side effect.
func TestApplyPromotionEmptyRoundIsANoop(t *testing.T) {
	f := &fakePromoter{}
	promoted, failed := applyPromotion(context.Background(), f, nil, true)

	if promoted != 0 || failed != nil {
		t.Errorf("promoted=%d failed=%v, want 0 and nil", promoted, failed)
	}
	if f.ensured != 0 {
		t.Errorf("created _global for an empty round (ensured=%d)", f.ensured)
	}
}

// TestApplyPromotionFoldsIntoExistingGlobal is issue #544 at the promotion
// boundary: every project's reflect feeds _global, so the same cross-project
// fact arrives as a fresh paraphrase again and again. _global accumulated 68
// redundant rows in 19 clusters that way, one of them nine paraphrases of a
// single gouroboros fact, while every project scope had none — a project
// memory is written once by one project, and _global is written by all of them.
//
// Promotion must therefore fold: strengthen what is already there, and do not
// store a second wording of it.
func TestApplyPromotionFoldsIntoExistingGlobal(t *testing.T) {
	f := &fakePromoter{}
	promoted, failed := applyPromotion(context.Background(), f,
		globals("first", "second", "third"), true)
	if promoted != 3 || len(failed) != 0 {
		t.Fatalf("promoted=%d failed=%d, want 3 promoted and none failed", promoted, len(failed))
	}
	for i, fold := range f.foldOnly {
		if !fold {
			t.Errorf("write %d did not request a fold: promotion stored a second row for a fact _global may already have", i)
		}
	}
}

// TestApplyPromotionDoesNotFoldWithoutOptIn: the default is that nothing is
// promoted at all, so no fold may be requested either. A fold-only write on a
// run that promoted nothing would be a silent data change behind a flag that
// was never set.
func TestApplyPromotionDoesNotFoldWithoutOptIn(t *testing.T) {
	f := &fakePromoter{}
	promoted, _ := applyPromotion(context.Background(), f, globals("a", "b"), false)
	if promoted != 0 {
		t.Fatalf("promoted=%d, want 0 without --promote-globals", promoted)
	}
	if len(f.foldOnly) != 0 {
		t.Errorf("fold requested %d times with promotion disabled", len(f.foldOnly))
	}
}

// TestRecoverUnpromotedSkipsWhenNothingWasReplaced is the no-project path. A
// --promote-globals round can contain only global candidates, so
// ReplaceNonManual never runs and nothing is deleted from the project. The
// recovery then re-inserts candidates that are already there, duplicating every
// one — the project ends up with a second copy of a fact the consolidator
// emitted once.
func TestRecoverUnpromotedSkipsWhenNothingWasReplaced(t *testing.T) {
	dir := t.TempDir()
	db, err := memory.OpenDB(filepath.Join(dir, "recover.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The candidate is still in the project, untouched by any replacement.
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "preference", Content: "a cross-project preference", Source: "reflection",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	kept, err := recoverUnpromoted(ctx, store, "proj", globals("a cross-project preference"), false)
	if err != nil {
		t.Fatalf("recoverUnpromoted: %v", err)
	}
	if kept != 0 {
		t.Errorf("kept = %d, want 0 — nothing was replaced, so there is nothing to recover", kept)
	}

	all, err := store.GetAll(ctx, "proj", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("project holds %d memories, want 1 — recovery duplicated a candidate that was never removed", len(all))
	}
}

// TestRecoverUnpromotedRestoresAfterReplaced: when the replacement DID commit,
// the candidate really was deleted, so recovery is the only copy left and it has
// to happen.
func TestRecoverUnpromotedRestoresAfterReplaced(t *testing.T) {
	dir := t.TempDir()
	db, err := memory.OpenDB(filepath.Join(dir, "recover.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "preference", Content: "a cross-project preference", Source: "reflection",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Promotion was on, so the candidate is not in the replacement set and is
	// deleted by it.
	if _, err := store.ReplaceNonManual(ctx, "proj", []memory.Memory{
		{Category: "fact", Content: "a project-only fact", Source: "reflection"},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	kept, err := recoverUnpromoted(ctx, store, "proj", globals("a cross-project preference"), true)
	if err != nil {
		t.Fatalf("recoverUnpromoted: %v", err)
	}
	if kept != 1 {
		t.Errorf("kept = %d, want 1", kept)
	}
	all, err := store.GetAll(ctx, "proj", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	found := false
	for _, m := range all {
		if m.Content == "a cross-project preference" {
			found = true
		}
	}
	if !found {
		t.Errorf("the unpromoted candidate was lost: %+v", all)
	}
}

// TestRecoverUnpromotedReportsWriteFailure: a candidate that fails promotion
// AND fails to return to the project now exists in neither place, because the
// project replacement has already committed. Swallowing that and continuing
// exits 0 on a consolidation that silently lost a memory, so the failure has to
// reach the caller.
func TestRecoverUnpromotedReportsWriteFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := memory.OpenDB(filepath.Join(dir, "recover.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.ReplaceNonManual(ctx, "proj", []memory.Memory{
		{Category: "fact", Content: "a project-only fact", Source: "reflection"},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	// The database is gone, so the recovery write cannot land.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	kept, err := recoverUnpromoted(ctx, store, "proj", globals("a cross-project preference"), true)
	if err == nil {
		t.Fatal("recoverUnpromoted reported success, want the failed write reported")
	}
	if kept != 0 {
		t.Errorf("kept = %d, want 0", kept)
	}
	// The error must identify the memory WITHOUT reproducing it: the content is
	// untrusted text that a reflection pass derived from project files, and
	// stderr is a durable, world-readable destination.
	if strings.Contains(err.Error(), "a cross-project preference") {
		t.Errorf("error = %v, want it to identify the memory by digest, not by content", err)
	}
	if !strings.Contains(err.Error(), "memory sha256:") {
		t.Errorf("error = %v, want an opaque content digest so the row can be identified", err)
	}
}
