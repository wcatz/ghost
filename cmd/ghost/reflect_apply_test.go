package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

type fakeReflectionApplier struct {
	calls                int
	projectMems          []memory.Memory
	globalMems           []memory.Memory
	promote              bool
	consolidated         string
	resultPreserved      []string
	resultPromoted       int
	resultKept           []memory.Memory
	resultProjectDropped int
	resultGlobalDropped  int
	err                  error
}

func (f *fakeReflectionApplier) ApplyReflection(_ context.Context, _ string, projectMems, globalMems []memory.Memory, consolidatedSince string, promote bool, _ []memory.CorpusRow) (preserved []string, promoted int, keptMems []memory.Memory, projectDropped, globalDropped int, err error) {
	f.calls++
	f.projectMems = append([]memory.Memory(nil), projectMems...)
	f.globalMems = append([]memory.Memory(nil), globalMems...)
	f.promote = promote
	f.consolidated = consolidatedSince
	return f.resultPreserved, f.resultPromoted, f.resultKept, f.resultProjectDropped, f.resultGlobalDropped, f.err
}

func projectMemories(contents ...string) []reflection.ReflectMemory {
	out := make([]reflection.ReflectMemory, len(contents))
	for i, content := range contents {
		out[i] = reflection.ReflectMemory{Category: "fact", Content: content}
	}
	return out
}

func globals(contents ...string) []reflection.ReflectMemory {
	out := make([]reflection.ReflectMemory, len(contents))
	for i, content := range contents {
		out[i] = reflection.ReflectMemory{Category: "preference", Content: content, Scope: "global"}
	}
	return out
}

// TestApplyReflectionAlwaysReachesPromotionWithoutProjectMemories pins the
// orchestration that the original nesting bug bypassed. An opt-in promotion
// request must still be applied when reflection produced no project-scoped
// memories.
func TestApplyReflectionAlwaysReachesPromotionWithoutProjectMemories(t *testing.T) {
	f := &fakeReflectionApplier{resultPromoted: 1}
	_, _, preserved, promoted, kept, _, _, _, err := applyReflection(
		context.Background(), f, "p1", nil, globals("global"), "since", true, nil, nil)

	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("applier calls = %d, want 1", f.calls)
	}
	if len(f.projectMems) != 0 || len(f.globalMems) != 1 {
		t.Fatalf("applier received project=%d global=%d, want 0/1", len(f.projectMems), len(f.globalMems))
	}
	if !f.promote {
		t.Fatal("applier did not receive the explicit promotion request")
	}
	if promoted != 1 || len(kept) != 0 || preserved != nil {
		t.Errorf("result = (%v, %d, %d), want (nil, 1, 0)", preserved, promoted, len(kept))
	}
}

func TestApplyReflectionForwardsBothHalvesSeparately(t *testing.T) {
	// The fold is the store's job now (#1051): ApplyReflection folds the
	// cross-project rows into the project replace when promotion is off, AFTER
	// its own corpus guard has filtered them. A fold here would put those rows
	// into the one list the store sees, so its two drop counts (project/global)
	// could no longer be attributed to the half that produced them, and the
	// summary's project count and cross-project component subtract different
	// halves. Both lists must travel intact, with the promotion request.
	for _, promote := range []bool{false, true} {
		t.Run(fmt.Sprintf("promote=%t", promote), func(t *testing.T) {
			f := &fakeReflectionApplier{}
			_, _, _, _, _, _, _, _, err := applyReflection(
				context.Background(), f, "p1", projectMemories("project"), globals("global"), "since", promote, nil, nil)

			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
			if len(f.projectMems) != 1 || len(f.globalMems) != 1 {
				t.Errorf("applier received project=%d global=%d, want 1/1", len(f.projectMems), len(f.globalMems))
			}
			if f.promote != promote {
				t.Errorf("applier promote = %t, want %t", f.promote, promote)
			}
		})
	}
}

func TestApplyReflectionPropagatesStoreFailure(t *testing.T) {
	want := errors.New("transaction failed")
	f := &fakeReflectionApplier{err: want}
	_, _, _, _, _, _, _, _, err := applyReflection(context.Background(), f, "p1", projectMemories("x"), nil, "", true, nil, nil)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestApplyReflectionCoversPromotionMatrix(t *testing.T) {
	for _, projectEmpty := range []bool{false, true} {
		for _, promote := range []bool{false, true} {
			for _, fail := range []bool{false, true} {
				name := fmt.Sprintf("project-empty=%t/promote=%t/fail=%t", projectEmpty, promote, fail)
				t.Run(name, func(t *testing.T) {
					project := projectMemories("project")
					if projectEmpty {
						project = nil
					}
					f := &fakeReflectionApplier{}
					if fail {
						f.err = errors.New("injected apply failure")
					}
					_, _, _, _, _, _, _, _, err := applyReflection(context.Background(), f, "p1", project, globals("global"), "", promote, nil, nil)
					if (err != nil) != fail {
						t.Fatalf("error = %v, want error=%t", err, fail)
					}
					if f.calls != 1 {
						t.Fatalf("applier calls = %d, want 1", f.calls)
					}
					// Both halves travel intact; the fold (and any guard drop)
					// happens inside the store, not here.
					wantProject := len(project)
					wantGlobal := 1
					if len(f.projectMems) != wantProject || len(f.globalMems) != wantGlobal {
						t.Errorf("received project=%d global=%d, want %d/%d", len(f.projectMems), len(f.globalMems), wantProject, wantGlobal)
					}
				})
			}
		}
	}
}

func TestAppliedSummaryCountsOnlyRowsAppliedToProject(t *testing.T) {
	project := projectMemories("project")
	project[0].Category = "fact"
	global := globals("global")
	global[0].Category = "preference"

	got := appliedSummary(project, global, 1, 0, 0, true)
	want := "1 memories consolidated (1 fact), 1 promoted to global"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	kept := appliedSummary(append(append([]reflection.ReflectMemory(nil), project...), global...), global, 0, 0, 0, false)
	wantKept := "2 memories consolidated (1 fact, 1 preference), 1 cross-project candidates kept project-scoped"
	if kept != wantKept {
		t.Errorf("kept summary = %q, want %q", kept, wantKept)
	}

	// A dropped emission is subtracted only from the component that counted it:
	// with promotion on, the project count loses the project half's drops while
	// the promoted component is the store's real count; with promotion off, the
	// cross-project rows were folded into the project replace, so the project
	// count loses both halves and the cross-project component loses its own.
	partial := appliedSummary(project, global, 0, 1, 0, true)
	wantPartial := "0 memories consolidated (1 fact), 0 promoted to global"
	if partial != wantPartial {
		t.Errorf("promoted partial summary = %q, want %q", partial, wantPartial)
	}
	folded := appliedSummary(append(append([]reflection.ReflectMemory(nil), project...), global...), global, 0, 2, 1, false)
	wantFolded := "0 memories consolidated (1 fact, 1 preference), 0 cross-project candidates kept project-scoped"
	if folded != wantFolded {
		t.Errorf("folded summary = %q, want %q", folded, wantFolded)
	}
}

func TestRestoreHintQualifiesPromotedGlobals(t *testing.T) {
	if got := restoreHint(0); got != "(use --restore to undo)" {
		t.Errorf("restoreHint(0) = %q", got)
	}
	got := restoreHint(1)
	if got == "(use --restore to undo)" || !containsAll(got, "--restore", "_global") {
		t.Errorf("restoreHint(1) = %q, want a qualified project-only undo hint", got)
	}
}

func containsAll(s string, want ...string) bool {
	for _, part := range want {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}

// TestApplyReflectionKeepsCrossProjectCandidatesWhenPromotionOff is the
// data-loss guard. With --promote-globals off, applyPromotion returns without
// writing anything, so a cross-project candidate has exactly one destination
// left: the project it came from. The replace is built from the project-scoped
// slice alone, which excludes cross-project candidates by construction — so
// folding them in here is what stops ReplaceNonManual from deleting a memory
// that nothing then re-creates.
//
// The end state is asserted against a real store rather than a fake, because the
// failure mode is "deleted from the project and absent everywhere else", which
// only a database can show.
func TestApplyReflectionKeepsCrossProjectCandidatesWhenPromotionOff(t *testing.T) {
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "reflect.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A pre-existing non-manual memory, so ReplaceNonManual has something to
	// replace rather than treating this as the first write.
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "an older project fact", Source: "reflection",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, _, _, promoted, kept, _, _, _, err := applyReflection(ctx, store, "proj", projectMemories("a consolidated project fact"), globals("prefer running the linter before pushing"), "", false, nil, nil)

	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if promoted != 0 {
		t.Errorf("promoted = %d, want 0 — promotion was not requested", promoted)
	}
	if len(kept) != 0 {
		t.Errorf("kept = %d, want 0 — a kept candidate means the global write was attempted at all", len(kept))
	}

	all, err := store.GetAll(ctx, "proj", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var found bool
	for _, m := range all {
		if m.Content == "prefer running the linter before pushing" {
			found = true
		}
	}
	if !found {
		t.Errorf("the cross-project candidate was deleted from the project and stored nowhere: %+v", all)
	}
	// It must not have leaked into _global either — promotion was off.
	if g, err := store.GetAll(ctx, "_global", 10); err == nil && len(g) > 0 {
		t.Errorf("_global holds %d rows with promotion off: %+v", len(g), g)
	}
}

// The corpus guard's drops surface through the wrapper's applied flag: a
// transaction that ran but refused every emission it was handed wrote nothing,
// so the round must be reported and recorded exactly like the credential-drop
// case that returns before the store — no "Applied", no restore hint, no skip
// fingerprint. A partial refusal is a successful round with an honest count:
// applied stays true, the kept sets still flow, and both drop counts travel so
// the summary can subtract only what each component counts.
func TestApplyReflectionReportsAnAllDroppedRoundAsNothingWritten(t *testing.T) {
	t.Run("every emission refused is applied=false", func(t *testing.T) {
		f := &fakeReflectionApplier{resultProjectDropped: 2}
		keptProject, keptGlobal, _, _, _, projectDropped, globalDropped, applied, err := applyReflection(
			context.Background(), f, "p1", projectMemories("a", "b"), nil, "", false, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if applied {
			t.Error("applied = true, want false: the store refused every emission, so nothing was written")
		}
		if projectDropped != 2 || globalDropped != 0 {
			t.Errorf("drops = %d/%d, want 2/0 to travel to the summary", projectDropped, globalDropped)
		}
		if len(keptProject) != 2 {
			t.Errorf("keptProject = %d rows, want 2: the reporting set is still the caller's post-credential-drop set", len(keptProject))
		}
		if len(keptGlobal) != 0 {
			t.Errorf("keptGlobal = %d rows, want 0", len(keptGlobal))
		}
	})

	t.Run("a partial refusal stays applied with the count", func(t *testing.T) {
		f := &fakeReflectionApplier{resultProjectDropped: 1}
		_, _, preserved, promoted, kept, projectDropped, globalDropped, applied, err := applyReflection(
			context.Background(), f, "p1", projectMemories("a", "b"), nil, "", false, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if !applied {
			t.Error("applied = false, want true: one of two emissions was written, so the round did write")
		}
		if projectDropped != 1 || globalDropped != 0 {
			t.Errorf("drops = %d/%d, want 1/0", projectDropped, globalDropped)
		}
		if preserved != nil || promoted != 0 || len(kept) != 0 {
			t.Errorf("result = (%v, %d, %d), want (nil, 0, 0)", preserved, promoted, len(kept))
		}
	})

	t.Run("an all-dropped global half alone is applied=false", func(t *testing.T) {
		f := &fakeReflectionApplier{resultGlobalDropped: 1}
		_, _, _, _, _, projectDropped, globalDropped, applied, err := applyReflection(
			context.Background(), f, "p1", nil, globals("g"), "", true, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if applied {
			t.Error("applied = true, want false: the only emission was a global candidate the store refused")
		}
		if projectDropped != 0 || globalDropped != 1 {
			t.Errorf("drops = %d/%d, want 0/1", projectDropped, globalDropped)
		}
	})
}
