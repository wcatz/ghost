package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
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
	return validityServerFor(t, newValidityStore(t))
}

// newValidityStore is the store behind newValiditySession, for a test that needs
// to seed rows directly and then read them back through the same handle.
func newValidityStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	// `_global` is created by seeding rather than by EnsureProject, and the global
	// rows in these fixtures carry a foreign key onto it.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject vproj: %v", err)
	}
	if err := st.EnsureProject(ctx, "bare", t.TempDir(), "bare"); err != nil {
		t.Fatalf("EnsureProject bare: %v", err)
	}
	return st
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

// strPtr is the validity-column pointer a `memory.Memory` literal needs, and it is
// here rather than borrowed from another package's test helper because the whole
// point of the fixture using it is that the row is written DIRECTLY, with a closed
// window, rather than through a save the linker would touch.
func strPtr(s string) *string { return &s }

// TestTheProjectContextAnswersAnUnresolvedProjectRatherThanFailing is a
// regression test for a review finding, and the finding was right.
//
// `ResolveProject` returns `("", "", nil)` for a name no `projects` row matches.
// The old loader was handed that empty id and read
// `project_id = ” OR project_id = '_global'`, so it fell through to the
// not-registered sentence when the store held no globals — and, when the store
// DID hold globals, listed them under a `## Memories` heading for a project that
// does not exist. Both are answers; the migration's answer was neither, because
// `validateRequest` refuses a project-context request with no project and the
// tool turned that refusal into an error.
//
// So the unresolved case is answered explicitly, and the same test pins BOTH
// halves of what used to vary: the sentence is the not-registered one whatever
// else the store holds, and it is not an error. An error here is strictly worse
// than the old inconsistency: a caller cannot act on "something went wrong" by
// saving a memory to the project.
func TestTheProjectContextAnswersAnUnresolvedProjectRatherThanFailing(t *testing.T) {
	_, session := newValiditySession(t)
	// Globals in the store, which is the case the old loader answered with a
	// listing rather than the sentence.
	saveValidityRow(t, session, "vproj: a project memory", nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "no-such-project",
	}))
	if !strings.Contains(out, "is not registered with Ghost yet") {
		t.Errorf("an unresolved project did not get the not-registered sentence:\n%s", out)
	}
	// The sentence has to name what the caller ASKED for. `ResolveProject` answers
	// an unknown name with "", so quoting the resolved id produces
	// `Project "" is not registered` — which names nothing the caller can act on,
	// and which reads as though Ghost had a project with an empty name.
	if !strings.Contains(out, `"no-such-project"`) {
		t.Errorf("the not-registered sentence does not name the requested project:\n%s", out)
	}
	if strings.Contains(out, "## Memories") {
		t.Errorf("an unresolved project rendered a memory listing; those rows belong to a different project, and a "+
			"heading naming this one is a claim the block never made:\n%s", out)
	}
}

// TestTheProjectContextServesTheGlobalProjectItself: the other half of the same
// finding.
//
// `IncludeGlobal` is the union `project_id = ? OR project_id = '_global'`, and a
// request whose bucket IS `_global` is not asking for a union — that bucket
// already reads exactly those rows. Setting the flag unconditionally made both
// seams' overlap refusal fire on it (`Bucket == _global` and a policy that
// includes it), so `ghost_project_context` with `project_id: '_global'` was
// REFUSED. It is a supported call: the old loader answered it with the global
// rows, and the `ghost://memories/global` resource is the same listing.
func TestTheProjectContextServesTheGlobalProjectItself(t *testing.T) {
	_, session := newValiditySession(t)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}
	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	if strings.Contains(out, "does not support candidate retrieval") {
		t.Fatalf("a project-context read of _global was refused outright:\n%s", out)
	}
	if !strings.Contains(out, "a cross-project preference") {
		t.Errorf("a project-context read of _global did not list the global rows:\n%s", out)
	}
}

// TestTheProjectContextKeepsTheOriginLabelOnAGlobalRowInAMixedBlock is a parity
// check on a field the goldens cannot see: every row in that fixture is
// `source='manual'`, and a manual row renders NO origin label at all. So the one
// field of the item line the migration could plausibly have broken is the one
// nothing was watching.
//
// The risk is real and specific. `memory.CanonicalOriginSourceForProject` scopes
// its legacy-seed rewrite to the GLOBAL project, so a row's own project id decides
// whether a builtin's shipped sentence is corrected — and a `_global` row read
// through a PROJECT bucket is exactly the shape where the two ids could be
// confused. `Item.Line` reads `i.ProjectID` (the row's own) and the old
// `formatMemories` read `m.ProjectID` (also the row's own), so they must agree;
// this asserts the rendered bytes against the renderer they replaced rather than
// against a string, because the string is the thing under test.
func TestTheProjectContextKeepsTheOriginLabelOnAGlobalRowInAMixedBlock(t *testing.T) {
	st := newValidityStore(t)
	// A global row Ghost itself wrote, which is the only shape that renders a label.
	if _, err := st.CreateWithIDFromCorpus(context.Background(), memory.GlobalProjectID, "gorigin", memory.Memory{
		Category: "preference", Content: "a preference the user typed once", Source: "mcp", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	// The same shipped seed text, once as a GLOBAL row and once as a PROJECT row.
	// `CanonicalOriginSourceForProject` rewrites a `manual` global row carrying
	// that exact sentence to `builtin`, and deliberately does NOT rewrite a project
	// row carrying it — a project row with the shipped words is the user's own
	// material, and misattributing it would both invent an origin and take away
	// the "no agent recorded" marker that says the row is untagged.
	//
	// This is the half that makes the mixed bucket dangerous, and it is why the
	// test is not satisfied by a row that merely HAS a label: the label's
	// correctness depends on the row's OWN project, and a `_global` row read
	// through a project bucket is precisely where that could be lost.
	//
	// The text comes from the store's own seed rather than a literal here, so the
	// test cannot drift from the sentence the correction keys on: if that sentence
	// changed, the precondition below fails instead of the test quietly exercising
	// a string nothing recognises.
	seedText := shippedSeedText(t, st)
	if got := memory.CanonicalOriginSourceForProject(memory.GlobalProjectID, "manual", seedText); got != "builtin" {
		t.Fatalf("fixture precondition: the store no longer rewrites the shipped seed text for a global row (got %q), "+
			"so this test would pass without exercising the scoping at all", got)
	}
	if got := memory.CanonicalOriginSourceForProject("vproj", "manual", seedText); got != "manual" {
		t.Fatalf("fixture precondition: a project row holding the shipped words is no longer left alone (got %q)", got)
	}
	for _, r := range []struct{ id, project string }{
		{"gseed", memory.GlobalProjectID},
		{"pseed", "vproj"},
	} {
		if _, err := st.CreateWithIDFromCorpus(context.Background(), r.project, r.id, memory.Memory{
			Category: "preference", Content: seedText, Source: "manual", Importance: 0.9,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	_, session := validityServerFor(t, st)

	// What the renderer this migration replaced would have produced, row for row.
	// One project at a time, because ListMemories takes a single project and an
	// empty one matches nothing.
	byID := map[string]string{}
	var replaced []memory.Memory
	for _, project := range []string{"vproj", memory.GlobalProjectID} {
		rows, err := st.ListMemories(context.Background(), project, "", "", 10)
		if err != nil {
			t.Fatalf("ListMemories %s: %v", project, err)
		}
		replaced = append(replaced, rows...)
	}
	for _, line := range strings.Split(formatMemories(replaced), "\n") {
		if i := strings.Index(line, "`"); i >= 0 {
			if j := strings.Index(line[i+1:], "`"); j >= 0 {
				byID[line[i+1:i+1+j]] = strings.TrimSpace(line)
			}
		}
	}
	// The seeded row is in there too, and it is not one of the three this test
	// compares; only the three fixture rows need a line.
	for _, id := range []string{"gorigin", "gseed", "pseed"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("fixture precondition: the replaced renderer printed no line for %s:\n%s", id, formatMemories(replaced))
		}
	}

	got := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "vproj",
	}))
	for _, id := range []string{"gorigin", "gseed", "pseed"} {
		want, ok := byID[id]
		if !ok {
			t.Fatalf("fixture precondition: the replaced renderer printed no line for %s", id)
		}
		if !strings.Contains(got, want) {
			t.Errorf("a row rendered differently than the renderer this migration replaced.\n"+
				"%s: the replaced renderer ---\n%s\n--- the assembled block ---\n%s", id, want, got)
		}
	}
	// Spelled out, because the byte comparison above would also pass if BOTH
	// renderers were wrong the same way, and this is the assertion that says which
	// way is right: the global seed is attributed to the builtin, the project copy
	// of the same words is not.
	if !strings.Contains(got, "`gseed` (0.9 source=builtin)") {
		t.Errorf("the global seed row is not attributed to the builtin; its OWN project is what decides that, and a "+
			"global row read through a project bucket is where it can be lost:\n%s", got)
	}
	if !strings.Contains(got, "`pseed` (0.9)") || strings.Contains(got, "`pseed` (0.9 source=builtin)") {
		t.Errorf("a PROJECT row holding the shipped words was attributed to the builtin; it is the user's own material:\n%s", got)
	}
}

// shippedSeedText is the sentence Ghost ships as a global seed, read back from the
// store's own seeding rather than written out here — so the fixture cannot drift
// from the string `CanonicalOriginSourceForProject` keys on.
func shippedSeedText(t *testing.T, st *memory.Store) string {
	t.Helper()
	if err := st.SeedGlobalMemories(context.Background()); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	rows, err := st.ListMemories(context.Background(), memory.GlobalProjectID, "", "", 10)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	for _, m := range rows {
		if m.Source == "builtin" {
			return m.Content
		}
	}
	t.Fatal("the store seeded no builtin global row, so there is no shipped seed text to copy")
	return ""
}

// TestEveryProjectContextSurfaceAnswersAnUnresolvedProject is the second and third
// review findings on this theme, and between them they show why the first fix was
// scoped too narrowly.
//
// `buildProjectContext` is reached by three surfaces, not one: the tool, the
// `ghost://project/{id}/context` resource template and the `recall_project` prompt.
// The first guard was written at the tool, and the other two kept passing the
// resolved id — `""` — straight into `assemble.Run`, which refuses it. So reading
// the resource or the prompt for a project Ghost has never seen became
// `reading project context "": assemble: project context requires a project`: a
// hard break against the base ref, where the same call returned a block, and an
// error quoting an empty id rather than the project the caller named.
//
// Then the fix over-corrected: returning the sentence INSTEAD of the block dropped
// the `## Global (applies to all projects)` section, which the base ref did deliver
// for an unknown name (under the mislabelled `## Memories`, which is the bug being
// removed — but delivered). A first session in a project Ghost has never seen is
// exactly when the cross-project preferences matter, and the server's own
// SessionStart instructions tell the agent to call these surfaces when the
// directory matched nothing and to look for a Global section.
//
// So the answer is the block PLUS the sentence: the section that does not depend
// on a project, under the heading that is true of it, and then the fact that this
// project is unknown.
func TestEveryProjectContextSurfaceAnswersAnUnresolvedProject(t *testing.T) {
	srv, session := newValiditySession(t)
	const wanted = "no-such-project-anywhere"
	// A cross-project row the store holds, so "did the Global section survive?" is
	// answerable rather than vacuously true on an empty store.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}

	// The assertions are LITERALS, not `projectNotRegistered(wanted)`. Comparing a
	// function's output against itself is a tautology: the first version of this
	// test did exactly that, and a mutation that made the function quote the
	// RESOLVED id — which is "" here, so the message read `Project "" is not
	// registered` — survived it. A test that asks a function whether the function
	// is right has no failure to fail.
	assertAnswered := func(surface, out string) {
		t.Helper()
		if !strings.Contains(out, "is not registered with Ghost yet") {
			t.Errorf("%s did not answer the not-registered sentence:\n%s", surface, out)
		}
		if !strings.Contains(out, wanted) {
			t.Errorf("%s did not NAME the project the caller asked for (want %q); quoting the resolved id would say "+
				`Project "" is not registered, which names nothing they can act on:`+"\n%s", surface, wanted, out)
		}
		if strings.Contains(out, "## Memories") {
			t.Errorf("%s rendered a memory listing for a project that does not exist; those rows belong to a "+
				"different project:\n%s", surface, out)
		}
		// The cross-project rows do not depend on the project, and the base ref
		// delivered them for an unknown name. Dropping them is a regression dressed
		// as a fix, and it is the one this test exists to hold.
		if !strings.Contains(out, "## Global (applies to all projects)") {
			t.Errorf("%s dropped the cross-project section for an unknown project; it does not depend on one, and a "+
				"first session in a project Ghost has never seen is when it matters most:\n%s", surface, out)
		}
		if !strings.Contains(out, "a cross-project preference") {
			t.Errorf("%s did not deliver the cross-project row:\n%s", surface, out)
		}
	}

	// The tool.
	assertAnswered("ghost_project_context", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": wanted})))

	// The resource template.
	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://project/" + wanted + "/context",
	})
	if err != nil {
		t.Fatalf("ReadResource context: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("expected 1 resource content, got %d", len(rr.Contents))
	}
	assertAnswered("the context RESOURCE", rr.Contents[0].Text)

	// The recall_project prompt.
	pr, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": wanted},
	})
	if err != nil {
		t.Fatalf("GetPrompt recall_project: %v", err)
	}
	if len(pr.Messages) != 1 {
		t.Fatalf("expected 1 prompt message, got %d", len(pr.Messages))
	}
	tc, ok := pr.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", pr.Messages[0].Content)
	}
	assertAnswered("the recall_project PROMPT", tc.Text)
	_ = srv
}

// TestTheToolRefusesAnAsOfRequestForAnUnresolvedProject is the sixth and seventh
// review findings on this path, and the second one is what the first one got wrong.
//
// Finding six: the unresolved-name guard sat BELOW the tool's `as_of` return, so
// only the present-tense branch was covered — `asOfScopeClause` builds the same
// `(project_id = ? OR project_id = '_global')` union, so `as_of` on an unknown name
// printed the cross-project rows under `## Memories` for a project that does not
// exist, while this PR's own docs said the surfaces skip the project-keyed reads.
//
// My fix moved the guard above the return and made the two branches answer
// IDENTICALLY, which was the second mistake: an `as_of` caller was then handed
// today's rows with no `as_of` note in the payload, so the requested instant was
// never consulted and nothing in the answer said so. That is a caller stating one
// thing and being silently given another.
//
// So the branches differ on purpose. The present-tense call appends the
// cross-project section, which does not depend on a project. The `as_of` call
// refuses and names the instant, because there is no set to show — a past reading
// of a project Ghost has never seen is not a reading of anything. Asserted as a
// DIFFERENCE, because the difference is the contract.
func TestTheToolRefusesAnAsOfRequestForAnUnresolvedProject(t *testing.T) {
	_, session := newValiditySession(t)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}
	const wanted = "no-such-project-as-of"

	current := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": wanted}))
	historical := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": wanted, "as_of": asOfToolFuture,
	}))

	// The present-tense read appends the cross-project rows, because they do not
	// depend on a project and the base ref delivered them.
	if !strings.Contains(current, "## Global (applies to all projects)") ||
		!strings.Contains(current, "a cross-project preference") {
		t.Errorf("the present-tense read dropped the cross-project section:\n%s", current)
	}
	if strings.Contains(current, "## Memories") {
		t.Errorf("the present-tense read rendered a memory listing for a project that does not exist:\n%s", current)
	}

	// The as_of read is a REFUSAL, and it must not carry a single row: handing a
	// caller today's rows for a request about a past instant is the defect, and
	// the cross-project section is the row set most likely to be handed over.
	if !strings.Contains(historical, "is not registered with Ghost yet") {
		t.Errorf("the as_of read did not answer the not-registered sentence:\n%s", historical)
	}
	if !strings.Contains(historical, asOfToolFuture) {
		t.Errorf("the as_of read does not name the instant, so the reader cannot tell the requested instant was never "+
			"consulted:\n%s", historical)
	}
	if strings.Contains(historical, "a cross-project preference") {
		t.Errorf("the as_of read returned a present-tense row; a caller who asked for an instant cannot be handed "+
			"today's rows with nothing in the payload saying so:\n%s", historical)
	}
	if strings.Contains(historical, "## Memories") {
		t.Errorf("the as_of read rendered a memory listing for a project that does not exist:\n%s", historical)
	}
	if current == historical {
		t.Errorf("the two branches answer identically, which was the first fix's mistake: an as_of caller must not be "+
			"handed a present-tense block.\n%s", current)
	}
	// And both still name the project, which is the fact the reader needs either
	// way.
	for _, c := range []struct{ name, out string }{
		{"the current read", current}, {"the as_of read", historical},
	} {
		if !strings.Contains(c.out, wanted) {
			t.Errorf("%s did not name the project the caller asked for:\n%s", c.name, c.out)
		}
	}
}

// TestAnUnresolvedProjectNameDoesNotHideABrokenStore is the seventh finding's third
// point, and it is the one a real store cannot show.
//
// Moving the unresolved-name check above the `as_of` return also moved it above the
// store's `asOfCapableStore` assertion, so on a store that cannot read its own
// history an unregistered project name reported SUCCESS — losing a diagnostic the
// caller may need, and losing it in exactly the case where the answer is already a
// refusal and so looks plausible.
//
// The test needs a store that is not history-capable, which `*memory.Store` never
// is. `provider.MemoryStore` embedded in a struct without `MemoriesAsOf` is
// exactly that: it satisfies the interface by delegation and is not
// `asOfCapableStore`. A real store would have made this assertion vacuous.
func TestAnUnresolvedProjectNameDoesNotHideABrokenStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	blind := connectedClient(t, New(notHistoryCapable{newValidityStore(t)}, logger, "test"))

	// A RESOLVED project, to prove the diagnostic is reachable at all on this store.
	res := callTool(t, blind, "ghost_project_context", map[string]any{
		"project_id": "vproj", "as_of": asOfToolFuture,
	})
	if !res.IsError || !strings.Contains(resultText(res), "cannot read its own history") {
		t.Fatalf("fixture precondition: a store that cannot read history must say so for a resolved project, got %q",
			resultText(res))
	}

	// And an UNRESOLVED one, which is the case the ordering is about: the store's
	// own diagnostic must win over the not-registered sentence, because the caller
	// still does not know whether their store can answer a historical question.
	unres := callTool(t, blind, "ghost_project_context", map[string]any{
		"project_id": "no-such-project-broken-store", "as_of": asOfToolFuture,
	})
	if !unres.IsError || !strings.Contains(resultText(unres), "cannot read its own history") {
		t.Errorf("an unresolved project name hid the store's missing-history diagnostic and reported the "+
			"not-registered sentence instead: %q", resultText(unres))
	}
}

// notHistoryCapable satisfies provider.MemoryStore by delegation and deliberately
// does NOT implement MemoriesAsOf, so it fails the asOfCapableStore assertion.
type notHistoryCapable struct{ provider.MemoryStore }

// TestTheUnresolvedNameHonoursTheCallersLimit is a review BLOCKER, and the
// test that would have caught it.
//
// The unresolved-name branch rendered the cross-project section through
// `projectContextGlobalSection` -> `projectContextGlobals` ->
// `projectContextGlobalBudget`, whose cap was the hard-coded
// `projectContextGlobalsCap`. So the tool returned 15 rows for a `limit: 3`
// request, for `limit: 100`, and for every other value - silently overriding the
// argument it publishes as "Max memories to return". The resource and prompt
// paths went through the same read and got 15 where origin/main returned 20.
//
// The fix makes the cap a parameter. This asserts the row SET as well as the
// count, and it takes the ORACLE from the reader this migration replaced:
// `formatMemories(store.GetTopMemories(ctx, "", limit))` is what origin/main
// rendered for this exact call, so "the same rows in the same order, under a
// heading that is true of them" is a checkable statement rather than a
// re-derivation of the new path's own logic.
//
// A count alone would pass against the wrong rows. A byte-golden cannot be used
// here at all, because the heading and the appended sentence are two of this PR's
// deliberate output changes, and re-recording a baseline to accommodate them would
// turn a parity proof into a diff record - which is what the two goldens exist to
// avoid.
func TestTheUnresolvedNameHonoursTheCallersLimit(t *testing.T) {
	st := newValidityStore(t)
	// 25 globals, so every cap under test binds and `limit: 100` is bounded by the
	// corpus rather than by the argument.
	for i := 0; i < 25; i++ {
		if _, err := st.CreateWithIDFromCorpus(context.Background(), memory.GlobalProjectID,
			"bulkg"+twoDigits(i/10)+twoDigits(i%10), memory.Memory{
				Category: "preference", Content: "bulk global row " + twoDigits(i),
				Source: "manual", Importance: 0.5,
			}); err != nil {
			t.Fatalf("seed global %d: %v", i, err)
		}
	}
	srv, session := validityServerFor(t, st)
	ctx := context.Background()

	rowLines := func(text string) []string {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "- [") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		return lines
	}
	// The oracle: the row lines origin/main's reader produced for this call.
	oracle := func(limit int) []string {
		rows, err := st.GetTopMemories(ctx, "", limit)
		if err != nil {
			t.Fatalf("GetTopMemories with an empty project and limit %d: %v", limit, err)
		}
		return rowLines(formatMemories(rows))
	}
	// report compares against the base reader and, when they differ, prints both
	// sides: "the sets differ" is not a finding a reader can act on, and the count
	// alone would pass against the wrong rows.
	report := func(what string, got, want []string) {
		t.Helper()
		for i := 0; i < len(got) && i < len(want); i++ {
			if got[i] != want[i] {
				t.Errorf("%s: row %d differs from the base reader.\n--- base reader (origin/main) ---\n%s\n"+
					"--- this PR ---\n%s", what, i, strings.Join(want, "\n"), strings.Join(got, "\n"))
				return
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s returned %d rows, the base reader returned %d.\n--- base reader (origin/main) ---\n%s\n"+
				"--- this PR ---\n%s", what, len(got), len(want), strings.Join(want, "\n"), strings.Join(got, "\n"))
		}
	}

	for _, limit := range []int{1, 3, 15, 20, 100} {
		want := oracle(limit)
		out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
			"project_id": "no-such-limited", "limit": limit,
		}))
		report(fmt.Sprintf("ghost_project_context with limit=%d", limit), rowLines(out), want)
	}

	// The resource and the prompt read at 20, which is what
	// `GetTopMemories(ctx, "", 20)` returned for them. Asserted through
	// buildProjectContext, the body both read.
	want := oracle(projectContextMemoriesCap)
	if len(want) != projectContextMemoriesCap {
		t.Fatalf("fixture precondition: the base reader returned %d rows for a %d cap, so the cap is not "+
			"binding and this test would pass without exercising it", len(want), projectContextMemoriesCap)
	}
	text, err := srv.buildProjectContext(ctx, "")
	if err != nil {
		t.Fatalf("buildProjectContext with an empty project: %v", err)
	}
	report("buildProjectContext with an unresolved project", rowLines(text), want)
}

// TestTheUnresolvedProjectBlockStillRendersTheResourceOnItsOwn is the half of the
// finding that a sentence-in-place-of-the-block fix breaks, stated on the resource
// alone: `buildProjectContext` is what the resource and the prompt both read, so
// this asserts that an unresolved project id produces a real block through it and
// not an empty string, which the prompt's own `if text == ""` fallback would then
// have rendered as "No memories or learned context saved yet for this project" —
// naming a project as having nothing saved when it has never been registered.
func TestTheUnresolvedProjectBlockStillRendersTheResourceOnItsOwn(t *testing.T) {
	srv, _ := newValiditySession(t)
	if _, err := srv.store.(*memory.Store).CreateWithIDFromCorpus(
		context.Background(), memory.GlobalProjectID, "lonelyglobal", memory.Memory{
			Category: "preference", Content: "a cross-project preference", Source: "manual", Importance: 0.9,
		}); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	text, err := srv.buildProjectContext(context.Background(), "")
	if err != nil {
		t.Fatalf("buildProjectContext(\"\"): %v", err)
	}
	if text == "" {
		t.Fatal("buildProjectContext returned nothing for an unresolved project, so both its callers render their own " +
			"empty-case text and name a project that was never registered as having nothing saved")
	}
	if !strings.Contains(text, "## Global (applies to all projects)") {
		t.Errorf("no cross-project section in the unresolved block:\n%s", text)
	}
}

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

// TestTheProjectContextAbstentionPromisesNoNoteTheBlockDoesNotCarry is the
// surface half of a review should-fix, and it is the half that matters: the
// sentence is correct inside the assembler and wrong in the bytes the caller
// receives.
//
// `Result.Abstention` is what `projectContextEmptyNote` returns, and that string
// IS the whole tool result and the whole resource body. `Result.Notes` is never
// rendered by either, so the sentence's "The note below breaks the removals down
// per stage" pointed at a breakdown that was not in the payload. Asserted on the
// tool's real output rather than on `res.Abstention`, because the sentence
// reaching a caller and the caller dropping its note are two different failures
// and only the second one is a defect in this package.
func TestTheProjectContextAbstentionPromisesNoNoteTheBlockDoesNotCarry(t *testing.T) {
	_, session := newValiditySession(t)
	saveValidityRow(t, session, "vproj: the only memory, and it is retired",
		map[string]any{"valid_until": "2021-01-01"})

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(out, "withheld as out of date") {
		t.Fatalf("fixture: the answer is no longer the all_invalid abstention, so this test is not exercising it:\n%s", out)
	}
	lower := strings.ToLower(out)
	for _, promise := range []string{"note below", "breaks the removals down", "per stage"} {
		if strings.Contains(lower, promise) {
			t.Errorf("the block promises %q and carries no note: the whole result is the abstention, so an agent "+
				"is sent after a breakdown that is not there:\n%s", promise, out)
		}
	}
	// The half that carries the meaning survives, and it is what makes this an
	// abstention rather than a census: the rows were found.
	if !strings.Contains(lower, "still marked with the window they carry") {
		t.Errorf("the abstention lost the pointer at the surface that still shows the rows:\n%s", out)
	}
}

// seedCrossProjectRow seeds a LIVE `_global` row directly, because `saveValidityRow`
// writes to `vproj` through the save tool and the whole point of these two tests is
// a row that is NOT the requesting project's.
//
// `cmd/ghost/bootstrap.go` seeds the global memories on every real store, so this is
// the normal state of a store and not an edge one — which is why the gate these
// tests are about was invisible in a fixture that seeded no globals at all.
func seedCrossProjectRow(t *testing.T, st *memory.Store, id, content string) {
	t.Helper()
	if _, err := st.CreateWithIDFromCorpus(context.Background(), memory.GlobalProjectID, id, memory.Memory{
		Category: "preference", Content: content, Source: "builtin", Importance: 0.8,
	}); err != nil {
		t.Fatalf("seed _global row %s: %v", id, err)
	}
}

// TestAProjectWhoseOwnRowsAreAllWithheldIsToldSoBesideTheCrossProjectRows is a
// review should-fix, and the gate it finds is the wrong SCOPE.
//
// `projectContextEmptyNote` is consulted only when the WHOLE block is empty. That is
// the right gate for the census and the wrong gate for the exclusion, because what
// makes the block empty of this PROJECT's rows is not the block being empty — and
// `projectContextBudget` sets `IncludeGlobal`, so the block is populated by `_global`
// rows whenever the store holds any, which `cmd/ghost/bootstrap.go` seeds on every
// real store. So on a project whose every memory has retired, the answer was the
// cross-project preferences under a `## Memories` heading and nothing whatever about
// the project's own rows having been withheld. That is strictly LESS than the base
// reader gave: `GetTopMemories` did not filter validity and listed them marked
// `expired`.
//
// The review's suggested gate — `Outcome == OutcomeEmpty && Reason != NoMemories` —
// does not reach this case, which is why the fix is here and not there. With a mixed
// bucket the live global IS an admitted item, so the outcome is `answerable` and the
// reason is empty; nothing above the caller can separate the two populations in one
// bucket, and that is the price of expressing "one cap over the union" as a union
// rather than as two buckets (two buckets at `limit` each would admit twice the rows
// the caller asked for).
//
// So the split happens at the caller, where the requested project is known: if no
// admitted row is this project's, say so. The count is `CountMemories`, which covers
// rows this block dropped for ANY reason — validity, the cap, dedup or resolution —
// so the sentence names no cause and stays true in all of them.
func TestAProjectWhoseOwnRowsAreAllWithheldIsToldSoBesideTheCrossProjectRows(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	saveValidityRow(t, session, "vproj: the only memory, and it is retired",
		map[string]any{"valid_until": "2021-01-01"})
	seedCrossProjectRow(t, st, "liveglobal", "a live cross-project preference")

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(out, "a live cross-project preference") {
		t.Fatalf("fixture: the block is not populated by the cross-project row, so this is not the case the old "+
			"gate missed:\n%s", out)
	}
	if strings.Contains(out, "nothing has been saved") {
		t.Errorf("the block answers a project Ghost holds a memory for with the never-saved census:\n%s", out)
	}
	// The whole sentence, not two fragments of it. An earlier version asserted
	// "Ghost holds 1 memory for this project" and separately that the output
	// mentioned `ghost_memories_list`, and a mutation that swapped one word of the
	// pointer's verb survived it — the assertions together said less than the
	// sentence does. A test that pins prose is testing the prose, and on a surface
	// whose output is prose that is the property.
	const wantNote = "Ghost holds 1 memory for this project and none of it is in the block above. Call " +
		"ghost_memories_list to browse it: a browse is not capped at what fits in a context block, and it " +
		"shows each row's validity window."
	if !strings.Contains(out, wantNote) {
		t.Errorf("the block shows no row of the requested project and does not say so. Before this change the only "+
			"row was the global one, under a `## Memories` heading, which reads as though it were the project's.\n"+
			"wanted this sentence:\n%s\ngot:\n%s", wantNote, out)
	}

	// The RESOURCE and the PROMPT read the same body, and a second call site is a
	// second thing to wire: asserted through buildProjectContext, which both read.
	body, err := srv.buildProjectContext(context.Background(), "vproj")
	if err != nil {
		t.Fatalf("buildProjectContext vproj: %v", err)
	}
	if !strings.Contains(body, "Ghost holds 1 memory for this project") {
		t.Errorf("the resource body tells a caller nothing about its own project's withheld rows:\n%s", body)
	}

	// The control, and it is the half a fix can get wrong in the other direction: a
	// project with a row of its OWN in the block is told nothing, or every project
	// sharing a store with a global would carry a note.
	if _, err := st.CreateWithIDFromCorpus(context.Background(), "bare", "ownrow", memory.Memory{
		Category: "fact", Content: "a live memory of its own", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed bare's own row: %v", err)
	}
	withOwn := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(withOwn, "a live memory of its own") {
		t.Fatalf("fixture: the control project has no row of its own in the block:\n%s", withOwn)
	}
	if strings.Contains(withOwn, "Ghost holds") {
		t.Errorf("a project whose own row IS in the block was told it holds nothing above the block:\n%s", withOwn)
	}

	// `_global` asked for directly IS the project, so there is no gap to report, and
	// the goldens pin its shape byte for byte.
	asGlobal := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "_global"}))
	if strings.Contains(asGlobal, "Ghost holds") {
		t.Errorf("the _global project was told it holds none of its own rows:\n%s", asGlobal)
	}

	// An UNRESOLVED name already gets the not-registered sentence, which says
	// something stronger; two sentences about one absence is one too many.
	unresolved := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "no-such-here"}))
	if strings.Contains(unresolved, "Ghost holds") {
		t.Errorf("an unresolved project name was also given the withheld-rows note, which is about a project that "+
			"exists:\n%s", unresolved)
	}
}

// TestAProjectHoldingNothingSaysSoWhenOnlyCrossProjectRowsAreShown is the other half
// of the same gate, and it is what makes the note total rather than partial.
//
// The census was reachable only on an empty block, so a REGISTERED project holding no
// memories at all, on a store with any global row, was answered with the global row
// under a `## Memories` heading and told nothing — the same misattribution as the
// withheld case, one clause shorter.
//
// This half is a parity CHANGE rather than a parity fix: `GetTopMemories(ctx, "noproj",
// 20)` read `project_id = ? OR project_id = '_global'` and returned exactly this, so
// origin/main said nothing either. It is here because the gate being repaired is "the
// block shows no row of the requested project", and leaving the empty half of that
// unfixed would be the same defect with one fewer word.
func TestAProjectHoldingNothingSaysSoWhenOnlyCrossProjectRowsAreShown(t *testing.T) {
	st := newValidityStore(t)
	_, session := validityServerFor(t, st)
	seedCrossProjectRow(t, st, "liveglobal", "a live cross-project preference")

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(out, "a live cross-project preference") {
		t.Fatalf("fixture: the block is not populated by the cross-project row:\n%s", out)
	}
	if !strings.Contains(out, "Ghost holds no memories for this project") {
		t.Errorf("a registered project with no memories of its own was answered with the cross-project row alone, "+
			"under a `## Memories` heading, and told nothing:\n%s", out)
	}
	if strings.Contains(out, "nothing has been saved for it") {
		t.Errorf("the empty-block census was not supposed to fire: the block is not empty, and that sentence "+
			"denies a Learned Context or a decision that may well exist:\n%s", out)
	}
}

// TestTheOwnRowsNoteNeverClaimsThatARowAboveIsCrossProjectWhenThereIsNone is a
// review should-fix, and the first version of it PASSED VACUOUSLY. Both facts
// about it matter more than the fix.
//
// **The vacuity.** It seeded the project-keyed section with `RecordDecision`, and
// `RecordDecision` also writes a COMPANION MEMORY ROW into the project
// (`internal/memory/decisions.go`). So a row of the project's own was admitted, the
// note returned "" at the `it.ProjectID == projectID` loop rather than at any
// section guard, and the test asserted a case it never reached. Reached with a
// decision whose memory row is deleted, it fails — which is what a reviewer's read
// of the fixture was worth.
//
// **The defect.** `projectContextOwnRowsNote`'s `n == 0` sentence claimed "every
// row above applies to all projects", and the only structural guard was
// `len(res.Items) == 0` — which says nothing about the sections these surfaces
// render outside the assembler. A project can hold zero memory rows and still have
// both: `ghost reflect` writes learned context into `ghost_state`,
// `ghost_decision_record` writes an active decision, and `ghost_memory_delete`
// removes only the `memories` row. With a live `_global` row admitted, the tool
// shipped:
//
//	## Memories
//
//	- [preference] `liveglobal` (0.8 source=builtin) «a live cross-project preference»
//
//	## Learned Context
//
//	what reflection concluded about this project
//
//	Ghost holds no memories for this project; every row above applies to all projects.
//
// which is the same misattribution the note exists to remove, one section further
// down: the learned summary is THIS project's.
//
// Fixed by REWORDING rather than by the alternative fix — threading a
// caller-supplied flag for "did you render a project-keyed section". That flag's only
// job would be to suppress a sentence that a narrower wording makes unnecessary, and
// it would have to be threaded through two callers that each already know the answer.
func TestTheOwnRowsNoteNeverClaimsThatARowAboveIsCrossProjectWhenThereIsNone(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	seedCrossProjectRow(t, st, "liveglobal", "a live cross-project preference")
	ctx := context.Background()

	// LEARNED CONTEXT, not a decision: it is a column in `ghost_state` and writes no
	// memory row at all, so `bare` really does hold none. `RecordDecision` would have
	// written a companion memory row and put the note's own loop in the way.
	if err := st.UpdateLearnedContext(ctx, "bare", "what reflection concluded about this project", ""); err != nil {
		t.Fatalf("seed a learned context: %v", err)
	}

	// The precondition, asserted rather than assumed — this is what the first version
	// of this test failed to do, and it is the whole reason it passed vacuously.
	res, err := srv.projectContextMemories(ctx, "bare", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories bare: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ProjectID != memory.GlobalProjectID {
		t.Fatalf("fixture: expected exactly one admitted row and it must be the cross-project one, got %d rows "+
			"(%v). A row of the project's own here would make this test vacuous again.", len(res.Items), res.Items)
	}

	// BOTH surfaces, because `## Learned Context` is the section each one renders and
	// the tool is where a project-keyed section above the note is reachable at all.
	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	body, err := srv.buildProjectContext(ctx, "bare")
	if err != nil {
		t.Fatalf("buildProjectContext bare: %v", err)
	}
	for name, block := range map[string]string{"ghost_project_context": tool, "the project-context resource": body} {
		if !strings.Contains(block, "a live cross-project preference") {
			t.Fatalf("fixture: %s does not carry the cross-project row:\n%s", name, block)
		}
		if !strings.Contains(block, "Learned Context") {
			t.Fatalf("fixture: %s does not carry a learned context, so the project-keyed section above the note "+
				"is not exercised:\n%s", name, block)
		}
		if !strings.Contains(block, "what reflection concluded about this project") {
			t.Fatalf("fixture: %s does not carry the learned text:\n%s", name, block)
		}
		if strings.Contains(block, "every row above applies to all projects") {
			t.Errorf("%s told the reader that every row above applies to all projects, above a learned summary "+
				"that belongs to this project and to no other:\n%s", name, block)
		}
	}

	// And the fact that was missing is still reported, on both surfaces.
	for name, block := range map[string]string{"ghost_project_context": tool, "the project-context resource": body} {
		if !strings.Contains(block, "Ghost holds no memories for this project") {
			t.Errorf("%s no longer says the project holds no memories, so it is back to listing the "+
				"cross-project row alone:\n%s", name, block)
		}
	}

	// The counter-case, on the OTHER side of the same rewording: a project whose own
	// memory IS in the block must not be told it holds none.
	if _, err := st.CreateWithIDFromCorpus(ctx, "bare", "ownrow", memory.Memory{
		Category: "fact", Content: "a live memory of its own", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed bare's own row: %v", err)
	}
	after := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(after, "a live memory of its own") {
		t.Fatalf("fixture: the control project has no row of its own in the block:\n%s", after)
	}
	if strings.Contains(after, "Ghost holds") {
		t.Errorf("a project whose own row IS in the block was told it holds nothing above it:\n%s", after)
	}
}

// TestTheOwnRowsNoteRefusesTheTwoProjectsItHasNothingToSayAbout is the SEAM-level
// test for the two guards the surface-level tests cannot kill, because both are
// unreachable through any surface — which is exactly why they need their own test
// rather than a note in a comment.
//
// `projectContextOwnRowsNote`'s first guard, `projectID == "" || projectID ==
// memory.GlobalProjectID`, is redundant with the loop beneath it on both counts:
// an unresolved name never reaches the function (the tool returns the
// not-registered sentence from its own branch), and `_global`'s admitted rows
// carry `_global` as their own ProjectID, so the loop matches and returns "".
// Redundant defence at a seam two callers share is worth keeping — and a guard
// nothing can reach is worth nothing, so it is asserted HERE rather than trusted.
// Removing it changes nothing today and would change nothing tomorrow for a
// caller that passes a bucket rather than a project.
//
// M42 and M43 were exactly this: two mutations of that one line, both survivors
// through every surface. This kills both.
func TestTheOwnRowsNoteRefusesTheTwoProjectsItHasNothingToSayAbout(t *testing.T) {
	st := newValidityStore(t)
	srv, _ := validityServerFor(t, st)
	ctx := context.Background()

	// A result carrying a `_global` row and nothing else — the shape both refused
	// projects are asked about. It is built by hand rather than read through a
	// surface, because no surface produces it: that unreachability is the point.
	crossProject := assemble.Result{
		Items: []assemble.Item{{ID: "liveglobal", ProjectID: memory.GlobalProjectID}},
	}

	if note := srv.projectContextOwnRowsNote(ctx, memory.GlobalProjectID, crossProject); note != "" {
		t.Errorf("_global IS a project, not a bucket that borrowed one, and its own rows were admitted: "+
			"the note fired with %q", note)
	}
	if note := srv.projectContextOwnRowsNote(ctx, "", crossProject); note != "" {
		t.Errorf("an unresolved project name has no rows to count, and the not-registered sentence already "+
			"says so with the caller's own words: the note fired with %q", note)
	}

	// And the half that makes the refusals safe: a project that is neither, with no
	// row of its own in the block, DOES get the note. Without this the two refusals
	// above would pass on a function that never fires at all.
	if _, err := st.CreateWithIDFromCorpus(ctx, "vproj", "retired", memory.Memory{
		Category: "fact", Content: "a retired row", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	// And the half that makes the refusals safe: a project that is neither, with no
	// row of its own in the block, DOES get the note. Without this the two refusals
	// above would pass on a function that never fires at all.
	if note := srv.projectContextOwnRowsNote(ctx, "vproj", assemble.Result{Items: crossProject.Items}); !strings.Contains(note, "Ghost holds 1 memory") {
		t.Errorf("a project holding a row, none of it admitted, was not told so; got %q", note)
	}
}

// TestTheOwnRowsNoteIsSilentWhenNoMemoryRowWasAdmittedAtAll is the third survivor,
// and it is the one that needed the decision section to reach.
//
// The guard is `len(res.Items) == 0`, and the only way to reach the function with
// an EMPTY item set and a NON-EMPTY block is a project whose block is made of the
// sections this surface keeps outside the assembler. With a live cross-project row
// present the items are never empty, which is why the earlier version of this test
// seeded one and therefore could not kill the mutation.
//
// So this seeds NO global at all, and the block is non-empty only because of the
// learned context. That is the shape the guard exists for.
func TestTheOwnRowsNoteIsSilentWhenNoMemoryRowWasAdmittedAtAll(t *testing.T) {
	st := newValidityStore(t)
	srv, _ := validityServerFor(t, st)
	ctx := context.Background()

	// `bare` holds no memories and no `_global` row exists, so the memory read admits
	// nothing. LEARNED CONTEXT is the section that makes the block non-empty: it is
	// a direct read of one column, not a memory row, and `ghost reflect` writes it
	// for a project that may hold no memories at all.
	if err := st.UpdateLearnedContext(ctx, "bare", "what reflection concluded about this project", ""); err != nil {
		t.Fatalf("seed a learned context: %v", err)
	}

	// The precondition, stated as one: a non-empty block with no admitted row.
	res, err := srv.projectContextMemories(ctx, "bare", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories bare: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d memory rows were admitted, so the empty-item case is not exercised", len(res.Items))
	}
	body, err := srv.buildProjectContext(ctx, "bare")
	if err != nil {
		t.Fatalf("buildProjectContext bare: %v", err)
	}
	if !strings.Contains(body, "Learned Context") {
		t.Fatalf("fixture: the block is empty as well, so it never reaches the note:\n%s", body)
	}
	if strings.Contains(body, "Ghost holds") {
		t.Errorf("a block whose every row belongs to this project was given a note about cross-project rows:\n%s", body)
	}
}

// TestAProjectWhoseOwnRowsAreAllWithheldIsToldSoBesideItsLearnedContext is #788,
// and the gate it finds is the one neither of the two gates above could see.
//
// `projectContextEmptyNote` is consulted only when the WHOLE block is empty, and
// `projectContextOwnRowsNote` returned "" for an empty item set on the reasoning
// that an empty block is the other function's. So a project whose every memory has
// retired AND which reflection has already summarised answered with the summary
// alone — the census the fix exists to remove never fired, and neither did the
// exclusion that replaces it. The caller is handed a conclusion derived from those
// very memories and told nothing about their retirement, which is the unmarked
// retired claim stage 2 exists to prevent.
//
// It is not an exotic store: `ghost reflect` writes learned context into
// `ghost_state` for a project it has memories for, and a project old enough to have
// been reflected over is one whose memories have aged out. LEARNED CONTEXT is the
// section to reach it with for two reasons, and the second is why a DECISION is not
// the section to use: it is the one project-keyed section BOTH surfaces render, and
// it writes no memory row, so the project really does admit nothing.
// `RecordDecision` writes a `decision_log` MEMORY in the same transaction
// (`internal/memory/decisions.go`) and the tool reports it — "a companion memory was
// also saved" — so a decision-only fixture would put a live row of the project's own
// in the block and answer `""` at the item loop rather than at any gate. That is
// asserted in the pre-existing `TestTheOwnRowsNoteNeverClaimsThatARowAboveIsCross
// ProjectWhenThereIsNone`, whose first version passed vacuously for exactly this
// reason; its fixture is learned context for the same reason this one is.
//
// The two preconditions are asserted rather than assumed, because each of them is
// the way this test stops exercising the defect: a live `_global` row would make the
// outcome `answerable` and the already-working half of the gate would answer, and a
// project holding no memory row at all would make the window empty rather than
// withheld, which is the census's case and the sibling test's.
//
// Asserted as TWO clauses rather than one string. "withheld as out of date" is the
// assembler's own exclusion wording and belongs to `internal/assemble`; the browse
// pointer is this surface's, and it is what makes the sentence actionable. A test
// that pinned the whole sentence would be a second copy of wording another package
// owns.
func TestAProjectWhoseOwnRowsAreAllWithheldIsToldSoBesideItsLearnedContext(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()
	// The only memory, and it has retired.
	saveValidityRow(t, session, "vproj: the only memory, and it is retired",
		map[string]any{"valid_until": "2021-01-01"})
	if err := st.UpdateLearnedContext(ctx, "vproj", "what reflection concluded about this project", ""); err != nil {
		t.Fatalf("seed a learned context: %v", err)
	}

	res, err := srv.projectContextMemories(ctx, "vproj", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories vproj: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d memory rows were admitted, so the empty-item case is not exercised", len(res.Items))
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason == assemble.ReasonNoMemories {
		t.Fatalf("fixture: got outcome %q reason %q, want an empty verdict that is not %q — a row was found and "+
			"withheld, which is the only fact that makes the note load-bearing", res.Outcome, res.Reason, assemble.ReasonNoMemories)
	}

	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	body, err := srv.buildProjectContext(ctx, "vproj")
	if err != nil {
		t.Fatalf("buildProjectContext vproj: %v", err)
	}
	for name, block := range map[string]string{
		"ghost_project_context":        tool,
		"the project-context resource": body,
	} {
		if !strings.Contains(block, "what reflection concluded about this project") {
			t.Fatalf("fixture: %s does not carry the learned context, so the note is not being asked to sit beside "+
				"it:\n%s", name, block)
		}
		if strings.Contains(block, "nothing has been saved") {
			t.Errorf("%s answers a project Ghost holds a memory for with the never-saved census:\n%s", name, block)
		}
		if !strings.Contains(block, "withheld as out of date") {
			t.Errorf("%s withheld every row of the project and said nothing about it, so a caller reading the "+
				"learned summary above is told a summary derived from retired memories and not that those memories "+
				"are retired:\n%s", name, block)
		}
		if !strings.Contains(block, "still marked with the window they carry") {
			t.Errorf("%s reported the exclusion without pointing at the surface that still shows the rows:\n%s", name, block)
		}
	}

	// The control, and it is the half a fix can get wrong in the other direction: a
	// project with a live row of its OWN beside a learned context has no gap to
	// report, and a note here would fire for every project on a store holding one.
	// Without it, "return the abstention whenever the block is non-empty" passes.
	if _, err := st.CreateWithIDFromCorpus(ctx, "bare", "ownrow", memory.Memory{
		Category: "fact", Content: "a live memory of its own", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed bare's own row: %v", err)
	}
	if err := st.UpdateLearnedContext(ctx, "bare", "what reflection concluded about bare", ""); err != nil {
		t.Fatalf("seed bare's learned context: %v", err)
	}
	withOwn := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(withOwn, "a live memory of its own") {
		t.Fatalf("fixture: the control project has no row of its own in the block:\n%s", withOwn)
	}
	if strings.Contains(withOwn, "withheld as out of date") {
		t.Errorf("a project whose own row IS in the block was told its rows were withheld:\n%s", withOwn)
	}

	// The second control: a project holding NO memory row of its own. Its block is
	// non-empty for the same reason as the first one's, and the exclusion sentence
	// would be a lie in the direction this whole rule exists to prevent — Ghost found
	// nothing FOR THIS PROJECT, and "withheld as out of date" claims it found rows and
	// retired them. The browse it points at agrees: an unfiltered
	// `ghost_memories_list` does not widen, so it returns nothing for a project
	// holding nothing.
	//
	// TWO windows, because the verdict is computed over the UNION
	// (`projectContextBudget` sets `IncludeGlobal`) and the two disagree about this
	// project. With no `_global` row at all the window is `no_memories` and the
	// census's case; the assertion below needs the hard one, where the window IS
	// non-empty and every row in it belongs to somebody else and is itself withheld,
	// so the reason is `all_invalid` while `CountMemories` for the project is 0. A
	// branch that reads the union's verdict for a project-scoped sentence gets that
	// one wrong, and the `no_memories` half alone passes while it does.
	for _, c := range []struct {
		project string
		globals bool
		want    string
	}{
		// No global anywhere: the window itself is empty.
		{project: "nowindow", want: assemble.ReasonNoMemories},
		// One live global and one RETIRED one: the window holds rows, stage 2 drops
		// the only admitted one for validity, and the reason is an exclusion.
		{project: "otherrows", globals: true, want: "all_invalid"},
	} {
		if err := st.EnsureProject(ctx, c.project, t.TempDir(), c.project); err != nil {
			t.Fatalf("EnsureProject %s: %v", c.project, err)
		}
		if err := st.UpdateLearnedContext(ctx, c.project, "what reflection concluded about "+c.project, ""); err != nil {
			t.Fatalf("seed %s's learned context: %v", c.project, err)
		}
		if c.globals {
			// EVERY global in the window is retired, and that is the shape rather than
			// a convenience: a live global is an ADMITTED item, so it would take the
			// branch this control is not about. The window is non-empty and stage 2
			// empties it, which is what makes the union's verdict an exclusion while
			// the project itself holds nothing. A real store reaches it whenever its
			// cross-project rows have aged out, which is the same ageing that put this
			// project in the case above.
			for i := 0; i < 2; i++ {
				if _, err := st.CreateWithIDFromCorpus(ctx, memory.GlobalProjectID,
					"retired"+c.project+twoDigits(i), memory.Memory{
						Category: "preference", Content: "a retired cross-project preference", Source: "manual",
						Importance: 0.9, ValidUntil: strPtr("2021-01-01"),
					}); err != nil {
					t.Fatalf("seed the retired global: %v", err)
				}
			}
		}

		res, err := srv.projectContextMemories(ctx, c.project, projectContextMemoriesCap)
		if err != nil {
			t.Fatalf("projectContextMemories %s: %v", c.project, err)
		}
		if len(res.Items) != 0 {
			t.Fatalf("fixture: %s admitted %d memory rows, so the empty-item case is not exercised",
				c.project, len(res.Items))
		}
		if res.Outcome != assemble.OutcomeEmpty || res.Reason != c.want {
			t.Fatalf("fixture: %s got outcome %q reason %q, want empty/%q", c.project, res.Outcome, res.Reason, c.want)
		}
		if n, err := st.CountMemories(ctx, c.project); err != nil || n != 0 {
			t.Fatalf("fixture: %s holds %d memories (err %v), so this is not the project-holds-nothing shape",
				c.project, n, err)
		}

		tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": c.project}))
		body, err := srv.buildProjectContext(ctx, c.project)
		if err != nil {
			t.Fatalf("buildProjectContext %s: %v", c.project, err)
		}
		for name, block := range map[string]string{
			"ghost_project_context":        tool,
			"the project-context resource": body,
		} {
			if !strings.Contains(block, "what reflection concluded about "+c.project) {
				t.Fatalf("fixture: %s does not carry %s's learned context:\n%s", name, c.project, block)
			}
			if strings.Contains(block, "withheld as out of date") {
				t.Errorf("%s told a caller that rows were found and retired for %s, a project Ghost holds no memory "+
					"for at all — the rows the union's verdict is describing belong to _global, and the browse it "+
					"points at returns nothing:\n%s", name, c.project, block)
			}
			if strings.Contains(block, "nothing has been saved") {
				t.Errorf("%s answered %s with the never-saved census; the block carries its learned context, so "+
					"that sentence denies what the reader is looking at:\n%s", name, c.project, block)
			}
		}
	}
}

// uncountableStore fails `CountMemories` for one project and delegates everything
// else, which is the only way to reach the count's ERROR path: a `*memory.Store`
// never fails it, and a real failure (a locked file, a truncated page) is not
// something a test should arrange on a real store.
type uncountableStore struct {
	provider.MemoryStore
	project string
	err     error
}

func (u uncountableStore) CountMemories(_ context.Context, projectID string) (int, error) {
	if projectID == u.project {
		return 0, u.err
	}
	return u.MemoryStore.CountMemories(context.Background(), projectID)
}

// TestAWithheldProjectIsToldNothingWhenItsRowCountCannotBeRead is the third
// control, and it is the one a surface test cannot reach: a count that ERRORS.
//
// The count is the one project-scoped fact in reach on this path, so an unreadable
// one leaves the sentence with no support. A failed count is not evidence that the
// project holds nothing, and it is not evidence that it does either, so the note
// stays silent — the same rule the `n == 0` branch below has always followed, and
// the reason this branch borrows that branch's guard rather than its own answer.
//
// Asserted at the SEAM, on the function, because the alternative is a store that
// fails only for a project with a learned context and an `all_invalid` window, and
// arranging that on top of an already-erroring count would test the fake.
func TestAWithheldProjectIsToldNothingWhenItsRowCountCannotBeRead(t *testing.T) {
	st := newValidityStore(t)
	// A project whose only memory is retired, so the verdict is a withheld one and
	// the count is the only thing standing between it and a false sentence.
	if _, err := st.CreateWithIDFromCorpus(context.Background(), "vproj", "retiredrow", memory.Memory{
		Category: "fact", Content: "a retired row", Source: "manual", Importance: 0.9,
		ValidFrom: strPtr("2020-01-01"), ValidUntil: strPtr("2021-01-01"),
	}); err != nil {
		t.Fatalf("seed the retired row: %v", err)
	}
	readErr := errors.New("the count could not be read")
	srv := New(uncountableStore{MemoryStore: st, project: "vproj", err: readErr},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	ctx := context.Background()

	// The verdict is read from the real store rather than the fake, because the fake
	// is not an `assembleCapableStore` — it delegates `provider.MemoryStore`, and the
	// point of this test is the count, not the assembly.
	readable := New(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	res, err := readable.projectContextMemories(ctx, "vproj", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories: %v", err)
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason == assemble.ReasonNoMemories {
		t.Fatalf("fixture: got outcome %q reason %q, want a withheld verdict", res.Outcome, res.Reason)
	}
	if n, err := st.CountMemories(ctx, "vproj"); err != nil || n != 1 {
		t.Fatalf("fixture: the project holds %d rows (err %v), so the count is not the only thing gating the note",
			n, err)
	}

	// A count that could not be read supports neither sentence: not the exclusion
	// (which claims rows were found and retired) and not the census (which claims
	// nothing was ever saved). Both are claims about this project, and the one fact
	// that could make either of them is the fact that failed.
	//
	// Asserted over BOTH item sets, and the second is the one a mutation of the
	// error path survives: an empty item set returns "" on the `n == 0` refusal, so
	// ignoring the error and reading the zero value produces the same answer there.
	// The non-empty set is where an ignored error renders "Ghost holds no memories for
	// this project" — the census, from a count that was never read, about a project
	// whose rows the block is visibly carrying someone else's for. A result carrying
	// a `_global` item and no row of the project's own is that shape, and it is built
	// by hand for the reason `TestTheOwnRowsNoteRefusesTheTwoProjectsItHasNothingTo
	// SayAbout` builds its own: no surface produces it.
	if note := srv.projectContextOwnRowsNote(ctx, "vproj", res); note != "" {
		t.Errorf("an unreadable row count produced a sentence about the project: %q. Silence on a failed count is "+
			"the same rule the n == 0 branch follows, and it is the only answer an unreadable fact supports.", note)
	}
	crossProject := assemble.Result{Items: []assemble.Item{{ID: "liveglobal", ProjectID: memory.GlobalProjectID}}}
	if note := srv.projectContextOwnRowsNote(ctx, "vproj", crossProject); note != "" {
		t.Errorf("an unreadable row count produced a sentence over a NON-empty item set: %q. There the zero value "+
			"an ignored error leaves behind renders \"Ghost holds no memories for this project\", which is the "+
			"census claimed from a fact nobody read.", note)
	}
	// And the delegate is used for every OTHER project, or the fake would pass by
	// failing every count and the assertion above would prove nothing about scoping.
	// `bare` now holds a retired row of its own, so the count there succeeds, reaches
	// zero is false, and the verdict renders the exclusion.
	if _, err := st.CreateWithIDFromCorpus(ctx, "bare", "baretired", memory.Memory{
		Category: "fact", Content: "another retired row", Source: "manual", Importance: 0.9,
		ValidFrom: strPtr("2020-01-01"), ValidUntil: strPtr("2021-01-01"),
	}); err != nil {
		t.Fatalf("seed bare's retired row: %v", err)
	}
	if note := srv.projectContextOwnRowsNote(ctx, "bare", res); !strings.Contains(note, "withheld as out of date") {
		t.Errorf("the failing count leaked to another project, so this test would pass against a fake that fails "+
			"everywhere — the count must be read for the project the sentence is about: got %q", note)
	}
}

// TestTheEmptyBlockGatesTheProjectScopedCountToo is the SECOND review-gate finding,
// and it is this PR's own comment that made it: the note on the new branch says a
// note is never read off the union's verdict alone, and that was true of one of the
// three branches that can render one. The two EMPTY-BLOCK gates —
// `projectContextEmptyNote` behind `if text == ""` in the tool and behind
// `if sb.Len() == 0` here — have always answered on the verdict alone, which is the
// same union bug #788 is about and it predates this PR.
//
// The shape is the empty-block twin of the `otherrows` control above: a registered
// project holding ZERO memory rows, on a store whose cross-project rows have all aged
// out, so the union window is non-empty, stage 2 empties it and the reason is
// `all_invalid`. The whole answer then becomes the abstention, telling a project
// Ghost holds nothing for that its rows were found and retired, and pointing at a
// `ghost_memories_list` that returns nothing for it — the same misattribution, and on
// this shape the block is empty, so there is nothing above the note to make it
// subtle.
//
// It is asserted with the learned context ABSENT deliberately: with it, the block is
// non-empty and this is the `otherrows` control, which already passes. The empty case
// is the one that reaches the other two gates, and it is the one that had no coverage
// at all.
func TestTheEmptyBlockGatesTheProjectScopedCountToo(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()
	// ONLY retired globals, so the window is non-empty and every row in it is
	// dropped by stage 2. A live global would be an admitted item and would take the
	// `answerable` path this is not about.
	for i := 0; i < 2; i++ {
		if _, err := st.CreateWithIDFromCorpus(ctx, memory.GlobalProjectID, "agedout"+twoDigits(i), memory.Memory{
			Category: "preference", Content: "a retired cross-project preference", Source: "manual",
			Importance: 0.9, ValidFrom: strPtr("2020-01-01"), ValidUntil: strPtr("2021-01-01"),
		}); err != nil {
			t.Fatalf("seed the retired global: %v", err)
		}
	}
	// `bare` holds nothing at all, and nothing is written to it: no learned context,
	// no decision. The two counters below are the preconditions, and each of them is a
	// way this test stops exercising the defect.
	res, err := srv.projectContextMemories(ctx, "bare", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories bare: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d memory rows were admitted", len(res.Items))
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason != "all_invalid" {
		t.Fatalf("fixture: got outcome %q reason %q, want empty/all_invalid — the exclusion reason is what makes "+
			"the union window disagree with the project's own row count", res.Outcome, res.Reason)
	}
	if n, err := st.CountMemories(ctx, "bare"); err != nil || n != 0 {
		t.Fatalf("fixture: the project holds %d rows (err %v), so this is not the project-holds-nothing shape",
			n, err)
	}

	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	body, err := srv.buildProjectContext(ctx, "bare")
	if err != nil {
		t.Fatalf("buildProjectContext bare: %v", err)
	}
	for name, block := range map[string]string{
		"ghost_project_context":        tool,
		"the project-context resource": body,
	} {
		if strings.Contains(block, "withheld as out of date") {
			t.Errorf("%s told a caller that rows were found and retired for a project Ghost holds no memory for at "+
				"all — the rows the union's verdict describes belong to _global, and the browse it points at returns "+
				"nothing for the project it names:\n%s", name, block)
		}
		// Silence would repair the defect above by removing the reader's only clue
		// that the window was read and came back empty, so the census has to be
		// there. Its wording is each surface's own — the tool resolves the project
		// and says so, the resource says "No memories found for this project" — so
		// this is the census rather than one of the two strings.
		if !strings.Contains(block, "nothing has been saved for it") &&
			!strings.Contains(block, "No memories found for this project.") {
			t.Errorf("%s is neither the exclusion nor a census, so a reader cannot tell what the read found:\n%s",
				name, block)
		}
	}

	// The control on the other side of the same gate: a project whose OWN row is
	// retired still gets the exclusion, because its count is one. Without it, a fix
	// that dropped the exclusion from every empty block would pass.
	if _, err := st.CreateWithIDFromCorpus(ctx, "vproj", "ownretired", memory.Memory{
		Category: "fact", Content: "a retired row of its own", Source: "manual", Importance: 0.9,
		ValidFrom: strPtr("2020-01-01"), ValidUntil: strPtr("2021-01-01"),
	}); err != nil {
		t.Fatalf("seed vproj's retired row: %v", err)
	}
	for name, block := range map[string]string{
		"ghost_project_context":        resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})),
		"the project-context resource": mustProjectContext(t, srv, "vproj"),
	} {
		if !strings.Contains(block, "withheld as out of date") {
			t.Errorf("%s stopped reporting the exclusion for a project whose own row was retired; the count gate is "+
				"meant to keep the project-holds-nothing case quiet, not to silence every empty block:\n%s", name, block)
		}
	}
}

// TestAResolvedRowIsNotAPermanentlyEmptyProject is the THIRD review-gate finding,
// and it is the one about the count being read and then DISCARDED.
//
// The two readers do not agree about what a project holds. The passive window is
// `WHERE (project_id = ? OR project_id = '_global') AND resolved_at IS NULL`
// (`passiveFetchSQL`), while `CountMemories` is `SELECT COUNT(*) FROM memories
// WHERE project_id = ?` — no `resolved_at` predicate. So a project whose only row
// `ghost resolve` has withdrawn has an EMPTY window and a count of one.
//
// `holdsOwnRows` therefore answers true, the gate opens, and the answer is then
// thrown away: `projectContextEmptyNote` returns "" because the reason is
// `no_memories`, so control falls through to the census — "nothing has been saved
// for it" — for a project Ghost holds a row for, while `ghost_memories_list` returns
// that row. The gate was read to make a sentence possible and then declined to use
// the only sentence it makes possible.
//
// The RESOLVED row is the fixture because it is the one way to reach a count above
// zero with a window the fetch emptied, without any stage involved: a validity
// window is stage 2 and would make the reason an exclusion, which is the case the
// abstention already answers. Resolution is the FETCH's own filter, so nothing
// downstream can name it and the sentence has to come from the count.
//
// The sentence is the census's sibling, not the exclusion: nothing was withheld, so
// "withheld as out of date" would be false, and "nothing has been saved" is false
// too. What is true is that the project holds a row and it is not in the block, and
// `projectContextOwnRowsNote` already renders exactly that from the same count —
// which is why the fix is to fall back to it rather than to invent a fourth
// sentence.
func TestAResolvedRowIsNotAPermanentlyEmptyProject(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()
	// The row is written directly with `resolved_at` stamped, because that is the
	// state `ghost resolve` leaves and the state the passive fetch filters on. A save
	// through the tool would be an un-resolved row, which is the other fixture.
	//
	// `MarkResolved` is not used: it REFUSES a row in a standing category and a
	// pinned one, and it returns which ids it actually stamped, so a fixture that
	// ignored the return would pass without having produced the state. The stamp is
	// the whole fixture, so it is written directly and then read back.
	if _, err := st.CreateWithIDFromCorpus(ctx, "vproj", "withdrawn", memory.Memory{
		Category: "fact", Content: "a row ghost resolve withdrew", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}
	stampedIDs, err := st.MarkResolved(ctx, "vproj", []string{"withdrawn"}, memory.Provenance{})
	if err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}
	if len(stampedIDs) != 1 || stampedIDs[0] != "withdrawn" {
		t.Fatalf("fixture: MarkResolved stamped %v, so this is not the state the passive fetch filters on", stampedIDs)
	}
	stamped, err := st.ListMemories(ctx, "vproj", "", "", 10)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(stamped) != 1 || stamped[0].ResolvedAt == nil {
		t.Fatalf("fixture: MarkResolved declined the row (%d rows, resolved_at %v), so this is not the state the "+
			"passive fetch filters on", len(stamped), func() any {
			if len(stamped) == 1 {
				return stamped[0].ResolvedAt
			}
			return nil
		}())
	}

	// The preconditions, and the second is the whole finding: the two readers
	// disagree. Without the count being one this is the genuinely-empty project the
	// census is for.
	res, err := srv.projectContextMemories(ctx, "vproj", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories vproj: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d rows were admitted, so the fetch did not filter this row", len(res.Items))
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason != assemble.ReasonNoMemories {
		t.Fatalf("fixture: got outcome %q reason %q, want empty/%q — a resolved row is filtered by the FETCH, so no "+
			"stage withheld anything and the reason is an empty window", res.Outcome, res.Reason, assemble.ReasonNoMemories)
	}
	if n, err := st.CountMemories(ctx, "vproj"); err != nil || n != 1 {
		t.Fatalf("fixture: CountMemories says %d (err %v); the finding is that it and the window disagree", n, err)
	}
	// And the browse the sentence points at really does return the row, so this is
	// not a row Ghost has forgotten about.
	if rows, err := st.ListMemories(ctx, "vproj", "", "", 10); err != nil || len(rows) != 1 {
		t.Fatalf("fixture: ghost_memories_list returns %d rows (err %v), so a sentence pointing there is pointing at "+
			"nothing", len(rows), err)
	}

	for name, block := range map[string]string{
		"ghost_project_context":        resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})),
		"the project-context resource": mustProjectContext(t, srv, "vproj"),
	} {
		if strings.Contains(block, "nothing has been saved for it") ||
			strings.Contains(block, "No memories found for this project.") {
			t.Errorf("%s told a caller that nothing was ever saved for a project holding a row, which "+
				"ghost_memories_list returns:\n%s", name, block)
		}
		if !strings.Contains(block, "Ghost holds 1 memory for this project") {
			t.Errorf("%s reported an empty project without saying the row exists; the count already read for the gate "+
				"is what makes that sentence, and it was discarded:\n%s", name, block)
		}
	}
}

// mustProjectContext is the resource body with a test-shaped failure, so the loop
// above reads as one table rather than a block of setup.
func mustProjectContext(t *testing.T, srv *Server, projectID string) string {
	t.Helper()
	body, err := srv.buildProjectContext(context.Background(), projectID)
	if err != nil {
		t.Fatalf("buildProjectContext %s: %v", projectID, err)
	}
	return body
}
