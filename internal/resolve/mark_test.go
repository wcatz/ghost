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

func idsOf(mems []memory.Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.ID)
	}
	return out
}

func TestMarkRefusesWithoutAUsableRequest(t *testing.T) {
	for _, req := range []MarkRequest{
		{Refs: []string{"x"}},
		{ProjectID: memory.GlobalProjectID, Refs: []string{"x"}},
		{ProjectID: "p"},
	} {
		if _, err := Mark(context.Background(), nil, req, nil); err == nil {
			t.Errorf("Mark(%+v) = nil error", req)
		}
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
