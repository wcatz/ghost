package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/reflection"
)

// fakePromoter records what promotion tried to write and can be told to fail
// specific writes, so the recovery path is reachable without a database.
type fakePromoter struct {
	ensureErr error
	failOn    map[string]bool // content → fail the Upsert
	ensured   int
	upserted  []string
}

func (f *fakePromoter) EnsureProject(ctx context.Context, id, path, name string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured++
	return nil
}

func (f *fakePromoter) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, string, float64, error) {
	if f.failOn[content] {
		return "", "", 0, errors.New("injected upsert failure")
	}
	f.upserted = append(f.upserted, projectID+":"+content)
	return "new-id", "", 0, nil
}

func globals(contents ...string) []reflection.ReflectMemory {
	out := make([]reflection.ReflectMemory, len(contents))
	for i, c := range contents {
		out[i] = reflection.ReflectMemory{Category: "preference", Content: c, Scope: "global"}
	}
	return out
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
