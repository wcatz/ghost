package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// newValiditySession returns a server over a store with three projects: `vproj`
// (the one the validity tests save into), `bare` (registered, no memories — the
// genuinely-empty case) and the fixture's own `pcgold`.
//
// `bare`'s path is a real, EMPTY directory rather than a made-up one, because the
// tool's first-contact import fires on a project with zero memories and reads
// that path. Pointing it at a directory that does not exist would test the
// import's error path instead of the branch under test.
func newValiditySession(t *testing.T) (*Server, *mcp.ClientSession) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject vproj: %v", err)
	}
	if err := st.EnsureProject(ctx, "bare", t.TempDir(), "bare"); err != nil {
		t.Fatalf("EnsureProject bare: %v", err)
	}
	srv := New(st, logger, "test")
	return srv, connectedClient(t, srv)
}

// saveValidityRow saves one memory through the real ghost_memory_save tool with
// the validity arguments a caller would use, and returns its id.
//
// The tool rather than a direct INSERT on purpose: the write side is the other
// half of this contract, and a fixture that wrote `valid_until` in SQL would be
// testing a row the callers of this surface cannot produce.
func saveValidityRow(t *testing.T, session *mcp.ClientSession, content string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"project_id": "vproj", "content": content, "category": "fact"}
	for k, v := range extra {
		args[k] = v
	}
	res := callTool(t, session, "ghost_memory_save", args)
	if res.IsError {
		t.Fatalf("save %q: %s", content, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("save %q carried no memory id: %q", content, resultText(res))
	}
	return id
}

// TestTheProjectContextDropsRowsWhoseWindowHasClosed is the ONE behaviour this
// migration changes on this surface, pinned by its own fixture rather than by the
// goldens beside it.
//
// The goldens cannot carry it: their fixture seeds no validity column, so a store
// whose rows carry a closed window renders identically before and after. That is
// the same reason the session-start stack pinned its inherited validity change
// separately, and the same reason neither golden was re-recorded to accommodate
// it — a re-recorded baseline is a diff record, and a diff record is not a proof.
//
// What changes: the loader filtered only `resolved_at IS NULL`, and the renderer
// MARKED a closed window `expired` rather than dropping it, so a memory the store
// itself says has retired was offered as a current fact. `assemble.Run` runs
// stage 2 unconditionally and there is no knob to decline it.
//
// All five verdicts are asserted, not only the two that change. A test that pins
// `expired` and `future` and says nothing about the other three passes against an
// implementation that drops the wrong rows — the `unverified` one in particular,
// which is KEPT, because `verified_at` is a flag and not a predicate.
func TestTheProjectContextDropsRowsWhoseWindowHasClosed(t *testing.T) {
	_, session := newValiditySession(t)

	live := saveValidityRow(t, session, "vproj: a durable fact about the project", nil)
	openWindow := saveValidityRow(t, session, "vproj: a policy with an open window",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31", "verified": true})
	unverified := saveValidityRow(t, session, "vproj: a claim with a window nobody checked",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31"})
	expired := saveValidityRow(t, session, "vproj: a migration that has been rolled back",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})
	future := saveValidityRow(t, session, "vproj: a policy that starts next year",
		map[string]any{"valid_from": "2999-01-01", "valid_until": "2999-12-31"})

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))

	for _, present := range []string{live, openWindow, unverified} {
		if !strings.Contains(out, present) {
			t.Errorf("%s is missing from the block; a row inside its window must survive:\n%s", present, out)
		}
	}
	// The unverified row is KEPT and MARKED. Dropping it would be the second
	// plausible-looking implementation, and it is wrong: nobody re-checked the
	// claim, which is not the same as the claim being wrong.
	if !strings.Contains(out, "unverified") {
		t.Errorf("a live row nobody re-verified lost its [unverified] marker, so the state is no longer reported:\n%s", out)
	}
	for _, absent := range []string{expired, future} {
		if strings.Contains(out, absent) {
			t.Errorf("%s reached the block; a row whose validity window has closed, or has not opened, is not a current claim:\n%s", absent, out)
		}
	}
	// And the retired rows are not merely absent: a block that rendered them with
	// a marker would satisfy the id check above, so the verdict wording is
	// checked too. Nothing in the block may still describe a row stage 2 dropped.
	for _, marker := range []string{"expired", "not yet valid"} {
		if strings.Contains(out, marker) {
			t.Errorf("the block still carries the %q verdict for a row stage 2 dropped, so the row reached the renderer:\n%s", marker, out)
		}
	}
}

// TestTheProjectContextEmptyBlockSaysRowsWereExcludedRatherThanThatNothingWasSaved
// is the consequence of the change above, and the reason it is a separate test.
//
// The tool's empty branch is a CENSUS — "nothing has been saved for it" — and it
// was written for a loader that only ever lost rows to its own cap. Stage 2
// introduces a second way for the section to be empty, and on a project whose
// every memory has retired the census becomes a lie: Ghost would report that
// nothing was ever saved about a project it holds a full history for.
//
// So the census is gated on the verdict. `no_memories` — the window came back
// empty — keeps it, because that is the one reason that may describe absence; a
// reason meaning rows were FOUND and withheld renders a sentence that says so and
// names the surface that still shows them. Asserted in BOTH directions: the census
// must still appear for a genuinely empty project, or the fix has replaced a lie
// with a silence and a caller can no longer tell the two cases apart.
func TestTheProjectContextEmptyBlockSaysRowsWereExcludedRatherThanThatNothingWasSaved(t *testing.T) {
	_, session := newValiditySession(t)

	// A project whose only memory has retired.
	saveValidityRow(t, session, "vproj: the only memory, and it is retired",
		map[string]any{"valid_until": "2021-01-01"})

	withheld := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if strings.Contains(withheld, "nothing has been saved") {
		t.Errorf("a project whose every memory has retired is reported as never having been saved for:\n%s", withheld)
	}
	if !strings.Contains(withheld, "out of date") {
		t.Errorf("the empty block does not say WHY it is empty, so a reader cannot tell a retired project from an empty one:\n%s", withheld)
	}

	// A project that genuinely has nothing: the census is the honest reading here
	// and it has to survive the change.
	empty := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(empty, "nothing has been saved") {
		t.Errorf("a registered project with no memories no longer says so; an empty window is the one case that may describe absence:\n%s", empty)
	}
}

// TestTheProjectContextSkipsTheNearDuplicateReorderUnderTheCap is a parity
// property the goldens CANNOT see, and it is the one this surface's policy most
// easily gets wrong.
//
// GetTopMemories ran its near-duplicate demotion only when the window was WIDER
// THAN THE CAP, because a demotion is a REORDER: on a selected set that fits
// entirely under the cap it can only shuffle rows the answer already shows in
// full. The passive policy has the same gate (`DemoteOnlyWhenOverCap`), and
// leaving it off is the kind of change that is invisible on any store with more
// memories than the limit — which is every store a reviewer would try.
//
// So the fixture is built the other way round: three rows against a cap of 20,
// and the pair arranged so the demotion has something to REORDER rather than
// merely confirm. `nearDuplicatePenaltyRows` gives the edge to whichever member
// ranks LOWER, so a pair of the top two rows penalises the second of them — and a
// penalised row is pushed behind every unpenalised one, which on three rows means
// the second row swaps with the third. Flip the flag and the swap happens; leave
// it and the plain score order stands, which is the shipped behaviour.
//
// The first version of this fixture paired the bottom two rows, where the loser is
// already last and the demotion is a no-op, and the second saved its rows through
// ghost_memory_save, whose linker auto-linked all three and penalised two of them
// — so a stable sort left the order alone and the test passed with the gate
// removed. Both are the same trap the session-start stack hit with a fixture that
// built no edge at all, and the mutation is what found it. The rows are written in
// SQL here for the same reason the golden fixture writes its own: a save is a
// LINKING opportunity, and a test about one edge cannot accept edges nobody asked
// for.
func TestTheProjectContextSkipsTheNearDuplicateReorderUnderTheCap(t *testing.T) {
	srv, session := newValiditySession(t)
	st, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("store is a %T, not a *memory.Store", srv.store)
	}
	for _, r := range []struct {
		id, content string
		imp         float32
	}{
		{"gatetop", "zeppelin", 0.9},
		{"gatemid", "quicksilver", 0.6},
		{"gatelow", "brass tacks", 0.5},
	} {
		if _, err := st.CreateWithIDFromCorpus(context.Background(), "vproj", r.id, memory.Memory{
			Category: "preference", Content: r.content, Source: "manual", Importance: r.imp,
		}); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	// A and B are the pair; B is the one that loses, because it ranks second.
	if err := st.CreateLink(context.Background(), "gatetop", "gatemid", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	loser, third := positionOf(out, "gatemid"), positionOf(out, "gatelow")
	if loser < 0 || third < 0 {
		t.Fatalf("both rows must be in the block; loser=%d third=%d\n%s", loser, third, out)
	}
	if loser > third {
		t.Errorf("the near-duplicate loser gatemid was pushed behind the unpaired row gatelow on a 3-row set under a "+
			"20-cap; the demotion is a REORDER, and on a set that fits under the cap the shipped loader skipped it\n%s", out)
	}
}

// positionOf is the index of id's first occurrence in backticks, or -1. The ids
// Ghost mints are unique, so a substring search cannot collide with content.
func positionOf(text, id string) int { return strings.Index(text, "`"+id+"`") }

// TestProjectContextLoadDoesNotScaleWithStoreSize is the bounded-window check, in
// the shape of TestSessionStartLoadDoesNotScaleWithStoreSize.
//
// It measures the SHIPPED surface — buildProjectContext, the same function before
// and after the migration — so the before and after numbers in the PR body come
// out of one test rather than two, and so a restatement of the policies here
// cannot leave the measurement describing code the surface no longer runs.
//
// A growth RATIO and not a duration ceiling: a wall-clock bound on a shared laptop
// is a flake, while "ten times the rows, nowhere near ten times the work" is a
// property of the access path. It is a backstop and not a detector, and the
// comment says so rather than over-claiming: at this size a whole-store read would
// plausibly read 2-4x rather than 10x, so the timing cannot see it. The assertion
// with teeth is the admitted count, which is the same bug stated directly — and the
// structural guarantee beside it needs no clock at all, because both seams refuse
// a passive slice stating no OverFetch, so an unbounded fetch cannot be expressed.
func TestProjectContextLoadDoesNotScaleWithStoreSize(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement: skipped in -short")
	}
	run := func(n int) (time.Duration, int) {
		srv := perfProjectContextStore(t, n)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := srv.buildProjectContext(ctx, "pcperf"); err != nil {
				t.Fatalf("buildProjectContext: %v", err)
			}
		}
		var total time.Duration
		admitted := 0
		for i := 0; i < 20; i++ {
			start := time.Now()
			text, err := srv.buildProjectContext(ctx, "pcperf")
			total += time.Since(start)
			if err != nil {
				t.Fatalf("buildProjectContext: %v", err)
			}
			admitted = strings.Count(text, "\n- [")
		}
		return total / 20, admitted
	}
	small, smallAdmitted := run(100)
	large, largeAdmitted := run(1000)
	t.Logf("project context: 100 memories %s (%d lines), 1000 memories %s (%d lines), ratio %.2fx for 10x the rows",
		small, smallAdmitted, large, largeAdmitted, float64(large)/float64(small))
	if small <= 0 {
		t.Fatalf("implausible measurement at 100 memories: %s", small)
	}
	// The caps are the surface's contract, so an admitted count that grows with
	// the store is the bug this test exists for, asserted directly rather than
	// inferred from a clock.
	const caps = 20 + 15
	if smallAdmitted > caps || largeAdmitted > caps {
		t.Errorf("the block carried %d (100 memories) and %d (1000) lines; the caps bound it at %d, so the "+
			"window is no longer bounded and the read is a store scan", smallAdmitted, largeAdmitted, caps)
	}
	if ratio := float64(large) / float64(small); ratio > 5 {
		t.Errorf("the project-context load grew %.2fx for a 10x larger store, which is the whole-store read this "+
			"path must not do (100 memories %s, 1000 memories %s)", ratio, small, large)
	}
}

// perfProjectContextStore seeds n project memories plus the globals a real store
// has, and returns a Server over it. Direct SQL rather than the save tool: the
// point of the measurement is the READ, and a thousand harness round-trips would
// dominate it.
func perfProjectContextStore(t *testing.T, n int) *Server {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	// `_global` is created by seeding, not by EnsureProject, and the global rows
	// below carry a foreign key onto it.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "pcperf", t.TempDir(), "pcperf"); err != nil {
		t.Fatalf("EnsureProject pcperf: %v", err)
	}
	// 120 globals, which is the shape the session-start baselines use: a store
	// whose cross-project rows are a real fraction of its own is where a bucket
	// that stopped honouring its over-fetch shows up first.
	for i := 0; i < n+120; i++ {
		project := "pcperf"
		if i >= n {
			project = memory.GlobalProjectID
		}
		id := "perf" + twoDigits((i/100)%100) + twoDigits((i/10)%10) + twoDigits(i%10)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, tags, created_at, updated_at)
		                       VALUES (?, ?, 'preference', ?, 'manual', 0.7, '[]', ?, ?)`,
			id, project, "performance row "+id, "2026-01-01 00:00:00", "2026-01-01 00:00:00"); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	return New(st, logger, "test")
}
