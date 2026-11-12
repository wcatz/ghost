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
	calls                 int
	projectMems           []memory.Memory
	globalMems            []memory.Memory
	promote               bool
	consolidated          string
	resultPreserved       []string
	resultPromoted        int
	resultKept            []memory.Memory
	resultProjectDropped  memory.EmissionDrops
	resultGlobalDropped   memory.EmissionDrops
	resultDeleted         int
	resultAppliedCategory map[string]int
	err                   error
}

func (f *fakeReflectionApplier) ApplyReflection(_ context.Context, _ string, projectMems, globalMems []memory.Memory, consolidatedSince string, promote bool, _ []memory.CorpusRow) (preserved []string, promoted int, keptMems []memory.Memory, projectDropped, globalDropped memory.EmissionDrops, deleted int, appliedByCategory map[string]int, err error) {
	f.calls++
	f.projectMems = append([]memory.Memory(nil), projectMems...)
	f.globalMems = append([]memory.Memory(nil), globalMems...)
	f.promote = promote
	f.consolidated = consolidatedSince
	return f.resultPreserved, f.resultPromoted, f.resultKept, f.resultProjectDropped, f.resultGlobalDropped, f.resultDeleted, f.resultAppliedCategory, f.err
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

func TestApplyReflectionPropagatesStoreFailure(t *testing.T) {
	want := errors.New("transaction failed")
	f := &fakeReflectionApplier{err: want}
	if _, err := applyReflection(context.Background(), f, "p1", projectMemories("x"), nil, "", true, nil, nil); !errors.Is(err, want) {
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
					_, err2 := applyReflection(context.Background(), f, "p1", project, globals("global"), "", promote, nil, nil)
					if (err2 != nil) != fail {
						t.Fatalf("error = %v, want error=%t", err2, fail)
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

// An all-refused round that still deletes omitted rows must say so on the
// Applied line: "0 memories consolidated" alone hid the removed rows (#1124).
func TestWriteAppliedReportStatesRemovedRowsOfAllRefusedRound(t *testing.T) {
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "reflect.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var ids []string
	for _, c := range []string{"keep alpha", "stale beta", "stale gamma"} {
		id, err := store.Create(ctx, "p1", memory.Memory{
			Category: "fact", Content: c, Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create %q: %v", c, err)
		}
		ids = append(ids, id)
	}
	rows, err := store.GetByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	var fed []memory.CorpusRow
	for _, m := range rows {
		fed = append(fed, memory.CorpusRow{
			ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt,
			Category: m.Category, Importance: m.Importance, Tags: m.Tags,
		})
	}
	reweighted := float32(0.95)
	if err := store.UpdateMemory(ctx, "p1", ids[0], nil, nil, &reweighted, nil); err != nil {
		t.Fatalf("UpdateMemory alpha: %v", err)
	}
	projectMems := []reflection.ReflectMemory{{Category: "fact", Content: "keep alpha"}}
	res, err := applyReflection(
		ctx, store, "p1", projectMems, nil, "", false, nil, fed)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	keptProject := res.KeptProject
	keptGlobal := res.KeptGlobal
	promoted := res.Promoted
	keptMems := res.KeptMems
	globalDropped := res.GlobalDropped
	deleted := res.Deleted
	byCategory := res.AppliedByCategory
	applied := res.Applied
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (beta and gamma)", deleted)
	}

	var out strings.Builder
	_, wrote := writeAppliedReport(&out, applied, false, byCategory, keptProject, keptGlobal, keptMems, projectMems, promoted, globalDropped.Total(), deleted)
	if !wrote {
		t.Fatal("wrote = false, want the Applied branch to print")
	}
	want := "Applied: 0 memories consolidated, 2 removed\n(use --restore to undo)\n"
	if out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
}

// A round that deleted nothing prints exactly what it printed before.
func TestWriteAppliedReportOmitsRemovedCountWhenNothingDeleted(t *testing.T) {
	projectMems := []reflection.ReflectMemory{{Category: "fact", Content: "a"}}
	var out strings.Builder
	_, wrote := writeAppliedReport(&out, true, false, map[string]int{"fact": 1}, projectMems, nil, nil, projectMems, 0, 0, 0)
	if !wrote {
		t.Fatal("wrote = false")
	}
	if strings.Contains(out.String(), "removed") {
		t.Errorf("report mentions removed rows on a round that deleted none: %q", out.String())
	}
	if !strings.HasPrefix(out.String(), "Applied: "+appliedSummary(map[string]int{"fact": 1}, nil, 0, 0, false)+"\n") {
		t.Errorf("Applied line changed: %q", out.String())
	}
}

// A promoted merge carries its sources' evidence and decision links into
// _global, and a restore of the project leaves it there with them: the hint has
// to say so, or an operator who removes nothing by hand leaves two live rows
// claiming the same decision beside the restored sources.
func TestRestoreHintSaysPromotedGlobalsKeepWhatTheyCarried(t *testing.T) {
	const want = "(use --restore to undo the project replace; promoted globals stay in _global with the evidence and decision links they carried from the replaced rows, and must be removed from _global by hand)"
	if got := restoreHint(1); got != want {
		t.Errorf("restoreHint(1) = %q, want %q", got, want)
	}
}
