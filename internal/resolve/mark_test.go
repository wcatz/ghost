package resolve

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// markSeed builds a store with one project ("p") and returns it plus the raw db
// so a test can pin an id or read a column the store does not surface. It is
// markSeed rather than a reuse of any other package's seed because this file's
// tests are about what an OPERATOR named — an id an operator can type — so they
// need to control ids directly, which no store method allows (every id-derived
// table references memories(id)).
func markSeed(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.EnsureProject(context.Background(), "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(context.Background(), "other", "/tmp/other", "other"); err != nil {
		t.Fatal(err)
	}
	return store, db
}

// markMem writes a memory in project p and returns its id. category defaults to
// "fact" so the store's own guard does not decline it for a standing category —
// the tests that care about that guard ask for it explicitly.
func markMem(t *testing.T, store *memory.Store, content, category string) string {
	t.Helper()
	if category == "" {
		category = "fact"
	}
	id, err := store.Create(context.Background(), "p", memory.Memory{Category: category, Content: content, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// pinMarkID writes a memory under a chosen id, so two of them can share an
// eight-character prefix. It writes the row directly because an id cannot be
// changed after the fact, only chosen up front.
func pinMarkID(t *testing.T, db *sql.DB, projectID, id, content string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO memories (id, project_id, category, content, importance, source)
		VALUES (?, ?, 'fact', ?, 0.5, 'mcp')
	`, id, projectID, content); err != nil {
		t.Fatalf("pin id %s: %v", id, err)
	}
}

func isMarkResolved(t *testing.T, store *memory.Store, id string) bool {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows", id, len(mems))
	}
	return mems[0].ResolvedAt != nil && *mems[0].ResolvedAt != ""
}

func keptHashOf(t *testing.T, store *memory.Store, projectID, id string) string {
	t.Helper()
	hashes, err := store.ResolveKeptHashes(context.Background(), projectID)
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	return hashes[id]
}

func markHistory(t *testing.T, store *memory.Store, id string) []memory.HistoryEntry {
	t.Helper()
	entries, err := store.MemoryHistory(context.Background(), id, 50)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	return entries
}

func markLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// keepVerdict answers KEEP for everything, which is the verdict that matters
// here: a KEEP is what un-hides a memory in a repair, and a KEEP is also what
// writes the cache entry a mark has to clear.
type keepVerdict struct{ calls int }

func (k *keepVerdict) IsResolvedBatch(_ context.Context, contents []string) ([]Verdict, error) {
	k.calls++
	out := make([]Verdict, len(contents))
	for i := range out {
		out[i] = VerdictKeep
	}
	return out, nil
}

// TestMarkThenReassessOnlyRoundTrips is the loop the issue is about. A memory an
// operator named as finished goes out of ranked injection, and a scoped repair
// brings it back — the two halves have to compose, because a mark nothing can
// undo is a mark an operator cannot be talked out of.
func TestMarkThenReassessOnlyRoundTrips(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "the staging relay port was fixed in the 0.36.0 release: it is 2222", "")
	bystander := markMem(t, store, "the port 2222 rollout experiment was abandoned upstream", "")

	applied, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{id[:8]}, Apply: true}, markLogger())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if applied.Marked != 1 || !applied.Memories[0].Marked {
		t.Fatalf("Mark = %+v, want the one row marked", applied)
	}
	if !isMarkResolved(t, store, id) {
		t.Fatal("the named memory is not resolved: the mark did not stamp it")
	}
	if isMarkResolved(t, store, bystander) {
		t.Fatal("the mark touched a memory it was not asked about")
	}

	// The inverse, exactly as the report names it: a repair SCOPED to the row
	// the mark buried. An unscoped one re-judges every resolved memory in the
	// project, which #698 measured proposing to un-hide 143 rows of which about
	// 35% were stale.
	cls := &keepVerdict{}
	res, reKept, err := Reassess(ctx, store, cls, "p", true, Scope{Only: []string{id}}, markLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Cleared != 1 || len(reKept) != 1 || reKept[0].ID != id {
		t.Fatalf("Reassess cleared=%d reKept=%v, want the marked memory back", res.Cleared, idsOf(reKept))
	}
	if isMarkResolved(t, store, id) {
		t.Error("the scoped repair did not clear the mark")
	}
}

// TestMarkDryRunWritesNothing: the default has to be a preview, because the
// judgement being trusted is the caller's and the corpus is the thing that has
// to survive being wrong about it. Every ref still has to RESOLVE in a dry run —
// a preview that cannot say which memory it is about is not a preview.
func TestMarkDryRunWritesNothing(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "the old release note for a port that moved", "")

	dry, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{id[:8]}}, markLogger())
	if err != nil {
		t.Fatalf("Mark (dry run): %v", err)
	}
	if dry.Resolved != 1 || dry.Marked != 0 {
		t.Fatalf("dry run = %+v, want one row resolved and none marked", dry)
	}
	if dry.Memories[0].ID != id {
		t.Errorf("the dry run resolved to %s, want %s", dry.Memories[0].ID, id)
	}
	if dry.Memories[0].Content == "" {
		t.Error("the dry run resolved the row but did not load it, so the report cannot show what it would mark")
	}
	if isMarkResolved(t, store, id) {
		t.Error("a dry run stamped resolved_at")
	}
	if n := markHistory(t, store, id); len(n) != 1 || n[0].Phase != "save" {
		t.Errorf("a dry run wrote history: %+v, want only the save", n)
	}
}

// TestMarkRefusesAnAmbiguousPrefix: a prefix naming two memories is a question
// with two answers, and the row it would have buried is not something to guess
// at. The refusal lists the matches so the operator can add characters, and
// nothing is written — including for the OTHER ref in the same request, which is
// the whole-request-settled rule.
func TestMarkRefusesAnAmbiguousPrefix(t *testing.T) {
	store, db := markSeed(t)
	ctx := context.Background()
	other := markMem(t, store, "a note the operator did name", "")
	first := "aaaaaaaa" + "1" + strings.Repeat("0", 23)
	second := "aaaaaaaa" + "2" + strings.Repeat("0", 23)
	pinMarkID(t, db, "p", first, "A note whose id was pinned so a prefix is ambiguous.")
	pinMarkID(t, db, "p", second, "Another note sharing the first eight characters.")

	_, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{other, "aaaaaaaa"}, Apply: true}, markLogger())
	if err == nil {
		t.Fatal("Mark accepted an ambiguous prefix")
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list the match %s: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "more") {
		t.Errorf("the refusal does not say how to disambiguate: %v", err)
	}
	// The whole request is settled before any write, so the ref that WAS fine is
	// not marked either.
	if isMarkResolved(t, store, other) {
		t.Error("one bad ref out of two marked the other: the request was not settled before the write")
	}
	if isMarkResolved(t, store, first) || isMarkResolved(t, store, second) {
		t.Error("an ambiguous ref marked something")
	}
}

// TestMarkRefusesAMemoryInAnotherProject: the ref query reaches `_global` and
// its neighbours' rows only as far as the project, so the guard that keeps a
// project from burying a memory it does not own is this one. A `_global` row is
// the case worth naming: it is reachable by ref from any project, and it is a row
// every project shares.
func TestMarkRefusesAMemoryInAnotherProject(t *testing.T) {
	store, db := markSeed(t)
	ctx := context.Background()
	fine := markMem(t, store, "a note in the project being marked", "")
	pinned := "bbbbbbbb" + "3" + strings.Repeat("0", 23)
	pinMarkID(t, db, "other", pinned, "A note that belongs to a different project.")

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{name: "a row in another project", ref: pinned},
		{name: "a promoted _global row", ref: markGlobal(t, store, "a promoted row every project shares")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{fine, tc.ref}, Apply: true}, markLogger())
			if err == nil {
				t.Fatal("Mark accepted a ref naming a memory the project does not own")
			}
			if isMarkResolved(t, store, fine) {
				t.Error("the project marked the memory it was asked about, alongside a refusal: the request was not settled first")
			}
		})
	}
}

// markGlobal saves a memory and promotes it to _global, which is what makes a
// row reachable by ref from a project that does not own it.
func markGlobal(t *testing.T, store *memory.Store, content string) string {
	t.Helper()
	id := markMem(t, store, content, "")
	if err := store.PromoteToGlobal(context.Background(), "p", id); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	return id
}

// TestMarkClearsTheKeptCache: the second effect the issue names. `--reassess
// --apply` caches its KEEP verdicts by content, so a row it wrongly re-kept
// reports `N KEEP cached` on the ordinary pass afterwards and is never re-judged
// until its text changes. Marking a memory that carries such a hash without
// dropping it would leave a row the operator buried on purpose visible again the
// moment anything rewrote it — the mark would be undone by an edit.
func TestMarkClearsTheKeptCache(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "NEVER run the restore with both ends on one spindle", "")
	other := markMem(t, store, "a note the mark was not asked about", "")

	// A KEEP hash on the named row and on one the request does not name: the
	// clear is scoped to the rows, not to the project.
	if err := store.MarkResolveKept(ctx, "p", map[string]string{
		id:    ContentHash("NEVER run the restore with both ends on one spindle"),
		other: ContentHash("a note the mark was not asked about"),
	}); err != nil {
		t.Fatal(err)
	}
	if keptHashOf(t, store, "p", id) == "" {
		t.Fatal("the fixture left no KEEP hash to clear")
	}

	if _, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{id}, Apply: true}, markLogger()); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if got := keptHashOf(t, store, "p", id); got != "" {
		t.Errorf("the marked row still carries the KEEP hash %q: the next pass would skip it as cached", got)
	}
	if keptHashOf(t, store, "p", other) == "" {
		t.Error("the mark cleared a KEEP cache entry on a memory it was not asked about")
	}
}

// TestMarkWritesTheResolveHistoryRowWithThePerformer: the issue's reason for
// making this a supported command at all. The SQL route an operator would
// otherwise take bypasses memory_history, so the record of how a memory reached
// its current state would show it resolved with nothing saying who said so. The
// `agent` column is the whole point: it is what tells a reader of that record
// that a person named this memory rather than a classifier judging it.
func TestMarkWritesTheResolveHistoryRowWithThePerformer(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "a note an operator decided was finished", "")

	prov := memory.Provenance{Agent: "operator"}
	if _, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{id}, Provenance: prov, Apply: true}, markLogger()); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	entries := markHistory(t, store, id)
	var resolves int
	for _, e := range entries {
		if e.Phase != "resolve" {
			continue
		}
		resolves++
		if e.Agent != "operator" {
			t.Errorf("the resolve history row's agent = %q, want %q — the record cannot say a person did this", e.Agent, "operator")
		}
		// The row is a VERSION, so it records the state the memory held once the
		// write landed. A history row without the stamp would be a record of a
		// resolution that did not happen.
		if e.ResolvedAt == nil || *e.ResolvedAt == "" {
			t.Errorf("the resolve history row records no resolved_at, so it does not describe the stamped state: %+v", e)
		}
	}
	if resolves != 1 {
		t.Errorf("the mark wrote %d resolve history row(s), want exactly 1", resolves)
	}
}

// TestMarkHistoryAndStampLandInOneTransaction: "in the same transaction" is not
// a description of two writes that happen to agree. A memory stamped without its
// history row is exactly the state the SQL route left behind and the issue
// exists to end, so the two must be one unit: make the history append fail and
// the stamp must not survive it.
//
// The failure is induced by taking the table away, which is the only way to make
// appendHistoryGroupTx's INSERT fail without a fake store — and it fails the same
// way a disk-full or a lock would, after the UPDATE has already run inside the
// open transaction.
func TestMarkHistoryAndStampLandInOneTransaction(t *testing.T) {
	store, db := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "a note whose history cannot be written", "")

	if _, err := db.ExecContext(ctx, `DROP TABLE memory_history`); err != nil {
		t.Fatalf("drop memory_history: %v", err)
	}
	_, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{id}, Apply: true}, markLogger())
	if err == nil {
		t.Fatal("Mark reported a stamp whose history row could not be written")
	}
	if isMarkResolved(t, store, id) {
		t.Error("resolved_at survived a failed history append: the stamp and its record are not one transaction")
	}
}

// TestMarkReportsAnAlreadyResolvedRowAsANoOp: the issue asks for this by name.
// Marking a memory the previous pass already buried changes nothing, and a
// report that called it a success would be claiming a write that did not happen —
// which is also the claim that makes a second `resolve` history row a lie.
func TestMarkReportsAnAlreadyResolvedRowAsANoOp(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	done := markMem(t, store, "a note an earlier pass already buried", "")
	fresh := markMem(t, store, "a note this pass is about to bury", "")
	if n, err := store.SetResolved(ctx, []string{done}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, err)
	}

	res, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{done, fresh}, Apply: true}, markLogger())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if res.Marked != 1 || res.AlreadyResolved != 1 {
		t.Fatalf("Mark = %+v, want one marked and one already resolved", res)
	}
	if res.Memories[0].Marked || !res.Memories[0].AlreadyResolved {
		t.Errorf("the already-resolved row is reported as %+v, want AlreadyResolved and not Marked", res.Memories[0])
	}
	if !res.Memories[1].Marked {
		t.Errorf("the fresh row is reported as %+v, want Marked", res.Memories[1])
	}
	// And no second resolve row for the no-op.
	resolves := 0
	for _, e := range markHistory(t, store, done) {
		if e.Phase == "resolve" {
			resolves++
		}
	}
	if resolves != 1 {
		t.Errorf("the no-op wrote %d resolve history row(s) in total, want the 1 the earlier pass wrote", resolves)
	}
}

// TestMarkReportsAPinAndAStandingCategoryRatherThanASilentNoOp: the store's
// guard declines three different rows for three different reasons, and only one
// of them is the issue's "already resolved". A pin is an explicit instruction to
// keep a memory visible; convention and preference are standing knowledge the
// pass never buries. A report that lumped all three under "not marked" would
// leave an operator with no idea which of them they can act on.
func TestMarkReportsAPinAndAStandingCategoryRatherThanASilentNoOp(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	pinned := markMem(t, store, "a note the operator pinned into view", "")
	if err := store.TogglePin(ctx, pinned, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	rule := markMem(t, store, "NEVER reuse a released database directory", "convention")

	res, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{pinned, rule}, Apply: true}, markLogger())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if res.Marked != 0 || res.Pinned != 1 || res.ExemptCategory != 1 {
		t.Fatalf("Mark = %+v, want nothing marked, one pinned and one standing category", res)
	}
	if isMarkResolved(t, store, pinned) || isMarkResolved(t, store, rule) {
		t.Error("the mark stamped a row the store's guard declines")
	}
	// They are still resolvable to full ids, so the report can name them.
	if res.Memories[0].ID != pinned || res.Memories[1].ID != rule {
		t.Errorf("the declined rows are not reported by id: %+v", res.Memories)
	}
}

// TestMarkRefusesARefItCannotResolve: a ref that names nothing is a refusal, and
// it says WHICH form failed. A reader who pasted a full id needs to be told the
// store does not hold it, not that the string they pasted is malformed — and a
// ref too short to be a prefix says THAT, without printing the project's ids.
func TestMarkRefusesARefItCannotResolve(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	id := markMem(t, store, "a note the operator can name by a short prefix", "")

	for _, tc := range []struct {
		name, ref, want string
	}{
		{name: "nothing holds it", ref: "ffffffffffffffff", want: `no memory in project p has an id starting with`},
		{name: "too short for a prefix", ref: id[:6], want: "too short to be a prefix"},
		{name: "not an id at all", ref: "not-an-id-at-all", want: `no memory in project p has an id starting with`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Mark(ctx, store, MarkRequest{ProjectID: "p", Refs: []string{tc.ref}, Apply: true}, markLogger())
			if err == nil {
				t.Fatal("Mark accepted a ref it cannot resolve")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to say %q", err, tc.want)
			}
			if isMarkResolved(t, store, id) {
				t.Error("a refused request marked a memory")
			}
		})
	}
}

// TestMarkNeedsAProject: a mark with no project is a request about every project,
// which is not a thing the operation is. The store refuses it too, and both
// refusals are here so neither can be the only one.
func TestMarkNeedsAProject(t *testing.T) {
	store, _ := markSeed(t)
	if _, err := Mark(context.Background(), store, MarkRequest{Refs: []string{"aaaaaaaa"}}, markLogger()); err == nil {
		t.Fatal("Mark accepted a request with no project")
	}
	if _, err := store.MarkResolved(context.Background(), "", []string{"aaaaaaaa"}, memory.Provenance{}); err == nil {
		t.Fatal("MarkResolved accepted an empty project, which would unbind the guard it exists to hold")
	}
	if _, err := Mark(context.Background(), store, MarkRequest{ProjectID: "p"}, markLogger()); err == nil {
		t.Fatal("Mark accepted a request naming no memory")
	}
}

// TestMarkRefusesTheGlobalProject: the sentinel is not a project a caller runs a
// lifecycle pass against, and the ownership guard does not catch it. `_global`
// holds promoted rows that EVERY project injects, so a mark naming it would bury
// a memory in all of them on the say-so of one command — and the per-row check
// passes, because the row's own project_id IS `_global` and the named project is
// too. The refusal therefore has to be a separate one, and it belongs at the
// store layer beside the empty-project refusal so every caller inherits it rather
// than each re-deriving it.
func TestMarkRefusesTheGlobalProject(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	promoted := markMem(t, store, "a promoted row every project injects", "")
	if err := store.PromoteToGlobal(ctx, "p", promoted); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	// Both entry points, because each resolves a project name of its own and a
	// caller could reach the sentinel through either.
	if _, err := Mark(ctx, store, MarkRequest{ProjectID: memory.GlobalProjectID, Refs: []string{promoted[:8]}, Apply: true}, markLogger()); err == nil {
		t.Error("Mark accepted a request naming the _global project")
	}
	if _, err := store.MarkResolved(ctx, memory.GlobalProjectID, []string{promoted}, memory.Provenance{}); err == nil {
		t.Error("MarkResolved accepted the _global project directly, so any caller reaching the store can cross the guard")
	}
	if isMarkResolved(t, store, promoted) {
		t.Fatal("a promoted memory was stamped: one project's command buried it for all of them")
	}
}

// TestMarkReportsARowTheWriteTimeGuardDeclined: the store re-checks eligibility
// where it writes, so a row that was eligible when this call read it can be
// ineligible by the time it writes — pinned, recategorized into a standing
// category, or moved to another project in between. The store declines such a row
// SILENTLY, because that is somebody else's decision rather than a failure here,
// and nothing in the returned ids distinguishes it from a row that was written.
//
// Without an explicit state the report's default marker says it was marked, and
// the memory is not buried: an operator who reads that will not look for it again,
// and the memory stays in every session's ranked context. The fake below is the
// only way to reach the state — a real store makes the race window vanishingly
// small, and a test that waits for it would be a test that passes by not running.
func TestMarkReportsARowTheWriteTimeGuardDeclined(t *testing.T) {
	store, _ := markSeed(t)
	ctx := context.Background()
	fresh := markMem(t, store, "a note that was eligible when the call read it", "")
	late := markMem(t, store, "a note another process pinned in the meantime", "")

	s := &decliningStore{MarkStore: store, decline: late}
	res, err := Mark(ctx, s, MarkRequest{ProjectID: "p", Refs: []string{fresh, late}, Apply: true}, markLogger())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if res.Marked != 1 || res.Declined != 1 {
		t.Fatalf("Mark = %+v, want one marked and one declined", res)
	}
	for _, m := range res.Memories {
		switch m.ID {
		case fresh:
			if !m.Marked {
				t.Error("the stamped row is not marked")
			}
		case late:
			if m.Marked {
				t.Error("the row the write-time guard declined is reported as marked: the memory is not buried")
			}
			if !m.Declined {
				t.Error("the row the write-time guard declined carries no state saying so")
			}
		}
	}
}

// decliningStore is the real store with a write that drops one id, which is what
// the store's own write-time guard does to a row that became ineligible between
// the caller's read and its write.
type decliningStore struct {
	MarkStore
	decline string
}

func (d *decliningStore) MarkResolved(ctx context.Context, projectID string, ids []string, prov memory.Provenance) ([]string, error) {
	keep := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != d.decline {
			keep = append(keep, id)
		}
	}
	return d.MarkStore.MarkResolved(ctx, projectID, keep, prov)
}

// TestMarkMakesNoClassifierCall: nothing is judged here, so a machine that cannot
// spawn a harness at all can still repair its corpus and nothing is billed. The
// same property internal/supersede.Withdraw has, and the reason both repairs are
// usable from a hook.
func TestMarkMakesNoClassifierCall(t *testing.T) {
	store, _ := markSeed(t)
	id := markMem(t, store, "a note an operator decided was finished", "")
	// A classifier that fails the test if it is ever asked. Mark takes no
	// classifier at all, so the assertion is structural: the request type has no
	// field for one. What this pins is that wiring it in is a change to a type
	// whose every other use is a pass that makes calls.
	res, err := Mark(context.Background(), store, MarkRequest{ProjectID: "p", Refs: []string{id}, Apply: true}, markLogger())
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if res.Marked != 1 {
		t.Errorf("Marked = %d, want 1", res.Marked)
	}
}

func idsOf(mems []memory.Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.ID)
	}
	return out
}
