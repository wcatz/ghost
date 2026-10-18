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

// TestApplyReflectionAlwaysReachesPromotionWithoutProjectMemories pins the
// orchestration that the original nesting bug bypassed. An opt-in promotion
// request must still be applied when reflection produced no project-scoped
// memories.
func TestApplyReflectionAlwaysReachesPromotionWithoutProjectMemories(t *testing.T) {
	f := &fakeReflectionApplier{resultPromoted: 1}
	_, _, preserved, promoted, kept, _, _, _, _, _, err := applyReflection(
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
			_, _, _, _, _, _, _, _, _, _, err := applyReflection(
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
	_, _, _, _, _, _, _, _, _, _, err := applyReflection(context.Background(), f, "p1", projectMemories("x"), nil, "", true, nil, nil)
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
					_, _, _, _, _, _, _, _, _, _, err := applyReflection(context.Background(), f, "p1", project, globals("global"), "", promote, nil, nil)
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
	global := globals("global")
	global[0].Category = "preference"

	// Total and category breakdown come from the store's per-category applied
	// counts, so a dropped emission is counted in NEITHER and the parts always
	// sum to the total.
	got := appliedSummary(map[string]int{"fact": 1}, global, 1, 0, true)
	want := "1 memories consolidated (1 fact), 1 promoted to global"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	kept := appliedSummary(map[string]int{"fact": 1, "preference": 1}, global, 0, 0, false)
	wantKept := "2 memories consolidated (1 fact, 1 preference), 1 cross-project candidates kept project-scoped"
	if kept != wantKept {
		t.Errorf("kept summary = %q, want %q", kept, wantKept)
	}

	// A dropped emission is subtracted only from the component that counted it:
	// with promotion on, the project count loses the project half's drops while
	// the promoted component is the store's real count; with promotion off, the
	// cross-project rows were folded into the project replace, so the project
	// count loses both halves and the cross-project component loses its own.
	// The map already excludes the refused emissions, so the parts next to a
	// zero total are empty — "0 memories consolidated", not "(1 fact)".
	partial := appliedSummary(map[string]int{}, global, 0, 0, true)
	wantPartial := "0 memories consolidated, 0 promoted to global"
	if partial != wantPartial {
		t.Errorf("promoted partial summary = %q, want %q", partial, wantPartial)
	}
	folded := appliedSummary(map[string]int{}, global, 0, 1, false)
	wantFolded := "0 memories consolidated, 0 cross-project candidates kept project-scoped"
	if folded != wantFolded {
		t.Errorf("folded summary = %q, want %q", folded, wantFolded)
	}

	// The same map drives both numbers: one landed fact among two emissions
	// (the other refused) reports exactly the row that landed.
	landed := appliedSummary(map[string]int{"fact": 1}, global, 0, 0, false)
	wantLanded := "1 memories consolidated (1 fact), 1 cross-project candidates kept project-scoped"
	if landed != wantLanded {
		t.Errorf("landed summary = %q, want %q", landed, wantLanded)
	}
}

func TestCrossProjectKeptCountsOnlyCandidatesTheStoreKept(t *testing.T) {
	// With promotion off the candidate list is the caller's post-credential-drop
	// set and the drop count covers BOTH guard passes — the global half's own and
	// the replace's second pass over the folded list, which the store attributes
	// back to the half that produced the candidate. One subtraction therefore has
	// to describe the whole round, and the "(re-run with --promote-globals)"
	// follow-up is gated on this count: a round that refused every candidate has
	// nothing in the project to inject into every other one.
	one := globals("a candidate")
	two := append(globals("a candidate"), globals("a second candidate")...)

	for _, tc := range []struct {
		name     string
		cands    []reflection.ReflectMemory
		dropped  int
		wantKept int
	}{
		{"nothing refused", one, 0, 1},
		{"one refused", one, 1, 0},
		{"one of two refused", two, 1, 1},
		{"more refusals than candidates floors at zero", two, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := crossProjectKept(tc.cands, tc.dropped)
			if got != tc.wantKept {
				t.Errorf("crossProjectKept = %d, want %d", got, tc.wantKept)
			}
			if got < 0 {
				t.Errorf("crossProjectKept = %d, must never be negative", got)
			}
			// The report and the "(re-run with --promote-globals)" follow-up both
			// read this count, so the report's component is asserted against the
			// same number runReflect gates the follow-up on.
			summary := appliedSummary(map[string]int{}, tc.cands, 0, tc.dropped, false)
			if want := fmt.Sprintf(", %d cross-project candidates kept project-scoped", tc.wantKept); !strings.HasSuffix(summary, want) {
				t.Errorf("summary = %q, want it to end with %q", summary, want)
			}
		})
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

	_, _, _, promoted, kept, _, _, _, _, _, err := applyReflection(ctx, store, "proj", projectMemories("a consolidated project fact"), globals("prefer running the linter before pushing"), "", false, nil, nil)

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
		f := &fakeReflectionApplier{resultProjectDropped: memory.EmissionDrops{Changed: 2}}
		keptProject, keptGlobal, _, _, _, projectDropped, globalDropped, _, _, applied, err := applyReflection(
			context.Background(), f, "p1", projectMemories("a", "b"), nil, "", false, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if applied {
			t.Error("applied = true, want false: the store refused every emission, so nothing was written")
		}
		if projectDropped.Changed != 2 || globalDropped.Changed != 0 {
			t.Errorf("drops = %d/%d, want 2/0 to travel to the summary", projectDropped.Changed, globalDropped.Changed)
		}
		if len(keptProject) != 2 {
			t.Errorf("keptProject = %d rows, want 2: the reporting set is still the caller's post-credential-drop set", len(keptProject))
		}
		if len(keptGlobal) != 0 {
			t.Errorf("keptGlobal = %d rows, want 0", len(keptGlobal))
		}
	})

	t.Run("a partial refusal stays applied with the count", func(t *testing.T) {
		f := &fakeReflectionApplier{resultProjectDropped: memory.EmissionDrops{Changed: 1}}
		_, _, preserved, promoted, kept, projectDropped, globalDropped, _, _, applied, err := applyReflection(
			context.Background(), f, "p1", projectMemories("a", "b"), nil, "", false, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if !applied {
			t.Error("applied = false, want true: one of two emissions was written, so the round did write")
		}
		if projectDropped.Changed != 1 || globalDropped.Changed != 0 {
			t.Errorf("drops = %d/%d, want 1/0", projectDropped.Changed, globalDropped.Changed)
		}
		if preserved != nil || promoted != 0 || len(kept) != 0 {
			t.Errorf("result = (%v, %d, %d), want (nil, 0, 0)", preserved, promoted, len(kept))
		}
	})

	t.Run("an all-dropped global half alone is applied=false", func(t *testing.T) {
		f := &fakeReflectionApplier{resultGlobalDropped: memory.EmissionDrops{Changed: 1}}
		_, _, _, _, _, projectDropped, globalDropped, _, _, applied, err := applyReflection(
			context.Background(), f, "p1", nil, globals("g"), "", true, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if applied {
			t.Error("applied = true, want false: the only emission was a global candidate the store refused")
		}
		if projectDropped.Changed != 0 || globalDropped.Changed != 1 {
			t.Errorf("drops = %d/%d, want 0/1", projectDropped.Changed, globalDropped.Changed)
		}
	})

	// The should-fix for this test: applied keyed on the drop count also hid an
	// all-dropped round that still changed the corpus. Refusing every emission
	// does not stop the replace from deleting the unclaimed candidates beside
	// the refused claims, and a round that deleted rows took a snapshot the
	// operator can restore — so it must report as applied: "Applied: 0
	// memories consolidated" with the restore hint, never "nothing was written
	// this round".
	t.Run("an all-dropped round that still deletes rows is applied with the delete count", func(t *testing.T) {
		f := &fakeReflectionApplier{resultProjectDropped: memory.EmissionDrops{Changed: 2}, resultDeleted: 1}
		keptProject, keptGlobal, _, _, _, projectDropped, globalDropped, deleted, appliedByCategory, applied, err := applyReflection(
			context.Background(), f, "p1", projectMemories("a", "b"), nil, "", false, nil, nil)
		if err != nil {
			t.Fatalf("applyReflection: %v", err)
		}
		if !applied {
			t.Error("applied = false, want true: the store deleted an unclaimed candidate, so the round changed the corpus and its snapshot must be offered for restore")
		}
		if projectDropped.Changed != 2 || globalDropped.Changed != 0 {
			t.Errorf("drops = %d/%d, want 2/0: the refusal is still reported", projectDropped.Changed, globalDropped.Changed)
		}
		if deleted != 1 {
			t.Errorf("deleted = %d, want 1: the number of rows the replace removed must travel to the caller", deleted)
		}
		if len(appliedByCategory) != 0 {
			t.Errorf("appliedByCategory = %v, want empty: no emission landed, so the summary total is zero and the parts are empty", appliedByCategory)
		}
		// runReflect prints the summary branch because applied is true and the
		// reporting set is non-empty: the report admits the round wrote nothing
		// but replaced the corpus (restore hint offered), instead of the
		// !applied branch's "left exactly as they are".
		if len(keptProject) != 2 || len(keptGlobal) != 0 {
			t.Errorf("kept sets = %d/%d, want 2/0", len(keptProject), len(keptGlobal))
		}
		if got := appliedSummary(appliedByCategory, nil, 0, 0, false); got != "0 memories consolidated" {
			t.Errorf("report line = %q, want %q: the summary must not hang a category on a dropped emission", got, "0 memories consolidated")
		}
		if got := restoreHint(0); got != "(use --restore to undo)" {
			t.Errorf("restore hint = %q, want %q for a round whose snapshot is offering the deleted rows", got, "(use --restore to undo)")
		}
	})
}

// TestApplyReflectionAllDroppedWithOmittedRowReportsReplaceAndRestore drives
// the should-fix through a REAL store, with the reviewer's exact scenario: a
// kept row whose source the #1051 guard refuses — the source's metadata moved
// while the harness was answering, so the emission is dropped — BESIDE an
// omitted row that no emission claims. The drop count alone says "all refused,
// nothing written", but the replace still deletes the omitted row, so the round
// changed the corpus and its snapshot must be offered for restore: applied
// stays true, the store's delete count travels, the summary admits zero writes
// with NO category hung on a dropped emission, and the restore hint is offered.
func TestApplyReflectionAllDroppedWithOmittedRowReportsReplaceAndRestore(t *testing.T) {
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
	claimed, err := store.Create(ctx, "p1", memory.Memory{
		Category: "gotcha", Content: "the claimed row", Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create claimed row: %v", err)
	}
	omitted, err := store.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "the omitted row", Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create omitted row: %v", err)
	}

	// The corpus runReflect hands the harness: every replaceable row as read.
	rows, err := store.GetByIDs(ctx, []string{claimed, omitted})
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

	// The harness answered, and a concurrent save reweighted the claimed row in
	// between: its write clock moved, so the keep below claims a row that is no
	// longer the one the pass was shown — the guard refuses it.
	reweighted := float32(0.95)
	if err := store.UpdateMemory(ctx, "p1", claimed, nil, nil, &reweighted, nil); err != nil {
		t.Fatalf("UpdateMemory claimed row: %v", err)
	}

	keptProject, keptGlobal, preserved, promoted, keptMems, projectDropped, globalDropped, deleted, appliedByCategory, applied, err := applyReflection(
		ctx, store, "p1", []reflection.ReflectMemory{{Category: "gotcha", Content: "the claimed row"}}, nil, "", false, nil, fed)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	if !applied {
		t.Error("applied = false, want true: the replace deleted the omitted row, so the round changed the corpus and its snapshot must be offered for restore")
	}
	if projectDropped.Changed != 1 || globalDropped.Changed != 0 {
		t.Errorf("drops = %d/%d, want 1/0: only the claimed emission was refused", projectDropped.Changed, globalDropped.Changed)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1: the omitted row is the one stored row nothing claimed", deleted)
	}
	if len(appliedByCategory) != 0 {
		t.Errorf("appliedByCategory = %v, want empty: the refused emission landed nowhere", appliedByCategory)
	}
	if promoted != 0 || len(keptMems) != 0 {
		t.Errorf("promoted/kept = %d/%d, want 0/0", promoted, len(keptMems))
	}
	// The refused emission's source survives (concurrent, preserved), and the
	// omitted row is gone — the store-observable half of the report.
	if len(preserved) != 1 || preserved[0] != claimed {
		t.Errorf("preserved = %v, want the claimed row %q kept", preserved, claimed)
	}
	after, err := store.GetByIDs(ctx, []string{omitted})
	if err != nil {
		t.Fatalf("GetByIDs after apply: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("the omitted row survived the all-dropped replace — it must be the deleted count's row")
	}
	// The user-facing half: runReflect prints the summary branch (applied &&
	// the reporting set is non-empty), whose report and restore hint are the
	// exact strings a captured run would show.
	if len(keptProject) != 1 {
		t.Errorf("keptProject = %d rows, want 1: the reporting set still carries the post-drop emission", len(keptProject))
	}
	if len(keptGlobal) != 0 {
		t.Errorf("keptGlobal = %d rows, want 0", len(keptGlobal))
	}
	report := appliedSummary(appliedByCategory, keptGlobal, promoted, 0, false)
	if report != "0 memories consolidated" {
		t.Errorf("report = %q, want %q — nothing landed, so no category may appear next to the zero total", report, "0 memories consolidated")
	}
	if hint := restoreHint(promoted); hint != "(use --restore to undo)" {
		t.Errorf("restore hint = %q, want %q — an all-dropped round that deleted rows took a snapshot worth offering", hint, "(use --restore to undo)")
	}
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
	keptProject, keptGlobal, _, promoted, keptMems, _, globalDropped, deleted, byCategory, applied, err := applyReflection(
		ctx, store, "p1", projectMems, nil, "", false, nil, fed)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
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
