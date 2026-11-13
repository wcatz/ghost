package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// #1745: the consolidation window's own stamp and a memory's created_at are
// both second-precision, so a memory saved in the same second the pass started
// reads as "saved during the round trip" and every proposal that claims it is
// refused. The refusal is deliberate (it errs toward keeping a row rather than
// toward writing over one the pass cannot prove it read), but the report said
// a memory "changed or was deleted while consolidation ran", and nothing
// changed. These tests pin the two halves of the repair: the store counts the
// same-second refusals apart from the ones whose row really moved, and the
// command says which is which.

// sameSecondFixture is one memory the pass read, and the corpus entry and the
// proposal that go with it: a keep that restates the row byte for byte, which
// claims the fed row carrying that text.
type sameSecondFixture struct {
	id       string
	stored   memory.Memory
	fed      []memory.CorpusRow
	proposal []reflection.ReflectMemory
}

// newSameSecondFixture seeds the row and then reads the corpus the way a pass
// does: after the row is in the store, and after whatever prepare did to it.
// The order matters — the corpus is what the pass read, so a fixture that
// moved the row afterwards would be comparing the corpus against a row the
// pass never saw.
func newSameSecondFixture(t *testing.T, s *memory.Store, project, content string, prepare func(t *testing.T, id string)) sameSecondFixture {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, project, memory.Memory{
		Category: "fact", Content: content, Source: "mcp", Importance: 0.6, Tags: []string{"deploy"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prepare != nil {
		prepare(t, id)
	}
	stored := mustMemory(t, s, id)
	return sameSecondFixture{
		id:     id,
		stored: stored,
		fed: []memory.CorpusRow{{
			ID: stored.ID, Content: stored.Content, UpdatedAt: stored.UpdatedAt,
			Category: stored.Category, Importance: stored.Importance, Tags: stored.Tags,
		}},
		proposal: []reflection.ReflectMemory{{
			Category: "fact", Content: stored.Content, Importance: stored.Importance, Tags: stored.Tags,
		}},
	}
}

// TestReflectASameSecondSaveIsHeldBackAndCountedApartFromAConcurrentChange is
// the case the issue reports: the row was written in the same second the pass
// started, so the window's fail-safe comparison marks it concurrent and the
// proposal over it is refused. The refusal stands — nothing is written and the
// row is untouched — but it is counted as a same-second refusal, not as a
// change that happened while the harness was answering.
func TestReflectASameSecondSaveIsHeldBackAndCountedApartFromAConcurrentChange(t *testing.T) {
	ctx := context.Background()
	s, _ := reflectReuseStore(t)
	const project = "p1"

	fixture := newSameSecondFixture(t, s, project, "the deploy host is alpha", nil)

	// The window stamp IS the row's own second: the row was written in the
	// second the pass started, which is the case under test.
	res, err := applyReflection(
		ctx, s, project, fixture.proposal, nil, fixture.stored.CreatedAt, false, nil, fixture.fed)
	preserved := res.Preserved
	projectDropped := res.ProjectDropped
	globalDropped := res.GlobalDropped
	deleted := res.Deleted
	applied := res.Applied
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	if projectDropped.SameSecond != 1 {
		t.Errorf("projectDropped.SameSecond = %d, want 1: the refusal is a same-second one", projectDropped.SameSecond)
	}
	if projectDropped.Changed != 0 {
		t.Errorf("projectDropped.Changed = %d, want 0: no row changed while the pass ran", projectDropped.Changed)
	}
	if globalDropped.SameSecond != 0 || globalDropped.Changed != 0 {
		t.Errorf("globalDropped = %+v, want no drops: the proposal was the project half's", globalDropped)
	}
	if projectDropped.Total() != 1 {
		t.Errorf("projectDropped.Total() = %d, want 1: a held-back proposal is still a refused one", projectDropped.Total())
	}

	// The fail-safe is unchanged: what was refused before is still refused, and
	// the row it claimed is still exactly as it was.
	if applied {
		t.Error("the round reports itself as applied, but every proposal was refused")
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0: a refused claim takes nothing", deleted)
	}
	after := mustMemory(t, s, fixture.id)
	if after.Content != fixture.stored.Content || after.UpdatedAt != fixture.stored.UpdatedAt {
		t.Errorf("the row = %q (updated %q), want %q (updated %q): a refused claim overwrites nothing",
			after.Content, after.UpdatedAt, fixture.stored.Content, fixture.stored.UpdatedAt)
	}
	if len(preserved) != 1 || preserved[0] != fixture.id {
		t.Errorf("preserved = %v, want [%s]: the row is still live and a later pass has to merge it", preserved, fixture.id)
	}
}

// TestRunReflectReportsASameSecondSaveAsHeldBack drives the real command over
// a memory saved in the same second the pass starts: the window's fail-safe
// comparison refuses the proposal over it, and the command says the proposal
// was held back rather than reporting a change that never happened.
func TestRunReflectReportsASameSecondSaveAsHeldBack(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	const project = "samewin"
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// The window's stamp and a row's created_at are both second-precision,
	// so the case under test pins the window to the row's own second; a row
	// written in an earlier second would be older than the window and its
	// proposal would apply instead.
	s := openReflectStore(t, dataHome, project)
	rowID, err := s.Create(ctx, project, memory.Memory{
		Category: "fact", Content: "the relay is reached over the vpn bastion",
		Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create the row: %v", err)
	}
	// Pin the consolidation window's stamp to the row's own second so the
	// pass cannot land in a later one on a slow runner: both are
	// second-precision, and equality is the case under test. The override
	// is restored on cleanup. This test must not call t.Parallel.
	seeded := mustMemory(t, s, rowID)
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	orig := reflectWindowStamp
	reflectWindowStamp = func(context.Context, *memory.Store) (string, error) {
		return seeded.CreatedAt, nil
	}
	t.Cleanup(func() { reflectWindowStamp = orig })

	report := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply")
	})
	t.Logf("report:\n%s", report)

	if strings.Contains(report, "memories consolidated") {
		t.Fatalf("the pass started in a later second than the row, so the case was not exercised:\n%s", report)
	}
	if !strings.Contains(report, "were held back because their memories were written in the same second this pass started") {
		t.Errorf("the round does not report the same-second refusal:\n%s", report)
	}
	if strings.Contains(report, "changed or was deleted") {
		t.Errorf("the round reports a change that never happened:\n%s", report)
	}

	// The refusal stands: the row is untouched, and no skip fingerprint is
	// recorded over a round that wrote nothing.
	after := openReflectStore(t, dataHome, project)
	rows, err := after.GetByIDs(ctx, []string{rowID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the row is gone (%d rows): a refused claim takes nothing", len(rows))
	}
	if rows[0].Content != "the relay is reached over the vpn bastion" {
		t.Errorf("the row = %q, want it untouched", rows[0].Content)
	}
	if got := readReflectSignature(t, dataHome); got != "" {
		t.Errorf("a skip fingerprint was recorded over a round that wrote nothing: %q", got)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
