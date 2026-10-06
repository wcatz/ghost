package mcpinit

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)

// TestSessionStartOnAStoreBehindTheRetentionColumnStillRenders pins the floor for
// the tier column, and it is the same floor memories.scope has always had: the
// session-start block reads through memory.OpenReadDB, which runs no migration and
// cannot make a store current, so naming memories.retention on a store from before
// schema v19 fails the whole query with "no such column" — which the read takes as
// no rows. A user whose first session after the upgrade starts the hook would get a
// digest with its header, its tasks and its decisions and no memories, with nothing
// saying why. The ORDER BY needs the same treatment, so the two fall back together
// and the block is the one that store produced before tiers were read at all.
//
// The block is a caller of assemble.Run, so the substitution is not this package's
// to make any more: internal/memory's passiveColumnsFor owns it, and this test is
// what says the seam actually delivers the same block for a store that needs it. A
// test of the substitution itself lives next to the function that performs it.
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
		"- [convention] `tierold01` (0.9) «sign every commit with DCO»",
		"- [fact] `tierold02` (0.8) «the prod datastore is postgres»",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a store below the retention column's version must render as it did before tiers were read; %q missing from:\n%s", want, got)
		}
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

// TestTheSessionBlockKeepsAKeepForeverNearDuplicate: the two session-start
// loaders are where a protection map matters most, and the globals path is the
// sharper of the two — it does not merely reorder the near-duplicate loser, it
// FILTERS it out of the block, so a pin-only map there would drop a memory the
// user declared untouchable out of every later session while every other pass
// spared it.
func TestTheSessionBlockKeepsAKeepForeverNearDuplicate(t *testing.T) {
	projectPath, dbPath := scopeSession(t, []scopeRow{
		// The pair, with the keep-forever row the LOWER-ranked member, which is the
		// only shape in which the demotion decides anything.
		{id: "protkeep1", category: "convention", content: "the tunnel mtu is 1400 in staging", importance: 0.9},
		{id: "protkeep2", category: "convention", content: "the tunnel mtu is 1400 while we debug", importance: 0.3},
	}, []scopeRow{
		{id: "protglob1", category: "convention", content: "the shared rule about running migrations", importance: 0.9},
		{id: "protglob2", category: "convention", content: "the shared rule about running migrations restated", importance: 0.3},
	})

	// One read-write open for both writes. memory.OpenDB would migrate a store it
	// found behind, and this one is current, so it only adds the row the link needs.
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	for _, id := range []string{"protkeep2", "protglob2"} {
		if _, err := db.ExecContext(ctx, `UPDATE memories SET retention = 'persistent' WHERE id = ?`, id); err != nil {
			t.Fatalf("mark %s keep-forever: %v", id, err)
		}
	}
	store := memory.NewStore(db, nil)
	for _, pair := range [][2]string{{"protkeep1", "protkeep2"}, {"protglob1", "protglob2"}} {
		if err := store.CreateLink(ctx, pair[0], pair[1], "duplicate", 0.99, "auto"); err != nil {
			t.Fatalf("CreateLink(%s,%s): %v", pair[0], pair[1], err)
		}
	}

	got := renderSessionStart(t, projectPath)
	for _, want := range []string{
		"the tunnel mtu is 1400 while we debug",
		"the shared rule about running migrations restated",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the session block dropped a keep-forever memory: %q missing from:\n%s", want, got)
		}
	}
}
