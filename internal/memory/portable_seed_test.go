package memory

import (
	"context"
	"testing"
)

// TestPortableMemoriesLeavesOutGhostsOwnSeeds: the artifact has to survive a
// round trip into a store that Ghost has already bootstrapped, and the _global
// builtin seeds are the rows that cannot.
//
// SeedGlobalMemories writes them under the schema's default id,
// hex(randomblob(16)), which is per-install — so the artifact's copy never
// equals the destination's own, and every importer dedups by id alone. The
// result is a second row with byte-identical content: with --trust-provenance a
// second pinned builtin copy of Ghost's shipped rule, surfaced to every session
// as a rule it already has; by default an onboarding copy that has silently lost
// the "this is Ghost's own" label, which is the exact downgrade the provenance
// policy exists to prevent. Nothing repairs it — SeedGlobalMemories skips by
// content, and no path deletes memories — so the duplicate is permanent.
//
// Excluding them at the export is the cheap side of the fix: it keeps the import
// rule simple (dedup by id) and matches what re-seeding already does, since
// Ghost writes exactly these rows on every open by content.
func TestPortableMemoriesLeavesOutGhostsOwnSeeds(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	// A real memory, and one a user could plausibly file under _global, so the
	// exclusion is on the seed specifically and not on the project.
	if _, err := store.Create(ctx, "p1", Memory{Category: "fact", Content: "project knowledge", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(ctx, "_global", Memory{Category: "fact", Content: "a global note of the user's own", Source: "manual"}); err != nil {
		t.Fatalf("Create(_global): %v", err)
	}

	got, err := store.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	var seeds int
	for _, m := range got {
		if m.ProjectID == "_global" && m.Source == "builtin" {
			seeds++
		}
	}
	if seeds != 0 {
		t.Errorf("the artifact carries %d _global builtin seed row(s); re-seeding writes those by content on every open, so a copy can only ever be a permanent duplicate", seeds)
	}
	// And the exclusion is narrow: a _global row that is not a seed is the
	// user's own material and has no other copy.
	var globalNotes int
	for _, m := range got {
		if m.ProjectID == "_global" && m.Source != "builtin" {
			globalNotes++
		}
	}
	if globalNotes != 1 {
		t.Errorf("the artifact carries %d non-seed _global row(s), want the user's own 1 — the exclusion must be on the seed, not the project", globalNotes)
	}
	// And the project's own memories are untouched.
	var projectRows int
	for _, m := range got {
		if m.ProjectID == "p1" {
			projectRows++
		}
	}
	if projectRows != 1 {
		t.Errorf("the artifact carries %d rows for p1, want 1", projectRows)
	}
}

// TestImportingAnotherInstallsSeedsDoesNotDuplicateThem is the end-to-end
// statement of the same property, through a real export and a real import into
// a store that has already been seeded — the case the round-trip tests miss
// because they import into a store that was never seeded.
func TestImportingAnotherInstallsSeedsDoesNotDuplicateThem(t *testing.T) {
	ctx := context.Background()
	// The "other machine": a bootstrapped store with its own seed row, under
	// its own random id.
	src := portableTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := src.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	// Its own knowledge, so the import has something to carry that is not the
	// seed — otherwise the exclusion leaves the source with nothing at all and
	// the duplicate the test is about never gets its chance.
	if _, err := src.Create(ctx, "p1", Memory{Category: "fact", Content: "the other install's knowledge", Source: "mcp"}); err != nil {
		t.Fatalf("Create(src): %v", err)
	}
	// This machine, also already bootstrapped, with its own seed row.
	dst := portableTestStore(t)
	// The project exists here too, as it would after a bootstrap that had
	// recorded this checkout. This is a store-level test and calls ImportMemory
	// directly, so the pre-write project check is live and the project has to be
	// there — the portable layer is what would otherwise create it.
	if err := dst.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject(dst): %v", err)
	}
	if err := dst.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories(dst): %v", err)
	}

	var before int
	if err := dst.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = '_global' AND source = 'builtin'`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1 {
		t.Fatalf("the destination holds %d seed rows before the import, want 1", before)
	}

	// The import, as the store would be handed it: the other's portable rows.
	incoming, err := src.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(incoming) == 0 {
		t.Fatal("the source carries no portable memories, so this tests nothing")
	}
	created := 0
	for _, m := range incoming {
		ok, _, _, err := dst.ImportMemory(ctx, m, ImportOptions{Apply: true, TrustProvenance: true})
		if err != nil {
			t.Fatalf("ImportMemory(%s): %v", m.ID, err)
		}
		if ok {
			created++
		}
	}

	var after int
	if err := dst.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = '_global' AND source = 'builtin'`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("the destination holds %d seed rows after the import, want the %d it started with — Ghost ships this row by content, so a second copy is a permanent duplicate",
			after, before)
	}
	// The row it did import is the other install's project knowledge, so the
	// test is not passing by dropping everything.
	var p1 int
	if err := dst.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE project_id = 'p1'`).Scan(&p1); err != nil {
		t.Fatalf("count p1: %v", err)
	}
	if p1 != 1 {
		t.Errorf("the destination holds %d rows for p1, want 1", p1)
	}
	if created == 0 {
		t.Error("nothing was created, so the duplicate the test is about never had a chance to appear")
	}
}

func TestPortableMemoriesSeedExclusionDoesNotLeakIntoAProjectFilter(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	// A filtered export names its own project, so the seed exclusion is not even
	// in reach — but the WHERE clause that carries the filter must still
	// assemble correctly with the exclusion on top of it, which is the easy
	// thing to get wrong.
	got, err := store.PortableMemories(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableMemories(filtered): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a filter for p1 returned %d rows, want 0 — the project has none", len(got))
	}
	if _, err := store.Create(ctx, "p1", Memory{Category: "fact", Content: "one", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err = store.PortableMemories(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableMemories(filtered): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("a filter for p1 returned %d rows, want 1", len(got))
	}
	// Two ids at once, so the multi-placeholder path is exercised too.
	both, err := store.PortableMemories(ctx, []string{"p1", "_global"})
	if err != nil {
		t.Fatalf("PortableMemories(two ids): %v", err)
	}
	for _, m := range both {
		if m.ProjectID == "_global" && m.Source == "builtin" {
			t.Errorf("a two-id filter still returned a seed row (%s)", m.ID)
		}
	}
	_ = both
}
