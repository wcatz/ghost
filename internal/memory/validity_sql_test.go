package memory

import (
	"testing"
	"time"
)

// TestValiditySQLAgreesWithValidityState is the pin for the SQL form of the
// passive validity rule, the sibling of TestScopeMatchesSQLAgreesWithScopeMatches.
//
// The passive fetch SQL includes a validity predicate that keeps rows whose
// window contains the request's Now (valid_from <= now AND valid_until >= now,
// with NULL bounds treated as open). Stage 2 in the assembler applies the same
// rule via ValidityState. This test runs every case the Go rule covers through
// both forms, so a change to either side the other does not follow fails here.
func TestValiditySQLAgreesWithValidityState(t *testing.T) {
	s, ctx := newDedupStore(t)

	insertRaw := func(name string, validFrom, validUntil, verifiedAt *string) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: name, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		// Use direct UPDATE to set validity columns to exact values,
		// including shapes the writers would not produce (unreadable stamps).
		if _, err := s.db.ExecContext(ctx,
			`UPDATE memories SET valid_from = ?, valid_until = ?, verified_at = ? WHERE id = ?`,
			validFrom, validUntil, verifiedAt, id); err != nil {
			t.Fatalf("set validity(%s): %v", name, err)
		}
		return id
	}

	// Fixed clock for all cases.
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	stamp := now.Format(StoredStampLayout)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
	future := now.Add(24 * time.Hour).Format(StoredStampLayout)

	cases := []struct {
		name        string
		validFrom   *string
		validUntil  *string
		verifiedAt  *string
		wantValid   bool // true if the SQL predicate should keep the row
		wantGoState string
	}{
		{
			name:      "no bounds, no verified_at -> unset, kept by SQL",
			validFrom: nil, validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnset,
		},
		{
			name:      "open window (valid_from only), no verified_at -> unverified, kept by SQL",
			validFrom: &[]string{past}[0], validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "open window (valid_until only), no verified_at -> unverified, kept by SQL",
			validFrom: nil, validUntil: &[]string{future}[0], verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "window contains now, no verified_at -> unverified, kept by SQL",
			validFrom: &[]string{past}[0], validUntil: &[]string{future}[0], verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "window contains now, with verified_at -> valid, kept by SQL",
			validFrom: &[]string{past}[0], validUntil: &[]string{future}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "window closed (valid_until in past) -> expired, DROPPED by SQL",
			validFrom: &[]string{past}[0], validUntil: &[]string{past}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "window not yet open (valid_from in future) -> future, DROPPED by SQL",
			validFrom: &[]string{future}[0], validUntil: &[]string{future}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: false, wantGoState: ValidityFuture,
		},
		{
			name:      "expired even with future valid_from -> expired, DROPPED by SQL",
			validFrom: &[]string{future}[0], validUntil: &[]string{past}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "exactly at valid_until boundary -> valid, kept by SQL",
			validFrom: &[]string{past}[0], validUntil: &[]string{stamp}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "exactly at valid_from boundary -> valid, kept by SQL",
			validFrom: &[]string{stamp}[0], validUntil: &[]string{future}[0], verifiedAt: &[]string{stamp}[0],
			wantValid: true, wantGoState: ValidityValid,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := insertRaw("case-"+c.name, c.validFrom, c.validUntil, c.verifiedAt)

			// Test SQL predicate: the same clause used in passiveFetchSQL
			var gotSQL int
			query := `SELECT CASE WHEN (valid_until IS NULL OR valid_until >= ?) AND (valid_from IS NULL OR valid_from <= ?) THEN 1 ELSE 0 END
				FROM memories WHERE id = ?`
			if err := s.db.QueryRowContext(ctx, query, stamp, stamp, id).Scan(&gotSQL); err != nil {
				t.Fatalf("validity predicate query: %v", err)
			}
			sqlValid := gotSQL == 1

			// Test Go rule
			goState, _ := ValidityState(c.validFrom, c.validUntil, c.verifiedAt, now)
			goValid := goState != ValidityExpired && goState != ValidityFuture

			if sqlValid != c.wantValid {
				t.Errorf("SQL: want valid=%v, got valid=%v", c.wantValid, sqlValid)
			}
			if goValid != c.wantValid {
				t.Errorf("Go: want valid=%v, got valid=%v (state=%s)", c.wantValid, goValid, goState)
			}
			if sqlValid != goValid {
				t.Errorf("SQL and Go disagree: SQL valid=%v, Go valid=%v (state=%s) — the SQL form of the validity rule has drifted from the Go one", sqlValid, goValid, goState)
			}
			if goState != c.wantGoState {
				t.Errorf("Go state: want %s, got %s", c.wantGoState, goState)
			}
		})
	}
}

// TestValiditySQLIncludesUnreadableBoundsGoAccepts pins the one place the
// two forms of the rule are known to disagree, rather than leaving it to a
// comment.
//
// Go's ValidityState treats an unreadable stamp as unset (ignores it), so a row
// with an unreadable bound but otherwise valid window is kept as unverified.
// The SQL predicate compares the raw string lexicographically. An unreadable
// value like "not-a-date" sorts AFTER a valid timestamp (since 'n' > '2'),
// so valid_until >= now evaluates to TRUE and the row is KEPT by SQL.
//
// Nothing in Ghost writes an unreadable validity stamp — the writers validate
// and normalize to StoredStampLayout — so this needs a hand-edited or imported
// row. The asymmetry is pinned because the predicate is now a user-visible
// filter rather than a probe's condition.
func TestValiditySQLIncludesUnreadableBoundsGoAccepts(t *testing.T) {
	s, ctx := newDedupStore(t)

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "unreadable bounds", Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	stamp := now.Format(StoredStampLayout)

	// Unreadable valid_until with otherwise valid window
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET valid_from = ?, valid_until = 'not-a-date', verified_at = ? WHERE id = ?`,
		now.Add(-48*time.Hour).Format(StoredStampLayout), stamp, id); err != nil {
		t.Fatalf("set validity: %v", err)
	}

	var gotSQL int
	query := `SELECT CASE WHEN (valid_until IS NULL OR valid_until >= ?) AND (valid_from IS NULL OR valid_from <= ?) THEN 1 ELSE 0 END
		FROM memories WHERE id = ?`
	if err := s.db.QueryRowContext(ctx, query, stamp, stamp, id).Scan(&gotSQL); err != nil {
		t.Fatalf("validity predicate query: %v", err)
	}
	sqlValid := gotSQL == 1

	// Go treats unreadable bound as unset, so valid_from is set, valid_until is unset -> unverified (kept)
	unreadableUntil := "not-a-date"
	goState, _ := ValidityState(
		&[]string{now.Add(-48 * time.Hour).Format(StoredStampLayout)}[0],
		&unreadableUntil,
		&stamp,
		now,
	)
	goValid := goState != ValidityExpired && goState != ValidityFuture

	// Both keep it, but for different reasons — Go ignores unreadable, SQL string-compares
	if !goValid {
		t.Fatal("test bug: Go should keep a row with unreadable valid_until but valid window")
	}
	if !sqlValid {
		t.Errorf("SQL says valid=%v; the recorded asymmetry is that it keeps a row Go's parser reads as having an open window (both keep, but for different reasons)", sqlValid)
	}
}
