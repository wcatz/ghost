package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A retention tier is the one claim a save can make that no lifecycle pass may
// take back, and the portable artifact is the documented way to move a store
// between machines. These tests pin the round trip end to end: the export
// carries the tier verbatim, an import restores it under --trust-provenance,
// and the default downgrades it to the durable tier exactly as it already
// downgrades source and pin — because an artifact is a file that arrived from
// somewhere, and on its own authority it must not be able to plant a
// keep-forever row.

// retentionFixture saves one memory per tier under p1 and returns them by tier.
func retentionFixture(t *testing.T, store *Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := map[string]string{}
	for _, tc := range []struct{ tier, content string }{
		{RetentionSession, "one conversation's fact"},
		{RetentionProject, "the durable default"},
		{RetentionPersistent, "a keep-forever fact"},
	} {
		id, _, _, err := store.UpsertWithOptions(ctx, "p1", "fact", tc.content, "mcp", 0.5, nil,
			UpsertOptions{Retention: tc.tier})
		if err != nil {
			t.Fatalf("UpsertWithOptions(%s): %v", tc.tier, err)
		}
		ids[tc.tier] = id
	}
	return ids
}

// TestPortableMemoriesCarryTheRetentionTier: the column the portable read
// selects is what the artifact carries, so a tier missing from the SELECT is a
// tier missing from the file — and a key the struct does not declare is a key
// an unmarshal drops, so a field only in Go is a field that never travelled.
func TestPortableMemoriesCarryTheRetentionTier(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	ids := retentionFixture(t, store)

	got, err := store.PortableMemories(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d memories, want one per tier", len(got))
	}
	byID := make(map[string]PortableMemory, len(got))
	for _, m := range got {
		byID[m.ID] = m
	}
	for tier, id := range ids {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("the %s memory is not in the portable read", tier)
		}
		if m.Retention != tier {
			t.Errorf("%s memory exported with retention %q, want %q", tier, m.Retention, tier)
		}
		// And on the wire, not only in the struct: the artifact is the thing
		// that crosses a machine boundary.
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if want := `"retention":"` + tier + `"`; !strings.Contains(string(raw), want) {
			t.Errorf("record does not carry %s on the wire: %s", want, raw)
		}
	}
}

// TestImportMemoryDowngradesRetentionUnlessTrusted: the tier rides the same
// flag as source and pin, and for the same reason. A keep-forever row planted
// by a file is a row no consolidation, resolve, supersede or prune can ever
// reach; a conversation tier planted by a file is a row scheduled for deletion
// by somebody else's clock. Both are neutralised the same way the planted
// provenance is — unless the run was told the artifact is the user's own.
func TestImportMemoryDowngradesRetentionUnlessTrusted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tier       string
		trust      bool
		wantTier   string
		wantExpiry bool
	}{
		{
			name: "a keep-forever row restored by its owner", tier: RetentionPersistent,
			trust: true, wantTier: RetentionPersistent,
		},
		{
			name: "a keep-forever row in a stranger's file", tier: RetentionPersistent,
			wantTier: RetentionProject,
		},
		{
			name: "a conversation row restored by its owner", tier: RetentionSession,
			trust: true, wantTier: RetentionSession, wantExpiry: true,
		},
		{
			name: "a conversation row in a stranger's file", tier: RetentionSession,
			wantTier: RetentionProject,
		},
		{
			name: "the durable default needs no rewrite", tier: RetentionProject,
			trust: true, wantTier: RetentionProject,
		},
		{
			// A v1 artifact states no tier, and it still imports: an absent
			// optional field is the documented no-change case.
			name: "a record that states no tier at all", tier: "",
			trust: true, wantTier: RetentionProject,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := portableTestStore(t)
			ctx := context.Background()
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			rec := PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "from an artifact",
				Source: "mcp", Retention: tc.tier,
				CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
			}
			created, _, downgraded, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true, TrustProvenance: tc.trust})
			if err != nil || !created {
				t.Fatalf("ImportMemory = (%v, %v); want created", created, err)
			}
			if downgraded != !tc.trust {
				t.Errorf("downgraded = %v, want %v — the tier is part of the same rewrite", downgraded, !tc.trust)
			}
			var gotTier string
			var gotExpiry sql.NullString
			if err := store.db.QueryRowContext(ctx,
				`SELECT retention, expires_at FROM memories WHERE id = 'm1'`).Scan(&gotTier, &gotExpiry); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if gotTier != tc.wantTier {
				t.Errorf("stored retention = %q, want %q", gotTier, tc.wantTier)
			}
			if gotExpiry.Valid != tc.wantExpiry {
				t.Errorf("expires_at present = %v (%q), want %v", gotExpiry.Valid, gotExpiry.String, tc.wantExpiry)
			}
			if tc.wantExpiry && gotExpiry.Valid {
				// A derived expiry, not the epoch and not a stale one: the row
				// is wanted from now, exactly as a save derives it.
				at, err := time.Parse("2006-01-02 15:04:05", gotExpiry.String)
				if err != nil {
					t.Fatalf("expires_at %q is not a stored timestamp: %v", gotExpiry.String, err)
				}
				if until := time.Until(at); until <= 0 || until > SessionTTL+time.Minute {
					t.Errorf("expires_at %s is %v away, want within %v of now", gotExpiry.String, until, SessionTTL)
				}
			}
		})
	}
}

// TestImportMemoryRefusesAnUnknownRetentionTier: a hand-edited artifact can
// carry a fourth tier, and the column's CHECK would refuse it at the INSERT
// with SQLite's own wording. The importer refuses it by name instead, the way
// it refuses an unknown category or source, and a dry run refuses it exactly as
// the apply run does.
func TestImportMemoryRefusesAnUnknownRetentionTier(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	rec := PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
		Retention: "forever",
	}
	for _, apply := range []bool{false, true} {
		created, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: apply})
		if err == nil {
			t.Fatalf("ImportMemory(apply=%v) accepted retention %q", apply, rec.Retention)
		}
		if created {
			t.Errorf("apply=%v reported creating a refused record", apply)
		}
		if !strings.Contains(err.Error(), "must be one of") {
			t.Errorf("error %q does not name the values the column accepts", err)
		}
		if !strings.Contains(err.Error(), "forever") {
			t.Errorf("error %q does not name the refused value", err)
		}
	}
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused tier wrote %d row(s)", n)
	}
	// And the predicate itself, which is the one `ghost export` calls: a record
	// holding a fourth tier must be named and left out of the artifact rather
	// than written and then rejected record by record on a restore (#813).
	if err := CheckImportedMemory(PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
		Retention: "forever",
	}); err == nil || !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("CheckImportedMemory with an unknown tier = %v, want a refusal naming the three tiers", err)
	}
	// A record that states no tier is not the same case, and must not be
	// refused: an absent tier is what every v1 artifact carries.
	if err := CheckImportedMemory(PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
	}); err != nil {
		t.Errorf("CheckImportedMemory with no tier stated = %v, want accepted", err)
	}
}

// TestARefusedImportNeverEchoesTheValueItRefused: CheckImportedMemory is a
// boundary that refuses arguments, and it refuses them BEFORE the secret guard
// further down — so the value is still whatever the file said. Its sentences
// are printed: the import report carries every rejected record's error and the
// CLI prints each one, which is the same route *SecretContentError exists to
// keep a credential off. Every value-set refusal therefore renders its value
// through RefusalArg, and this is the assertion that the boundary, not only the
// renderer, holds: a credential-shaped or oversized value is withheld, and an
// ordinary typo is still named because the diagnostic is the other half of the
// rule.
func TestARefusedImportNeverEchoesTheValueItRefused(t *testing.T) {
	oversized := strings.Repeat("z", 200*1024)
	for _, tc := range []struct {
		name  string
		value string
		token bool
		mut   func(*PortableMemory)
	}{
		{
			name: "a category holding a token", value: credentialToken, token: true,
			mut: func(m *PortableMemory) { m.Category = credentialToken },
		},
		{
			name: "a category longer than a refusal repeats", value: oversized,
			mut: func(m *PortableMemory) { m.Category = oversized },
		},
		{
			name: "a source holding a token", value: credentialToken, token: true,
			mut: func(m *PortableMemory) { m.Source = credentialToken },
		},
		{
			name: "a source longer than a refusal repeats", value: oversized,
			mut: func(m *PortableMemory) { m.Source = oversized },
		},
		{
			name: "a tier holding a token", value: credentialToken, token: true,
			mut: func(m *PortableMemory) { m.Retention = credentialToken },
		},
		{
			name: "a tier longer than a refusal repeats", value: oversized,
			mut: func(m *PortableMemory) { m.Retention = oversized },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := PortableMemory{ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp"}
			tc.mut(&m)
			// The predicate, which is the refusal `ghost export` also makes.
			err := CheckImportedMemory(m)
			if err == nil {
				t.Fatalf("CheckImportedMemory accepted %s", tc.name)
			}
			assertRefusalWithholds(t, err, tc.value)
			if tc.token && !strings.Contains(err.Error(), "withheld") {
				t.Errorf("the refusal did not say it withheld the credential: %v", err)
			}
			// And the import itself, which is the path whose errors the report
			// prints and the CLI reprints.
			store := portableTestStore(t)
			ctx := context.Background()
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			if _, _, _, err := store.ImportMemory(ctx, m, ImportOptions{Apply: true}); err == nil {
				t.Fatalf("ImportMemory accepted %s", tc.name)
			} else {
				assertRefusalWithholds(t, err, tc.value)
			}
		})
	}

	// The other half of the renderer, at the surface it exists for: an ordinary
	// typo is still NAMED, because a fix that stopped naming anything would close
	// the leak by taking the diagnostic with it.
	for _, m := range []PortableMemory{
		{ID: "m1", ProjectID: "p1", Category: "nope", Content: "x", Source: "mcp"},
		{ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "telepathy"},
		{ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp", Retention: "forever"},
	} {
		err := CheckImportedMemory(m)
		if err == nil {
			t.Fatalf("CheckImportedMemory accepted %+v", m)
		}
		want := m.Retention
		if m.Category != "fact" {
			want = m.Category
		} else if m.Source != "mcp" {
			want = m.Source
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a refusal no longer names the ordinary value %q it refused: %v", want, err)
		}
	}
}

// assertRefusalWithholds is the one rule every refusal on this boundary owes: no
// part of the value is in the sentence, and the sentence is bounded so an
// oversized argument cannot arrive back through it.
func assertRefusalWithholds(t *testing.T, err error, value string) {
	t.Helper()
	if strings.Contains(err.Error(), value) {
		t.Errorf("the refusal echoed the value it refused: %v", err)
	}
	// A refusal that echoed even the first 256 bytes of an oversized value would
	// slip past a Contains of the whole thing, so the run is what is asserted.
	if len(value) > ArgEchoMaxBytes && strings.Contains(err.Error(), value[:64]) {
		t.Errorf("the refusal echoed a prefix of an oversized value: %v", err)
	}
	if len(err.Error()) > 1024 {
		t.Errorf("the refusal is %d bytes — it repeated an argument: %v", len(err.Error()), err)
	}
}

// TestARestoredSessionRowStaysPrunable: the artifact carries no expiry —
// expires_at is derived for a session save and stated by nobody else — so a
// restore that put the tier back without deriving one would leave a row the
// prune can never reach, which is the growth the tier exists to bound. The
// import derives one, and a prune with the clock past it takes the row.
func TestARestoredSessionRowStaysPrunable(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	rec := PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "one conversation's fact",
		Source: "mcp", Retention: RetentionSession,
		CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
	}
	created, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true, TrustProvenance: true})
	if err != nil || !created {
		t.Fatalf("ImportMemory = (%v, %v); want created", created, err)
	}
	var expiry string
	if err := store.db.QueryRowContext(ctx,
		`SELECT expires_at FROM memories WHERE id = 'm1'`).Scan(&expiry); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if expiry == "" {
		t.Fatal("a restored session row has no expiry, so no prune can ever reach it")
	}
	at, err := time.Parse("2006-01-02 15:04:05", expiry)
	if err != nil {
		t.Fatalf("expires_at %q is not a stored timestamp: %v", expiry, err)
	}
	report, err := store.PruneSessionMemories(ctx, PruneOptions{
		Now:   at.Add(DefaultSessionGrace).Add(time.Minute),
		Apply: true,
	})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 {
		t.Fatalf("prune removed %v, want the restored session row", report.RemovedIDs)
	}
	var n int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE id = 'm1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a restored session row survived the prune that its tier promised")
	}
}

// TestAPersistentMemorySurvivesItsOwnExportAndImport: the issue's repro at the
// store level. A keep-forever row exported and restored by its owner comes back
// keep-forever, which is the whole point of carrying the column.
func TestAPersistentMemorySurvivesItsOwnExportAndImport(t *testing.T) {
	ctx := context.Background()
	src := portableTestStore(t)
	ids := retentionFixture(t, src)

	records, err := src.PortableMemories(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableMemories(src): %v", err)
	}
	var persistent PortableMemory
	for _, m := range records {
		if m.ID == ids[RetentionPersistent] {
			persistent = m
		}
	}
	if persistent.Retention != RetentionPersistent {
		t.Fatalf("the exported record carries %q, want %q", persistent.Retention, RetentionPersistent)
	}

	dst := portableTestStore(t)
	if err := dst.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject(dst): %v", err)
	}
	if _, _, _, err := dst.ImportMemory(ctx, persistent, ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	var got string
	if err := dst.db.QueryRowContext(ctx,
		`SELECT retention FROM memories WHERE id = ?`, persistent.ID).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != RetentionPersistent {
		t.Errorf("restored retention = %q, want %q — the one tier a default may not take back", got, RetentionPersistent)
	}
}
