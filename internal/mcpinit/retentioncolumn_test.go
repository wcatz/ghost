package mcpinit

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)

// TestSessionStartOnAStoreBehindTheRetentionColumnStillRenders pins the floor for
// the tier column, and it is the same floor memories.scope has always had: the
// session-start loaders read through memory.OpenReadDB, which runs no migration
// and cannot make a store current, so naming memories.retention on a store from
// before schema v19 fails the whole query with "no such column" — which
// loadSessionContext reads as no rows. A user whose first session after the
// upgrade starts the hook would get a digest with its header, its tasks and its
// decisions and no memories, with nothing saying why. The ORDER BY needs the same
// treatment (memory.DecayRankingSQL reads the column too), so the two fall back
// together and the block is the one that store produced before tiers were read at
// all.
//
// The fixture makes the store what a pre-v19 one is: the stamp is back to 18 AND
// the column is gone. Both, for the reason the scope fixture above gives — the
// stamp alone leaves the column physically present, and the column alone leaves
// the stamp current.
func TestSessionStartOnAStoreBehindTheRetentionColumnStillRenders(t *testing.T) {
	projectPath, dbPath := scopeSession(t, []scopeRow{
		{id: "tierold01", category: "convention", content: "sign every commit with DCO", importance: 0.9},
		{id: "tierold02", category: "fact", content: "the prod datastore is postgres", importance: 0.8},
	}, nil)

	stamper, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open for rewriting the store: %v", err)
	}
	// The partial index reads the column, so it goes first — the same order
	// migrate.go's own reverse would need.
	if _, err := stamper.Exec(`DROP INDEX IF EXISTS idx_memories_session_expiry`); err != nil {
		t.Fatalf("drop the session-expiry index: %v", err)
	}
	if _, err := stamper.Exec(`ALTER TABLE memories DROP COLUMN expires_at`); err != nil {
		t.Fatalf("drop the expires_at column: %v", err)
	}
	if _, err := stamper.Exec(`ALTER TABLE memories DROP COLUMN retention`); err != nil {
		t.Fatalf("drop the retention column: %v", err)
	}
	if _, err := stamper.Exec(`PRAGMA user_version = 18`); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	// On the handle that made the change, before it is closed: the fixture's whole
	// claim is that the column is gone, so an ALTER that became a no-op must fail
	// here rather than in the assertions below.
	if _, err := stamper.Exec(`SELECT retention FROM memories`); err == nil {
		t.Fatal("the fixture must leave the store without a retention column, or it proves nothing")
	}
	if err := stamper.Close(); err != nil {
		t.Fatalf("close stamper: %v", err)
	}
	readBack, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer readBack.Close() //nolint:errcheck
	if v, err := memory.DBUserVersion(readBack); err != nil || v != 18 {
		t.Fatalf("user_version = %d (err %v), want 18", v, err)
	}

	got := renderSessionStart(t, projectPath)
	for _, want := range []string{
		"- [convention] «sign every commit with DCO»",
		"- [fact] «the prod datastore is postgres»",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a store below the retention column's version must render as it did before tiers were read; %q missing from:\n%s", want, got)
		}
	}
}

// TestRetentionColumnExprFallsBackOnAStoreBehindIt is the seam the render above
// depends on, pinned directly: a store at or past the floor names the column, and
// one below it selects a NULL literal and says so, because a caller that ORDERS by
// the column cannot name a column it is not selecting.
func TestRetentionColumnExprFallsBackOnAStoreBehindIt(t *testing.T) {
	_, dbPath := scopeSession(t, []scopeRow{
		{id: "tierexpr01", category: "fact", content: "a row to make the store non-empty", importance: 0.5},
	}, nil)

	current, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	expr, hasTier := retentionColumnExpr(current)
	if !hasTier || expr != "retention" {
		t.Errorf("a current store got (%q, %v), want the column", expr, hasTier)
	}
	if err := current.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stamper, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open for rewriting: %v", err)
	}
	if _, err := stamper.Exec(`PRAGMA user_version = 18`); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	if err := stamper.Close(); err != nil {
		t.Fatalf("close stamper: %v", err)
	}

	behind, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer behind.Close() //nolint:errcheck
	expr, hasTier = retentionColumnExpr(behind)
	if hasTier {
		t.Error("a store below the floor was told it has the column")
	}
	if !strings.Contains(expr, "NULL AS retention") {
		t.Errorf("the fallback is %q, want a NULL literal the ORDER BY can share", expr)
	}
}

// TestDecayRankingSQLWithTierKeepsTheCategoryHalf: the fallback expression exists
// so a reader that cannot name the column still ranks the way it always did, which
// means the category half has to be the same expression. A hand-copied second
// version is what would drift, so this pins that the two differ by the tier
// factor and by nothing else.
func TestDecayRankingSQLWithTierKeepsTheCategoryHalf(t *testing.T) {
	withTier := memory.DecayRankingSQL
	withoutTier := memory.DecayRankingSQLWithTier(false)
	if withTier != memory.DecayRankingSQLWithTier(true) {
		t.Error("DecayRankingSQL is not the with-tier form of the same template")
	}
	if withTier == withoutTier {
		t.Fatal("the two forms are identical, so the tier half is not in either")
	}
	// The category curve is the shared part: every term of it is present in both.
	for _, term := range []string{
		"WHEN pinned = 1 THEN 1.0",
		"WHEN category IN ('preference', 'convention', 'fact') THEN 1.0",
		"WHEN category IN ('pattern', 'architecture') THEN",
		"MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))",
		"MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))",
	} {
		if !strings.Contains(withTier, term) || !strings.Contains(withoutTier, term) {
			t.Errorf("the category half is not shared: %q is missing from one of the two forms", term)
		}
	}
	if strings.Contains(withoutTier, "retention") {
		t.Errorf("the fallback form still names the column a pre-v19 store does not have:\n%s", withoutTier)
	}
}
