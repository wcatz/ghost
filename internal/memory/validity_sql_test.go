package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// setRawValidity writes the three validity columns to exact values through SQL,
// including shapes the writers would refuse (unreadable stamps), because the rule
// the two forms must agree on is defined over what the COLUMN can hold, not over
// what a current writer happens to produce.
//
// A NIL bound is written as SQL NULL; a NON-NIL empty string is written as the
// empty string. Those are different column values and the rule treats them the
// same (no claim), so collapsing them here would leave the empty-string edge
// untested while the table claimed to cover it.
func setRawValidity(t *testing.T, s *Store, ctx context.Context, id string, from, until, verified *string) {
	t.Helper()
	ns := func(p *string) sql.NullString {
		if p == nil {
			return sql.NullString{}
		}
		return sql.NullString{String: *p, Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET valid_from = ?, valid_until = ?, verified_at = ? WHERE id = ?`,
		ns(from), ns(until), ns(verified), id); err != nil {
		t.Fatalf("set validity(%s): %v", id, err)
	}
}

// TestValiditySQLAgreesWithValidityState pins the SQL form of the passive validity
// rule to ValidityState. Every case runs through BOTH forms, the SQL through the
// production builder validityMatchesSQL, so a change to either side the other does
// not follow fails on the row that changed. The SQL may keep a row Go drops (a
// stored fraction cut to the whole second Now is bound at) but never the reverse.
func TestValiditySQLAgreesWithValidityState(t *testing.T) {
	s, ctx := newDedupStore(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	stamp := now.Format(StoredStampLayout)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
	future := now.Add(24 * time.Hour).Format(StoredStampLayout)
	p := func(v string) *string { return &v }

	cases := []struct {
		name                  string
		validFrom, validUntil *string
		verifiedAt            *string
		wantValid             bool // true if the SQL predicate should keep the row
		wantGoState           string
		sqlOverAdmits         bool
	}{
		{"no bounds -> unset, kept", nil, nil, nil, true, ValidityUnset, false},
		{"valid_from only -> unverified, kept", p(past), nil, nil, true, ValidityUnverified, false},
		{"valid_until only -> unverified, kept", nil, p(future), nil, true, ValidityUnverified, false},
		{"window contains now, verified -> valid", p(past), p(future), p(stamp), true, ValidityValid, false},
		{"window closed -> expired, dropped", p(past), p(past), p(stamp), false, ValidityExpired, false},
		{"window not yet open -> future, dropped", p(future), p(future), p(stamp), false, ValidityFuture, false},
		{"expired even with future valid_from", p(future), p(past), p(stamp), false, ValidityExpired, false},
		{"valid_until exactly at now -> kept", p(past), p(stamp), p(stamp), true, ValidityValid, false},
		{"valid_from exactly at now -> kept", p(stamp), p(future), p(stamp), true, ValidityValid, false},
		// An RFC 3339 valid_from sorts AFTER a leading '2', so a raw string comparison
		// would drop it while Go reads it as no claim.
		{"unreadable valid_from sorts after now -> no claim, kept", p("2026-12-31T00:00:00Z"), nil, nil, true, ValidityUnset, false},
		// An unreadable valid_until that sorts BEFORE now would be dropped by a raw
		// string comparison.
		{"unreadable valid_until sorts before now -> kept", p(past), p("0000-00-00"), nil, true, ValidityUnverified, false},
		{"empty valid_until -> no claim, kept", p(past), p(""), nil, true, ValidityUnverified, false},
		{"range-invalid valid_until -> no claim, kept", p(past), p("2026-02-30"), nil, true, ValidityUnverified, false},
		// Fractional-second bounds are READABLE and must be judged.
		{"fractional valid_until .5 in the past -> expired", p(past), p("2026-09-27 11:00:00.5"), p(stamp), false, ValidityExpired, false},
		{"fractional valid_until with comma in the past -> expired", p(past), p("2026-09-27 11:00:00,5"), p(stamp), false, ValidityExpired, false},
		{"fractional valid_until just before now -> expired", p(past), p("2026-09-27 11:59:59.9"), p(stamp), false, ValidityExpired, false},
		{"fractional valid_until .9 after now's second -> kept", p(past), p("2026-09-27 12:00:00.9"), p(stamp), true, ValidityValid, false},
		{"fractional valid_until .000 exactly at now -> kept", p(past), p("2026-09-27 12:00:00.000"), p(stamp), true, ValidityValid, false},
		{"fractional valid_from .000 exactly at now -> kept", p("2026-09-27 12:00:00.000"), p(future), p(stamp), true, ValidityValid, false},
		{"fractional valid_from .5 after now -> future, SQL over-admits", p("2026-09-27 12:00:00.5"), p(future), p(stamp), false, ValidityFuture, true},
		{"fractional valid_from in the future -> dropped", p("2026-09-28 12:00:00,5"), p(future), p(stamp), false, ValidityFuture, false},
		{"fractional valid_until with trailing text is unreadable", p(past), p("2026-09-27 11:00:00.5x"), nil, true, ValidityUnverified, false},
		{"valid_until with a bare separator is unreadable", p(past), p("2026-09-27 11:00:00."), nil, true, ValidityUnverified, false},
		{"unreadable valid_until, readable window -> valid", p(past), p("9999-99-99"), p(stamp), true, ValidityValid, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "case-" + c.name, Source: "manual", Importance: 0.7,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			setRawValidity(t, s, ctx, id, c.validFrom, c.validUntil, c.verifiedAt)

			var got int
			query := `SELECT CASE WHEN ` + validityMatchesSQL() + ` THEN 1 ELSE 0 END FROM memories WHERE id = ?`
			if err := s.db.QueryRowContext(ctx, query, stamp, stamp, id).Scan(&got); err != nil {
				t.Fatalf("validity predicate query: %v", err)
			}
			sqlValid := got == 1

			goState, _ := ValidityState(c.validFrom, c.validUntil, c.verifiedAt, now)
			goValid := goState != ValidityExpired && goState != ValidityFuture
			if sqlValid != (c.wantValid || c.sqlOverAdmits) {
				t.Errorf("SQL valid=%v, want %v", sqlValid, c.wantValid || c.sqlOverAdmits)
			}
			if goValid != c.wantValid || goState != c.wantGoState {
				t.Errorf("Go state %s (valid=%v), want %s (valid=%v)", goState, goValid, c.wantGoState, c.wantValid)
			}
			// Production Now carries a sub-second part while the SQL binds it truncated:
			// the SQL must be a superset of Go's verdict at any such Now.
			for _, frac := range []time.Duration{300 * time.Millisecond, 999 * time.Millisecond} {
				fstate, _ := ValidityState(c.validFrom, c.validUntil, c.verifiedAt, now.Add(frac))
				if fstate != ValidityExpired && fstate != ValidityFuture && !sqlValid {
					t.Errorf("fractional Now +%s: Go calls the row %s but the SQL drops it", frac, fstate)
				}
			}
		})
	}
}

// TestPassiveWindowIsNotFilledByExpiredRows is the reproduction from #892 against
// the real store: more expired, high-importance rows than the over-fetch sits
// AHEAD of one valid, low-importance row in the ranking. Filtering validity after
// the LIMIT would fill the whole window with the expired rows and return nothing;
// the valid row has to be the one that comes back. The count is larger than the
// over-fetch on purpose, so a window that merely happens to be big enough cannot
// pass this by accident.
func TestPassiveWindowIsNotFilledByExpiredRows(t *testing.T) {
	s, ctx := newDedupStore(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
	future := now.Add(24 * time.Hour).Format(StoredStampLayout)

	const overFetch = 20
	for i := 0; i < overFetch+10; i++ {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "expired-" + itoa(i), Source: "manual", Importance: 0.9,
		})
		if err != nil {
			t.Fatalf("Create expired %d: %v", i, err)
		}
		setRawValidity(t, s, ctx, id, strPtr(past), strPtr(past), nil)
	}
	validID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the one valid row", Source: "manual", Importance: 0.1,
	})
	if err != nil {
		t.Fatalf("Create valid: %v", err)
	}
	setRawValidity(t, s, ctx, validID, strPtr(past), strPtr(future), nil)

	pol := projectPassivePolicy()
	pol.Bucket = testProject
	pol.OverFetch = overFetch
	req := passiveRequest(testProject, pol)
	req.Now = now
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := passiveIDs(set)
	if len(ids) != 1 || ids[0] != validID {
		t.Fatalf("passive window = %v; want exactly the valid row %s (expired rows must not spend the over-fetch)", ids, validID)
	}
	if set.ValidityExcluded != 0 {
		t.Errorf("ValidityExcluded = %d on a non-empty window; the probe runs only for an empty bucket", set.ValidityExcluded)
	}
}

// TestPassiveValidityExcludedCountsAnEntirelyInvalidBucket pins the verdict input
// for the two surfaces that have to say "withheld as out of date" rather than
// "nothing was saved": a project bucket and a `_global` bucket whose every row
// the validity predicate removed. It is called directly, and through Candidates,
// because the count is only worth anything if the set carries it.
func TestPassiveValidityExcludedCountsAnEntirelyInvalidBucket(t *testing.T) {
	s, ctx := newDedupStore(t)
	if err := s.EnsureProject(ctx, GlobalProjectID, "", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
	future := now.Add(24 * time.Hour).Format(StoredStampLayout)

	// Two buckets, each entirely invalid: one expired row and one not yet open.
	for _, bucket := range []string{testProject, GlobalProjectID} {
		for name, win := range map[string][2]string{"expired": {past, past}, "future": {future, future}} {
			id, err := s.Create(ctx, bucket, Memory{
				Category: "fact", Content: bucket + "-" + name, Source: "manual", Importance: 0.7,
			})
			if err != nil {
				t.Fatalf("Create(%s,%s): %v", bucket, name, err)
			}
			setRawValidity(t, s, ctx, id, strPtr(win[0]), strPtr(win[1]), nil)
		}
	}
	// A resolved row and a row in another bucket must not be counted as exclusions.
	other, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "resolved", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create resolved: %v", err)
	}
	setRawValidity(t, s, ctx, other, strPtr(past), strPtr(past), nil)
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET resolved_at = ? WHERE id = ?`, past, other); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	cols, err := passiveColumnsFor(s)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
		pol.Bucket = map[string]string{"proj": testProject, "_global": GlobalProjectID}[pol.Bucket]
		req := passiveRequest(testProject, pol)
		req.Now = now

		n, capped, err := s.passiveValidityExcluded(ctx, req, pol, cols)
		if err != nil {
			t.Fatalf("passiveValidityExcluded(%s): %v", pol.Bucket, err)
		}
		if n != 2 || capped {
			t.Errorf("passiveValidityExcluded(%s) = %d (capped=%v); want 2 (the expired and the not-yet-open row), not capped", pol.Bucket, n, capped)
		}

		set, err := s.Candidates(ctx, req)
		if err != nil {
			t.Fatalf("Candidates(%s): %v", pol.Bucket, err)
		}
		if len(set.Rows) != 0 {
			t.Errorf("Candidates(%s) returned %d rows; every row is out of its window", pol.Bucket, len(set.Rows))
		}
		if set.ValidityExcluded != 2 {
			t.Errorf("CandidateSet.ValidityExcluded(%s) = %d; want 2", pol.Bucket, set.ValidityExcluded)
		}
	}
}

// TestPassiveValidityExcludedStopsAtTheCap pins the bound: a bucket holding more
// expired rows than the cap reports the cap, not the bucket, and says so.
func TestPassiveValidityExcludedStopsAtTheCap(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p','/p','p')`); err != nil {
		t.Fatal(err)
	}
	n := PassiveEligibleCountCap + 100
	if _, err := db.Exec(`
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < ?)
		INSERT INTO memories (id, project_id, category, content, source, importance, valid_until, expires_at)
		SELECT 'bulk' || i, 'p', 'fact', 'c', 'manual', 0.5, '2020-01-01 00:00:00', '2020-01-01 00:00:00' FROM seq`, n); err != nil {
		t.Fatal(err)
	}
	st := NewStore(db, nil)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatal(err)
	}
	pol := SlicePolicy{Bucket: "p", Order: OrderDecay, OverFetch: 20}
	req := CandidateRequest{ProjectID: "p", Mode: ProjectScoped, Now: now, Condition: CondHybrid, Passive: []SlicePolicy{pol}}
	got, capped, err := st.passiveValidityExcluded(context.Background(), req, pol, cols)
	if err != nil {
		t.Fatal(err)
	}
	if got != PassiveEligibleCountCap || !capped {
		t.Fatalf("passiveValidityExcluded = %d (capped=%v), want %d capped=true", got, capped, PassiveEligibleCountCap)
	}
	set, err := st.Candidates(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if set.ValidityExcluded != PassiveEligibleCountCap {
		t.Fatalf("CandidateSet.ValidityExcluded = %d, want %d", set.ValidityExcluded, PassiveEligibleCountCap)
	}

}
