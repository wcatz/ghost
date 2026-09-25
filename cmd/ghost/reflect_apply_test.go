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
	calls           int
	projectMems     []memory.Memory
	globalMems      []memory.Memory
	promote         bool
	consolidated    string
	resultPreserved []string
	resultPromoted  int
	resultKept      []memory.Memory
	err             error
}

func (f *fakeReflectionApplier) ApplyReflection(_ context.Context, _ string, projectMems, globalMems []memory.Memory, consolidatedSince string, promote bool) (preserved []string, promoted int, keptMems []memory.Memory, err error) {
	f.calls++
	f.projectMems = append([]memory.Memory(nil), projectMems...)
	f.globalMems = append([]memory.Memory(nil), globalMems...)
	f.promote = promote
	f.consolidated = consolidatedSince
	return f.resultPreserved, f.resultPromoted, f.resultKept, f.err
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
	preserved, promoted, kept, err := applyReflection(
		context.Background(), f, "p1", nil, globals("global"), "since", true,
	)
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

func TestApplyReflectionFoldsGlobalsIntoProjectUnlessAsked(t *testing.T) {
	for _, tc := range []struct {
		name        string
		promote     bool
		wantProject int
		wantGlobal  int
	}{
		{name: "default keeps candidates in project", promote: false, wantProject: 2, wantGlobal: 0},
		{name: "explicit promotion keeps them separate", promote: true, wantProject: 1, wantGlobal: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeReflectionApplier{}
			_, _, _, err := applyReflection(
				context.Background(), f, "p1", projectMemories("project"), globals("global"), "since", tc.promote,
			)
			if err != nil {
				t.Fatalf("applyReflection: %v", err)
			}
			if len(f.projectMems) != tc.wantProject || len(f.globalMems) != tc.wantGlobal {
				t.Errorf("applier received project=%d global=%d, want %d/%d", len(f.projectMems), len(f.globalMems), tc.wantProject, tc.wantGlobal)
			}
		})
	}
}

func TestApplyReflectionPropagatesStoreFailure(t *testing.T) {
	want := errors.New("transaction failed")
	f := &fakeReflectionApplier{err: want}
	_, _, _, err := applyReflection(context.Background(), f, "p1", projectMemories("x"), nil, "", true)
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
					_, _, _, err := applyReflection(context.Background(), f, "p1", project, globals("global"), "", promote)
					if (err != nil) != fail {
						t.Fatalf("error = %v, want error=%t", err, fail)
					}
					if f.calls != 1 {
						t.Fatalf("applier calls = %d, want 1", f.calls)
					}
					wantProject := len(project)
					wantGlobal := 1
					if !promote {
						wantProject++
						wantGlobal = 0
					}
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

	got := appliedSummary(project, global, 1, true)
	want := "1 memories consolidated (1 fact), 1 promoted to global"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	kept := appliedSummary(append(append([]reflection.ReflectMemory(nil), project...), global...), global, 0, false)
	wantKept := "2 memories consolidated (1 fact, 1 preference), 1 cross-project candidates kept project-scoped"
	if kept != wantKept {
		t.Errorf("kept summary = %q, want %q", kept, wantKept)
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

	_, promoted, kept, err := applyReflection(ctx, store, "proj",
		projectMemories("a consolidated project fact"),
		globals("prefer running the linter before pushing"),
		"", false)
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
