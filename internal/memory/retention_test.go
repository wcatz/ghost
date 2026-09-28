package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v18Fixture builds a store at the schema version before retention tiers
// existed: the current initSQL with the tier's own index and its two columns
// removed, stamped at the version that migration runs from. The drop is the
// stand-in for "a v18 store" — a v18 release wrote exactly initSQL-without-those,
// and the alternative (a hand-copied CREATE TABLE) would be a second copy of the
// schema that can drift from the one it is claiming to describe.
func v18Fixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v18 db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// The index first: SQLite refuses to drop a column a partial index reads.
	for _, drop := range []string{
		`DROP INDEX IF EXISTS idx_memories_session_expiry`,
		`ALTER TABLE memories DROP COLUMN expires_at`,
		`ALTER TABLE memories DROP COLUMN retention`,
	} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v19-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
		 VALUES ('m-old', 'p1', 'gotcha', 'a fact from before retention tiers', 'mcp', 0.8, 1, '2026-01-02 03:04:05', '2026-01-03 03:04:05')`,
		`INSERT INTO memories (id, project_id, category, content, source)
		 VALUES ('m-bare', 'p1', 'fact', 'a fact nobody touched', 'reflection')`,
		`PRAGMA user_version = 18`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v18 db (%s): %v", s, err)
		}
	}
	return db, dbPath
}

func columnsOf(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[string]string{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		out[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	return out
}

// TestMigrateV19GivesEveryExistingRowTheProjectTier: a store that predates
// retention tiers has rows nobody ever classified, and the migration's job is to
// say so with the one value that is safe — the tier the corpus has always
// behaved as. A migration that instead wrote session, or left the column
// nullable, would either schedule a user's own memories for deletion or make
// every reader carry a NULL case for a value the schema says cannot be NULL.
func TestMigrateV19GivesEveryExistingRowTheProjectTier(t *testing.T) {
	db, _ := v18Fixture(t)

	if err := migrate(db, 18); err != nil {
		t.Fatalf("migrate v18->v19: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	cols := columnsOf(t, db, "memories")
	for _, c := range []string{"retention", "expires_at"} {
		if _, ok := cols[c]; !ok {
			t.Fatalf("memories has no %s column after migrateV19: %v", c, cols)
		}
	}

	for _, id := range []string{"m-old", "m-bare"} {
		var retention string
		var expiresAt sql.NullString
		if err := db.QueryRow(
			`SELECT retention, expires_at FROM memories WHERE id = ?`, id,
		).Scan(&retention, &expiresAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if retention != RetentionProject {
			t.Errorf("%s retention = %q, want %q — an existing row has no expiry and must persist until resolved", id, retention, RetentionProject)
		}
		if expiresAt.Valid {
			t.Errorf("%s expires_at = %q, want NULL — only a session row is scheduled for expiry", id, expiresAt.String)
		}
	}
}

// TestMigrateV19TouchesNothingButTheTier: "existing rows stay untouched" is a
// claim about every OTHER column too. A migration that rewrote created_at to
// now would age a two-year-old memory into a fresh one, and created_at is what
// decay ranks on.
func TestMigrateV19TouchesNothingButTheTier(t *testing.T) {
	db, _ := v18Fixture(t)

	type before struct {
		category, content, source, createdAt, updatedAt string
		importance                                      float64
		pinned                                          int
	}
	read := func(id string) before {
		t.Helper()
		var b before
		if err := db.QueryRow(`
			SELECT category, content, source, importance, pinned, created_at, updated_at
			FROM memories WHERE id = ?`, id,
		).Scan(&b.category, &b.content, &b.source, &b.importance, &b.pinned, &b.createdAt, &b.updatedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return b
	}
	was := read("m-old")

	if err := migrate(db, 18); err != nil {
		t.Fatalf("migrate v18->v19: %v", err)
	}

	got := read("m-old")
	if got != was {
		t.Errorf("migrateV19 changed a row it had no business touching:\n before %+v\n after  %+v", was, got)
	}
}

// TestFreshAndMigratedStoresAgreeOnTheTier: initSQL and migrateV19 are two
// copies of one DDL, and a fresh install and an upgraded store that disagree
// are indistinguishable from a bug until something reads the difference.
func TestFreshAndMigratedStoresAgreeOnTheTier(t *testing.T) {
	fresh, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB fresh: %v", err)
	}
	defer fresh.Close() //nolint:errcheck
	migrated, _ := v18Fixture(t)
	if err := migrate(migrated, 18); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	freshCols, migratedCols := columnsOf(t, fresh, "memories"), columnsOf(t, migrated, "memories")
	if len(freshCols) != len(migratedCols) {
		t.Errorf("memories has %d columns on a fresh store and %d on a migrated one", len(freshCols), len(migratedCols))
	}
	for name, typ := range freshCols {
		if migratedCols[name] != typ {
			t.Errorf("memories.%s is %q on a fresh store and %q on a migrated one", name, typ, migratedCols[name])
		}
	}

	// The index too, for the same reason: a partial index is what keeps a prune
	// off a full scan of a large corpus, and only a store that already had rows
	// at the migration would be missing it.
	for _, db := range []*sql.DB{fresh, migrated} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_memories_session_expiry'`,
		).Scan(&n); err != nil || n != 1 {
			t.Errorf("idx_memories_session_expiry missing after upgrade: n=%d err=%v", n, err)
		}
	}
}

// TestRetentionCheckRefusesATierTheVocabularyDoesNotHold: the CHECK is the last
// line, and it has to refuse the same set the Go vocabulary does — a value the
// schema accepts but no reader knows is a value nothing can filter.
func TestRetentionCheckRefusesATierTheVocabularyDoesNotHold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p-check", "/tmp/p-check", "p-check"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// The three tiers the vocabulary holds, which the CHECK must accept — the
	// other half of the same contract, and the half a narrowing CHECK breaks
	// first.
	for _, tier := range RetentionValues() {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, retention)
			VALUES ('p-check', 'fact', 'a row written behind the writers', 'mcp', ?)`, tier); err != nil {
			t.Errorf("retention = %q was refused by the schema: %v", tier, err)
		}
	}
	for _, tier := range []string{"forever", "SESSION", "project ", "global", "keep"} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, retention)
			VALUES ('p-check', 'fact', 'a row written behind the writers', 'mcp', ?)`, tier); err == nil {
			t.Errorf("retention = %q was accepted by the schema; only %v are tiers", tier, RetentionValues())
		}
	}
	// And the omitted column is project, read back rather than assumed.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (project_id, category, content, source)
		VALUES ('p-check', 'fact', 'a row nobody classified', 'mcp')`); err != nil {
		t.Fatalf("write a row with no tier: %v", err)
	}
	var retention string
	if err := s.db.QueryRowContext(ctx, `SELECT retention FROM memories WHERE project_id='p-check' AND content = 'a row nobody classified'`).Scan(&retention); err != nil {
		t.Fatalf("read the defaulted row: %v", err)
	}
	if retention != RetentionProject {
		t.Errorf("a row written without a tier reads %q, want %q", retention, RetentionProject)
	}
}

// TestUpsertRoundTripsTheRetentionTier: the save is the only place a caller
// states a tier, so a tier that does not come back out of the store is a tier
// nothing can filter on, decay by, or exempt.
func TestUpsertRoundTripsTheRetentionTier(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before := time.Now().UTC()

	for _, tier := range RetentionValues() {
		t.Run(tier, func(t *testing.T) {
			content := "a " + tier + " tier memory that has to read back as itself"
			id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: tier})
			if err != nil {
				t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
			}
			got, err := s.GetByIDs(ctx, []string{id})
			if err != nil {
				t.Fatalf("GetByIDs: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("GetByIDs returned %d rows, want 1", len(got))
			}
			if got[0].Retention != tier {
				t.Errorf("retention = %q, want %q", got[0].Retention, tier)
			}
			// expires_at is derived for a session row and stated by nobody else:
			// a durable row carrying an expiry is a claim nobody made about when
			// the user stops wanting it, and prune reads the column.
			switch tier {
			case RetentionSession:
				if got[0].ExpiresAt == nil {
					t.Fatalf("a session row has no expires_at: the tier's expiry is derived on save")
				}
				exp, err := time.Parse("2006-01-02 15:04:05", *got[0].ExpiresAt)
				if err != nil {
					t.Fatalf("parse expires_at %q: %v", *got[0].ExpiresAt, err)
				}
				if exp.Before(before.Add(SessionTTL-time.Minute)) || exp.After(before.Add(SessionTTL+time.Minute)) {
					t.Errorf("expires_at = %v, want about %v after the save", exp, SessionTTL)
				}
			default:
				if got[0].ExpiresAt != nil {
					t.Errorf("a %s row carries expires_at = %q; only a session row is scheduled for expiry", tier, *got[0].ExpiresAt)
				}
			}
		})
	}
}

// TestUpsertDefaultsToTheProjectTier: the zero UpsertOptions is what every
// caller that predates tiers passes, and it has to keep meaning what it meant.
func TestUpsertDefaultsToTheProjectTier(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a save that names no tier at all", "mcp", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.GetByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].Retention != RetentionProject {
		t.Errorf("retention = %q, want %q", got[0].Retention, RetentionProject)
	}
	if got[0].ExpiresAt != nil {
		t.Errorf("expires_at = %q, want NULL", *got[0].ExpiresAt)
	}
}

// TestUpsertRefusesAnUnknownTierBeforeWritingAnything: a bad tier is a caller's
// typo, and the answer has to name the vocabulary rather than failing the whole
// transaction with SQLite's own wording. Nothing may be written on the way to
// that error — not the row, and not the near-duplicate link a fold would have
// made.
func TestUpsertRefusesAnUnknownTierBeforeWritingAnything(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, bad := range []string{"forever", "SESSION", "session ", "keep", "global"} {
		_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a save whose tier is a typo: "+bad, "mcp", 0.6, nil, UpsertOptions{Retention: bad})
		if err == nil {
			t.Fatalf("retention %q was accepted, want a refusal naming the three tiers", bad)
		}
		if !strings.Contains(err.Error(), "session") ||
			!strings.Contains(err.Error(), "project") ||
			!strings.Contains(err.Error(), "persistent") {
			t.Errorf("error for %q does not name the vocabulary: %v", bad, err)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("error for %q does not quote the value that was refused: %v", bad, err)
		}
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE project_id = ?`, testProject).Scan(&rows); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if rows != 0 {
		t.Errorf("a refused tier wrote %d row(s)", rows)
	}
}

// TestUpsertRefusesAnUnknownTierOnAFoldToo: the fold path returns an existing id
// and writes a linked copy, so a refusal has to happen before either — a tier
// the fold silently dropped is a protection the caller was told about and did
// not get.
func TestUpsertRefusesAnUnknownTierOnAFold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "the near-duplicate probe and the tier are both validated before any statement runs"
	existing, _, _, err := s.Upsert(ctx, testProject, "fact", content, "mcp", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the near-duplicate probe and the tier are both validated before any statement", "mcp", 0.6, nil,
		UpsertOptions{Retention: "keep-forever"}); err == nil {
		t.Fatal("a fold with an unknown tier was accepted")
	}
	mems, err := s.GetAll(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(mems) != 1 || mems[0].ID != existing {
		t.Fatalf("store holds %d rows (first %q), want the single original %q", len(mems), firstID(mems), existing)
	}
}

func firstID(mems []Memory) string {
	if len(mems) == 0 {
		return ""
	}
	return mems[0].ID
}

// TestAFoldRaisesTheTiersItFoldsIntoAndNeverLowersIt: a near-duplicate save
// strengthens the row the corpus already holds and returns a new id, so the tier
// the caller stated has to reach the SURVIVING row or the protection is on a
// copy consolidation will absorb. It may only ever raise it: a session-scoped
// save that lowered a durable memory to its own tier would be a caller
// scheduling somebody else's memory for deletion.
func TestAFoldRaisesTheTiersItFoldsIntoAndNeverLowersIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("raises project to persistent", func(t *testing.T) {
		const content = "a durable fact that a second agent will restate as keep-forever"
		existing, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a durable fact that a second agent will restate as keep-forever for good", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionPersistent}); err != nil {
			t.Fatalf("fold as persistent: %v", err)
		}
		got := getOne(t, s, existing)
		if got.Retention != RetentionPersistent {
			t.Errorf("surviving row retention = %q, want %q — the fold target is the row consolidation absorbs", got.Retention, RetentionPersistent)
		}
	})

	t.Run("raises session to project", func(t *testing.T) {
		const content = "a session fact that turns out to be worth keeping"
		existing, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a session fact that turns out to be worth keeping past the session", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionProject}); err != nil {
			t.Fatalf("fold as project: %v", err)
		}
		got := getOne(t, s, existing)
		if got.Retention != RetentionProject {
			t.Errorf("surviving row retention = %q, want %q", got.Retention, RetentionProject)
		}
		if got.ExpiresAt != nil {
			t.Errorf("surviving row expires_at = %q, want NULL once it is no longer a session row", *got.ExpiresAt)
		}
	})

	t.Run("never lowers", func(t *testing.T) {
		const content = "a keep-forever fact restated as a passing detail of this session"
		existing, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: RetentionPersistent})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a keep-forever fact restated as a passing detail of this session only", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionSession}); err != nil {
			t.Fatalf("fold as session: %v", err)
		}
		got := getOne(t, s, existing)
		if got.Retention != RetentionPersistent {
			t.Errorf("surviving row retention = %q, want %q — a fold may raise a tier, never lower it", got.Retention, RetentionPersistent)
		}
		if got.ExpiresAt != nil {
			t.Errorf("surviving row expires_at = %q, want NULL: a persistent row has no expiry", *got.ExpiresAt)
		}
	})
}

func getOne(t *testing.T, s *Store, id string) Memory {
	t.Helper()
	got, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs(%s): %v", id, err)
	}
	if len(got) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows, want 1", id, len(got))
	}
	return got[0]
}

// TestRetentionValuesIsTheVocabularyEverySurfaceQuotes: the MCP tool's error
// text, the CLI usage and the schema CHECK all name the same three values, and
// they can only stay in step if they are told from one list.
func TestRetentionValuesIsTheVocabularyEverySurfaceQuotes(t *testing.T) {
	got := RetentionValues()
	want := []string{RetentionSession, RetentionProject, RetentionPersistent}
	if len(got) != len(want) {
		t.Fatalf("RetentionValues() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RetentionValues()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !IsValidRetention(got[i]) {
			t.Errorf("IsValidRetention(%q) = false for a value the list hands out", got[i])
		}
	}
	if IsValidRetention("") {
		t.Error("IsValidRetention(\"\") = true; the empty value is the default, not a tier")
	}
	// A caller that mutates the returned slice must not be able to rewrite the
	// vocabulary every other surface reads.
	got[0] = "mutated"
	if RetentionValues()[0] != RetentionSession {
		t.Error("RetentionValues hands out the package's own storage")
	}
}

// TestNormalizeRetentionIsTheOnlyPlaceTheDefaultIsApplied: "" means the caller
// said nothing, and there are exactly two readings of that — project, or a
// refusal. This is the one that reads it, so nothing else has to.
func TestNormalizeRetentionIsTheOnlyPlaceTheDefaultIsApplied(t *testing.T) {
	got, err := NormalizeRetention("")
	if err != nil {
		t.Fatalf("NormalizeRetention(\"\"): %v", err)
	}
	if got != RetentionProject {
		t.Errorf("NormalizeRetention(\"\") = %q, want %q", got, RetentionProject)
	}
	if _, err := NormalizeRetention("nope"); err == nil {
		t.Error("NormalizeRetention(\"nope\") returned no error")
	}
}

// TestInvalidRetentionErrorIsNotAStoreFailure: the MCP save tool reports the
// refusal, and it must be able to tell a caller's typo from a store that would
// not open.
func TestInvalidRetentionErrorIsNotAStoreFailure(t *testing.T) {
	err := InvalidRetentionError("keep")
	if err == nil {
		t.Fatal("InvalidRetentionError returned nil")
	}
	if !errors.Is(err, ErrInvalidRetention) {
		t.Errorf("error does not wrap ErrInvalidRetention: %v", err)
	}
	if !strings.Contains(err.Error(), "keep") {
		t.Errorf("error does not quote the value: %v", err)
	}
}
