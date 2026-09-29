package memory

import (
	"context"
	"strings"
	"testing"
)

// TestCandidatesPassiveReadsAMixedBucketAsProjectScoped is the store half of the
// project-context migration's substrate: one policy, one cap, and `_global` rows
// admitted into the project's read — which is what
// `WHERE project_id = ? OR project_id = '_global'` has always meant on this
// surface, and what a per-bucket seam cannot otherwise express.
//
// The bucket name is `proj`, NOT a union, so the assertion that the globals
// really arrived is the point: a policy that quietly ignored the flag would return
// a correct-looking project-only set.
func TestCandidatesPassiveReadsAMixedBucketAsProjectScoped(t *testing.T) {
	st := passiveFixture(t)
	pol := projectPassivePolicy()
	pol.TwoPass = false
	pol.IncludeGlobal = true
	pol.ItemCap = 20
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := passiveIDs(set)
	var sawProject, sawGlobal bool
	for _, id := range ids {
		if strings.HasPrefix(id, "gp_") {
			sawGlobal = true
		} else {
			sawProject = true
		}
	}
	if !sawProject || !sawGlobal {
		t.Errorf("a mixed bucket must read both projects; got %v (project=%v global=%v)", ids, sawProject, sawGlobal)
	}
}

// TestCandidatesPassiveAMixedBucketFiltersResolvedAndScopeForBothProjects is the
// precedence trap, and it is a KILL rather than a claim.
//
// `AND` binds tighter than `OR` in SQL, so an unbracketed
// `project_id = ? OR project_id = '_global' AND resolved_at IS NULL` reads as
// `project_id = ? OR (… AND resolved_at IS NULL)`: every row of the REQUESTING
// project escapes the resolved filter, and the scope clause appended after it
// applies to the `_global` half alone. The block then shows resolved rows, and a
// scope the caller set does nothing to the rows that matter.
//
// Both halves are asserted, and the scope half needs a scoped project row and a
// scoped global of the same key plus an unscoped row that must survive — otherwise
// "the scope filter worked" is also what an absent filter looks like.
func TestCandidatesPassiveAMixedBucketFiltersResolvedAndScopeForBothProjects(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	for _, r := range []struct {
		id, project string
		scope       map[string]string
	}{
		{"mx_scoped_proj", "proj", map[string]string{"area": "payments"}},
		{"mx_scoped_glob", "_global", map[string]string{"area": "payments"}},
		{"mx_other_proj", "proj", map[string]string{"area": "search"}},
		{"mx_resolved_proj", "proj", nil},
	} {
		// CreateWithID rather than Upsert: Upsert MINTS the id, and a fixture
		// that then updates a row by a name it chose is a fixture asserting on
		// nothing. The scope rides the Memory rather than a second statement, so
		// the row is written once.
		if _, err := st.CreateWithID(ctx, r.project, r.id, Memory{
			Category: "fact", Content: "a mixed-bucket row " + r.id, Source: "manual",
			Importance: 0.5, Scope: r.scope,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	if _, err := st.db.Exec(`UPDATE memories SET resolved_at = '2026-01-01 00:00:00' WHERE id = 'mx_resolved_proj'`); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	pol := projectPassivePolicy()
	pol.TwoPass = false
	pol.IncludeGlobal = true
	pol.ItemCap = 50
	req := passiveRequest("proj", pol)
	req.Scope = map[string]string{"area": "payments"}
	set, err := st.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := passiveIDs(set)
	for _, absent := range []string{"mx_resolved_proj", "mx_other_proj"} {
		for _, id := range ids {
			if id == absent {
				t.Errorf("%s reached a mixed bucket; the resolved filter and the scope predicate must bind over the "+
					"whole union, not over the _global half of it (ids: %v)", absent, ids)
			}
		}
	}
	for _, present := range []string{"mx_scoped_proj", "mx_scoped_glob"} {
		found := false
		for _, id := range ids {
			if id == present {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing from a scoped mixed bucket; the scope predicate must not exclude the project's own "+
				"rows (ids: %v)", present, ids)
		}
	}
}

// TestCandidatesPassiveRejectsAPolicyThatBothMixesAndFetchesGlobal: the store's
// own half of the overlap refusal, for the same reason the assembler has one —
// `Candidates` is reachable directly (the bench harness, the tests), so a guard
// only the assembler's copy of the contract applies is a guard half the callers
// miss.
//
// Two DISTINCT bucket names describing one overlapping row set is the shape the
// repeated-bucket refusal cannot see: the rows are fetched once per policy and
// concatenated, so every `_global` row comes back twice.
func TestCandidatesPassiveRejectsAPolicyThatBothMixesAndFetchesGlobal(t *testing.T) {
	st := passiveFixture(t)
	mixing := projectPassivePolicy()
	mixing.IncludeGlobal = true
	for _, order := range [][]SlicePolicy{
		{mixing, globalPassivePolicy()},
		{globalPassivePolicy(), mixing},
	} {
		if _, err := st.Candidates(context.Background(), passiveRequest("proj", order...)); err == nil {
			t.Errorf("a request that both mixes %q into a project bucket and fetches it in its own right must be "+
				"refused, not served with every global row twice", GlobalProjectID)
		}
	}
}

// TestCandidatesPassiveRecordsWhichPolicyFetchedEachRow is the store half of the
// cap fix: a row admitted under a bucket that mixes `_global` carries the POLICY
// that read it, which is not its own project.
//
// Without it the assembler cannot apply a per-slice cap to such a row at all —
// it would look for a slice named `_global`, find none, and leave the row
// unbounded — so "at most N rows" would become "at most N project rows, plus
// however many globals were nearby". Asserted here because the store is where the
// fact is produced.
func TestCandidatesPassiveRecordsWhichPolicyFetchedEachRow(t *testing.T) {
	st := passiveFixture(t)
	pol := projectPassivePolicy()
	pol.TwoPass = false
	pol.IncludeGlobal = true
	pol.ItemCap = 50
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	sawGlobal := false
	for _, r := range set.Rows {
		if r.FetchedBy != "proj" {
			t.Errorf("row %s carries FetchedBy %q; every row of this request came from the one mixing policy", r.ID, r.FetchedBy)
		}
		if r.ProjectID == GlobalProjectID {
			sawGlobal = true
			if r.ProjectID == r.FetchedBy {
				t.Errorf("row %s is a global read under a bucket of the same name, which is the case that hides the "+
					"distinction; this test needs a global row whose policy is the PROJECT's", r.ID)
			}
		}
	}
	if !sawGlobal {
		t.Fatal("the fixture produced no global row, so FetchedBy is untested for the case that needs it")
	}
}
