package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// Issue #727, at the layer the issue is filed against. The store no longer
// writes a verbatim re-emission (internal/memory/reflect_noop_test.go covers the
// predicate), but the issue names two consequences that live above the store and
// are only reachable through the real `ghost reflect --apply`:
//
//   - --skip-unchanged fingerprints a project by updated_at, so a reflect that
//     stamped every row it merely looked at made the project read as changed;
//   - and the next lifecycle round therefore re-reflected the whole project.
//
// The sqlite tier is the right driver here: it emits every input verbatim
// (a `keep` and the pass-through both go through verbatimMemory), so an apply
// over a corpus of distinct facts IS the all-keep case, with no harness and
// nothing billable.

// distinctFacts are neither near-duplicates of each other nor of anything, so
// the sqlite tier's near-duplicate fold (Jaccard >= 0.5) has nothing to fold
// and consolidation carries every one of them through byte-identically.
var distinctFacts = []struct {
	category string
	content  string
}{
	{"architecture", "the block producer writes its KES epochs under /var/lib/cardano/kes"},
	{"gotcha", "the cardano-node metrics port is 12798 and must stay off the public interface"},
	{"fact", "unit tests run with the race detector on linux only"},
	{"convention", "YAML files in this repository are indented with two spaces"},
}

// seedPersistentRow writes a single keep-forever row and ages it, so the row is
// part of a corpus reflect would otherwise rewrite. The concurrent-save guard
// in ReplaceNonManual treats any row created at or after the round trip's start
// as concurrent and leaves it alone, so both timestamps have to move — the same
// reason seedAllKeepProject runs execBackdate + execBackdateAges.
func seedPersistentRow(t *testing.T, dataHome, project string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	ctx := context.Background()
	id, _, _, err := s.UpsertWithOptions(ctx, project, "fact",
		"the one durable fact reflection must never rewrite", "mcp", 0.6, nil,
		memory.UpsertOptions{Retention: memory.RetentionPersistent})
	if err != nil {
		t.Fatalf("create the persistent row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := execBackdateAges(dataHome, project); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
	return id
}

func countContent(t *testing.T, dataHome, content string) int {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for count: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, content).Scan(&n); err != nil {
		t.Fatalf("count content: %v", err)
	}
	return n
}

// TestRunReflectDoesNotDuplicateAPersistentRow is the review reproduction for
// #587: one persistent row, then `ghost reflect <project> --tier sqlite
// --apply`. Before the fix, consolidatable() fed the persistent row to the
// consolidator (the sqlite tier printed "passed through (not named): 1"), and
// ReplaceNonManual — whose replaceable set already excluded persistent rows via
// retentionExemptSQL, so the original survived — inserted the passed-through row
// as a fresh reflection copy beside it. A keep-forever memory came out
// duplicated on every apply, with the duplicate's own default tier and source.
//
// The fix is on the input side: a persistent row must not reach the
// consolidator at all, because the consolidator's output is a *rewrite* and
// every rewrite of a keep-forever row is a change the user asked it to be
// exempt from. With the input empty the sqlite tier emits nothing and the run
// returns before ReplaceNonManual, so the corpus is byte-identical after the
// apply as well as before it.
func TestRunReflectDoesNotDuplicateAPersistentRow(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	const project = "persistz"
	const durable = "the one durable fact reflection must never rewrite"
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	id := seedPersistentRow(t, dataHome, project)

	before := map[string]string{
		"retention": readColumn(t, dataHome, `SELECT retention FROM memories WHERE id = ?`, id),
		"source":    readColumn(t, dataHome, `SELECT source FROM memories WHERE id = ?`, id),
	}

	runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply")

	if n := countContent(t, dataHome, durable); n != 1 {
		t.Errorf("the persistent memory exists %d time(s) after an apply, want 1: consolidation duplicated it", n)
	}
	if got := readColumn(t, dataHome, `SELECT retention FROM memories WHERE id = ?`, id); got != before["retention"] {
		t.Errorf("the persistent row's tier is %q after the apply, want %q: reflection rewrote it", got, before["retention"])
	}
	if got := readColumn(t, dataHome, `SELECT source FROM memories WHERE id = ?`, id); got != before["source"] {
		t.Errorf("the persistent row's source is %q after the apply, want %q: reflection replaced it", got, before["source"])
	}
	if n := countHistory(t, dataHome, "reflect"); n != 0 {
		t.Errorf("an apply over a persistent-only corpus wrote %d reflect history row(s), want none", n)
	}
}

// seedAllKeepProject writes a project the sqlite tier will carry through
// unchanged, and returns the ids in write order.
func seedAllKeepProject(t *testing.T, dataHome, project string) []string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	ctx := context.Background()
	var ids []string
	for _, f := range distinctFacts {
		id, err := s.Create(ctx, project, memory.Memory{
			Category: f.category, Content: f.content, Source: "mcp", Importance: 0.6,
		})
		if err != nil {
			t.Fatalf("create %q: %v", f.content, err)
		}
		ids = append(ids, id)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// An hour ago, so the rows are part of the corpus reflection may rewrite.
	// ReplaceNonManual treats anything created at or after the round trip's start
	// as a concurrent save and leaves it alone, and created_at has one-second
	// resolution — a row seeded in the same second as the run is "concurrent" and
	// never reaches the reuse path at all.
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// execBackdate moves created_at only, which is all the replace's
	// concurrent-save guard reads. updated_at has to move as well, or every
	// assertion here about a timestamp that "did not move" is decided by whether
	// the test happened to run inside one second — datetime('now') has
	// one-second resolution, so a seeded row and a row an apply touched in the
	// same second are indistinguishable. Measured: with created_at alone
	// backdated, a pre-fix apply left updated_at at the seed's value and every
	// timestamp assertion below passed against the unfixed code.
	if err := execBackdateAges(dataHome, project); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
	return ids
}

// execBackdateAges moves updated_at as well as created_at, so "the apply did not
// touch this row" is a fact about the apply rather than about the clock.
func execBackdateAges(dataHome, project string) error {
	db, err := openRawDB(dataHome)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec(
		`UPDATE memories SET created_at = datetime('now', '-1 hour'), updated_at = datetime('now', '-1 hour') WHERE project_id = ?`,
		project)
	return err
}

func openReflectStore(t *testing.T, dataHome, project string) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(context.Background(), project, "/tmp/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

// countHistory reads the TABLE rather than the reader, for the same reason
// internal/memory's helper does: MemoryHistory defaults to the per-memory cap, so
// a read cannot distinguish a row this run appended from a row an earlier one
// left behind.
func countHistory(t *testing.T, dataHome, phase string) int {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for history count: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memory_history WHERE phase = ?`, phase).Scan(&n); err != nil {
		t.Fatalf("count %s history rows: %v", phase, err)
	}
	return n
}

func openRawDB(dataHome string) (*sql.DB, error) {
	return sql.Open("sqlite", filepath.Join(dataHome, "ghost", "ghost.db"))
}

func readColumn(t *testing.T, dataHome, query string, args ...any) string {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var got string
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return got
}

func TestRunReflectApplyOnAVerbatimCorpusWritesNoHistory(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	const project = "noopz"
	ids := seedAllKeepProject(t, dataHome, project)

	before := map[string]string{}
	for _, id := range ids {
		before[id] = readColumn(t, dataHome, `SELECT updated_at FROM memories WHERE id = ?`, id)
	}

	runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply")

	if n := countHistory(t, dataHome, "reflect"); n != 0 {
		t.Errorf("an all-keep apply wrote %d reflect history row(s), want none — every applied reflect "+
			"was appending one row per memory, which measured at 80%% of memory_history on a real store", n)
	}
	for _, id := range ids {
		if got := readColumn(t, dataHome, `SELECT updated_at FROM memories WHERE id = ?`, id); got != before[id] {
			t.Errorf("%s: updated_at %q -> %q — a re-emission stamped the row as touched", id, before[id], got)
		}
	}
}

// TestRunReflectSkipUnchangedSkipsAfterAnAllKeepApply is the gate the issue
// names, and what it asserts is narrower than the issue's phrasing — measured on
// the pre-fix code, not assumed.
//
// The issue says a reflect that stamped every row it merely looked at made the
// project "never read as unchanged, and the next lifecycle reflects it again in
// full". The first half is exactly right and is what this fix changes: an
// all-keep apply rewrote updated_at on every row, and updated_at is a field
// InputSignature carries as its change proxy. The second half did NOT reproduce
// on a corpus nothing else touched, because the fingerprint is recorded AFTER
// the apply (cmd/ghost/lifecycle.go, the post-apply corpus reload) — so the
// signature it stored already included the stamps, and the next round matched it
// and skipped. The claim holds only when something else moves the corpus between
// the two rounds, which is the case this fix removes at the source rather than
// papering over.
//
// So the property asserted here is the one that is both true and load-bearing:
// an all-keep apply leaves the fingerprint IDENTICAL to the corpus it was handed.
// Before the fix the two differed on every row's updated_at, which is the state
// in which any other writer's no-op — a resolve pass, a pin, a session's
// reflected read — invalidates the gate and pays for a full-corpus
// consolidation. After it, they are the same string, and the real runReflect
// takes the skip branch, which returns before it writes anything.
func TestRunReflectSkipUnchangedSkipsAfterAnAllKeepApply(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	const project = "noopz2"
	ids := seedAllKeepProject(t, dataHome, project)

	// The fingerprint of the corpus BEFORE the apply. This is the value the
	// property is about: an apply that changes nothing must not change what the
	// gate will read next time.
	before := corpusSignature(t, dataHome, project)

	runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply", "--skip-unchanged")

	recorded := readColumn(t, dataHome,
		`SELECT COALESCE((SELECT reflect_input_sig FROM ghost_state WHERE project_id = ?), '')`, project)
	if recorded == "" {
		t.Fatal("the first round recorded no skip fingerprint, so the second round has nothing to match")
	}
	if recorded != before {
		t.Errorf("the fingerprint of the corpus is %q but the apply recorded %q: an all-keep apply moved a "+
			"field the fingerprint reads, so --skip-unchanged is invalidated by reflect having done nothing, and "+
			"the next round pays for a full-corpus consolidation", before, recorded)
	}

	stamps := map[string]string{}
	for _, id := range ids {
		stamps[id] = readColumn(t, dataHome, `SELECT updated_at FROM memories WHERE id = ?`, id)
	}

	// The real gate, on the real corpus.
	runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply", "--skip-unchanged")

	for _, id := range ids {
		if got := readColumn(t, dataHome, `SELECT updated_at FROM memories WHERE id = ?`, id); got != stamps[id] {
			t.Errorf("%s: updated_at %q -> %q across the skip round — the second run consolidated instead "+
				"of skipping, so the project is reflected again in full", id, stamps[id], got)
		}
	}
	if n := countHistory(t, dataHome, "reflect"); n != 0 {
		t.Errorf("the two rounds left %d reflect history row(s), want none; a skip writes nothing", n)
	}
}

// corpusSignature is the value the --skip-unchanged gate compares: the same
// function, over the same consolidatable set, from the store the gate reads.
func corpusSignature(t *testing.T, dataHome, project string) string {
	t.Helper()
	s := openReflectStore(t, dataHome, project)
	defer func() { _ = s.Close() }()
	live, err := s.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	return reflection.InputSignature(consolidatable(live))
}

// TestRunReflectFingerprintIsStableAcrossRounds is the gate from the other side:
// run the lifecycle's own shape — apply, then skip, then apply-and-skip again —
// and the corpus' fingerprint must be the same string after every round. Before
// the fix each round restamped every row's updated_at, so the value the gate
// reads moved every round even though the corpus did not; a project whose only
// change was being looked at was therefore never provably unchanged.
func TestRunReflectFingerprintIsStableAcrossRounds(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	const project = "noopz3"
	seedAllKeepProject(t, dataHome, project)

	want := corpusSignature(t, dataHome, project)
	for round := range 3 {
		runReflectArgs(t, "reflect", project, "--tier", "sqlite", "--apply", "--skip-unchanged")
		if got := corpusSignature(t, dataHome, project); got != want {
			t.Fatalf("round %d: the corpus fingerprint moved from %q to %q although nothing in the corpus "+
				"changed", round, want, got)
		}
	}
}

// runReflectArgs drives the real command entry point, which is the only place
// the --skip-unchanged gate and the fingerprint recording are both reachable.
// It is called directly, never re-exec'd from os.Args[0].
func runReflectArgs(t *testing.T, args ...string) {
	t.Helper()
	orig := os.Args
	os.Args = append([]string{orig[0]}, args...)
	defer func() { os.Args = orig }()
	runReflect()
}
