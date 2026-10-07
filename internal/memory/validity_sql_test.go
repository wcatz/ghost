package memory

import (
	"context"
	"database/sql"
	"strings"
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

// TestValiditySQLAgreesWithValidityState is the pin for the SQL form of the
// passive validity rule, the sibling of TestScopeMatchesSQLAgreesWithScopeMatches.
//
// The passive fetch keeps rows whose window contains the request's Now
// (valid_from <= now AND valid_until >= now, with NULL and unreadable bounds
// treated as no claim). Stage 2 applies the same rule through ValidityState. This
// test runs every case through BOTH forms — the SQL through the production
// builder `validityMatchesSQL`, not a predicate retyped here — so a change to
// either side the other does not follow fails on the row that changed.
func TestValiditySQLAgreesWithValidityState(t *testing.T) {
	s, ctx := newDedupStore(t)

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
		// sqlOverAdmits marks the one direction the predicate may differ in: it keeps
		// a row Go drops (a stored fraction cut to the whole second Now is bound at),
		// which stage 2 then withholds. The reverse is never allowed.
		sqlOverAdmits bool
	}{
		{
			name:      "no bounds, no verified_at -> unset, kept",
			validFrom: nil, validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnset,
		},
		{
			name:      "open window (valid_from only), no verified_at -> unverified, kept",
			validFrom: strPtr(past), validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "open window (valid_until only), no verified_at -> unverified, kept",
			validFrom: nil, validUntil: strPtr(future), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "window contains now, no verified_at -> unverified, kept",
			validFrom: strPtr(past), validUntil: strPtr(future), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "window contains now, with verified_at -> valid, kept",
			validFrom: strPtr(past), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "window closed (valid_until in past) -> expired, dropped",
			validFrom: strPtr(past), validUntil: strPtr(past), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "window not yet open (valid_from in future) -> future, dropped",
			validFrom: strPtr(future), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityFuture,
		},
		{
			name:      "expired even with future valid_from -> expired, dropped",
			validFrom: strPtr(future), validUntil: strPtr(past), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "valid_until exactly at now -> kept (a closed window)",
			validFrom: strPtr(past), validUntil: strPtr(stamp), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "valid_from exactly at now -> kept",
			validFrom: strPtr(stamp), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		// The unreadable-bound direction the reviewer named: an RFC 3339
		// valid_from sorts AFTER a leading '2', so a raw string comparison drops
		// it (`valid_from <= now` false) while Go reads it as no claim and keeps
		// it. The SQL must keep it too.
		{
			name:      "unreadable valid_from, RFC 3339, sorts after now -> no claim, kept",
			validFrom: strPtr("2026-12-31T00:00:00Z"), validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnset,
		},
		// The other direction: an unreadable valid_until that sorts BEFORE now
		// (`0000-00-00`) would be dropped by a raw string comparison
		// (`valid_until >= now` false) while Go treats it as no claim.
		{
			name:      "unreadable valid_until, sorts before now -> no claim, kept",
			validFrom: strPtr(past), validUntil: strPtr("0000-00-00"), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "unreadable valid_from, not-a-date, sorts after now -> no claim, kept",
			validFrom: strPtr("not-a-date"), validUntil: nil, verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnset,
		},
		{
			name:      "empty valid_until -> no claim, kept",
			validFrom: strPtr(past), validUntil: strPtr(""), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "range-invalid valid_until (2026-02-30) -> no claim, kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-02-30"), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		// Fractional-second bounds. Go's time.Parse accepts a separator and one or
		// more digits after the seconds field, so these are READABLE and must be
		// judged, not treated as no claim.
		{
			name:      "fractional valid_until .000 in the past -> expired, dropped",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:00:00.000"), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "fractional valid_until .5 in the past -> expired, dropped",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:00:00.5"), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "fractional valid_until ,5 (comma) in the past -> expired, dropped",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:00:00,5"), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "fractional valid_until just before now (11:59:59.9) -> expired, dropped",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:59:59.9"), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityExpired,
		},
		{
			name:      "fractional valid_until .9 after now's second -> kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 12:00:00.9"), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "fractional valid_until .000 exactly at now -> kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 12:00:00.000"), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "fractional valid_until .5 after now's second -> kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 12:00:00.5"), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "fractional valid_from .000 exactly at now -> kept",
			validFrom: strPtr("2026-09-27 12:00:00.000"), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "fractional valid_from .5 after now -> future, SQL over-admits by truncation",
			validFrom: strPtr("2026-09-27 12:00:00.5"), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityFuture, sqlOverAdmits: true,
		},
		{
			name:      "fractional valid_from in the future (,5) -> future, dropped",
			validFrom: strPtr("2026-09-28 12:00:00,5"), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: false, wantGoState: ValidityFuture,
		},
		{
			name:      "fractional valid_from in the past -> kept",
			validFrom: strPtr("2026-09-27 11:00:00.5"), validUntil: strPtr(future), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
		{
			name:      "fractional valid_until with trailing text is unreadable -> no claim, kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:00:00.5x"), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "valid_until with a bare separator and no digits is unreadable -> kept",
			validFrom: strPtr(past), validUntil: strPtr("2026-09-27 11:00:00."), verifiedAt: nil,
			wantValid: true, wantGoState: ValidityUnverified,
		},
		{
			name:      "unreadable valid_until, readable window -> valid, kept",
			validFrom: strPtr(past), validUntil: strPtr("9999-99-99"), verifiedAt: strPtr(stamp),
			wantValid: true, wantGoState: ValidityValid,
		},
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

			// The production predicate, called rather than retyped.
			var got int
			query := `SELECT CASE WHEN ` + validityMatchesSQL() + ` THEN 1 ELSE 0 END
				FROM memories WHERE id = ?`
			if err := s.db.QueryRowContext(ctx, query, stamp, stamp, id).Scan(&got); err != nil {
				t.Fatalf("validity predicate query: %v", err)
			}
			sqlValid := got == 1

			goState, _ := ValidityState(c.validFrom, c.validUntil, c.verifiedAt, now)
			goValid := goState != ValidityExpired && goState != ValidityFuture

			wantSQL := c.wantValid || c.sqlOverAdmits
			if sqlValid != wantSQL {
				t.Errorf("SQL: want valid=%v, got valid=%v", wantSQL, sqlValid)
			}
			if goValid != c.wantValid {
				t.Errorf("Go: want valid=%v, got valid=%v (state=%s)", c.wantValid, goValid, goState)
			}
			if sqlValid != goValid && !c.sqlOverAdmits {
				t.Errorf("SQL and Go disagree: SQL valid=%v, Go valid=%v (state=%s) — the SQL form of "+
					"the validity rule has drifted from the one rule", sqlValid, goValid, goState)
			}
			// Production Now carries a sub-second part, while the SQL binds it
			// truncated to the second. The SQL must be a SUPERSET of Go's verdict
			// at any such Now: it may keep a row Go drops, never drop one Go keeps.
			for _, frac := range []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 999 * time.Millisecond} {
				fnow := now.Add(frac)
				fstate, _ := ValidityState(c.validFrom, c.validUntil, c.verifiedAt, fnow)
				if fstate != ValidityExpired && fstate != ValidityFuture && !sqlValid {
					t.Errorf("fractional Now +%s: Go calls the row %s but the SQL drops it", frac, fstate)
				}
			}
			if goState != c.wantGoState {
				t.Errorf("Go state: want %s, got %s", c.wantGoState, goState)
			}
		})
	}
}

// TestPassiveFetchSQLUsesTheValidityBuilder stops the fetch from drifting off the
// builder the pin above calls: the clause has to be in the statement the store
// actually runs, not merely equal to one a test also runs.
func TestPassiveFetchSQLUsesTheValidityBuilder(t *testing.T) {
	s, _ := newDedupStore(t)
	cols, err := passiveColumnsFor(s)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	if !cols.HasValidity {
		t.Fatalf("fixture: the store must have the validity columns for this to test anything")
	}
	q, args := passiveFetchSQL(projectPassivePolicy(), passiveRequest("proj"), cols)
	if !strings.Contains(q, validityMatchesSQL()) {
		t.Errorf("the passive fetch does not use validityMatchesSQL, so the pin tests a predicate the "+
			"fetch does not run:\n%s", q)
	}
	// Two bindings for the predicate, plus the bucket: a query whose clause is
	// present but whose stamps are never bound fails at run time with "missing
	// argument with index".
	if !strings.Contains(q, "?") {
		t.Fatal("the validity predicate carries no placeholder")
	}
	if len(args) < 3 {
		t.Errorf("args = %d; want at least the bucket and the two validity stamps", len(args))
	}
}

// TestPassiveReadAdmitsExactlyTheValidRows is the end-to-end form of the pin: the
// rows the store RETURNS from a passive read are exactly the rows Go's rule keeps,
// over a set that mixes valid, expired, future and unreadable windows. The
// direct-builder test above can pass while a wiring mistake (an unbound stamp, the
// predicate applied to the wrong column) drops or admits the wrong row here.
func TestPassiveReadAdmitsExactlyTheValidRows(t *testing.T) {
	s, ctx := newDedupStore(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	stamp := now.Format(StoredStampLayout)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
	future := now.Add(24 * time.Hour).Format(StoredStampLayout)

	cases := []struct {
		name       string
		validFrom  *string
		validUntil *string
		wantKept   bool
	}{
		{"kept-unset", nil, nil, true},
		{"kept-open-from", strPtr(past), nil, true},
		{"kept-open-until", nil, strPtr(future), true},
		{"kept-window", strPtr(past), strPtr(future), true},
		{"kept-boundary-until", strPtr(past), strPtr(stamp), true},
		{"dropped-expired", strPtr(past), strPtr(past), false},
		{"dropped-future", strPtr(future), strPtr(future), false},
		{"kept-unreadable-from", strPtr("2026-12-31T00:00:00Z"), nil, true},
		{"kept-unreadable-until", strPtr(past), strPtr("0000-00-00"), true},
	}

	want := map[string]bool{}
	for _, c := range cases {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: c.name, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", c.name, err)
		}
		setRawValidity(t, s, ctx, id, c.validFrom, c.validUntil, nil)
		if c.wantKept {
			want[id] = true
		}
	}

	pol := projectPassivePolicy()
	pol.Bucket = testProject
	pol.OverFetch = 100
	req := passiveRequest(testProject, pol)
	// The request clock is what the SQL predicate and the Go rule are both judged
	// against, so bind the same instant the cases were built around.
	req.Now = now
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	got := map[string]bool{}
	for _, r := range set.Rows {
		got[r.ID] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("a row Go's rule keeps was not returned by the passive read: %s", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("a row Go's rule drops was returned by the passive read: %s", id)
		}
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

		n, err := s.passiveValidityExcluded(ctx, req, pol, cols)
		if err != nil {
			t.Fatalf("passiveValidityExcluded(%s): %v", pol.Bucket, err)
		}
		if n != 2 {
			t.Errorf("passiveValidityExcluded(%s) = %d; want 2 (the expired and the not-yet-open row)", pol.Bucket, n)
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
