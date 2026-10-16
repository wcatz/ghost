package memory

import (
	"context"
	"testing"
	"time"
)

// The tests below cover Candidate.SupersededBy on a QUERY read. A live
// `supersedes` edge switches the lexical replacement guess (MaybeReplacedBy) off
// for its pair on a query read as on a passive one, so the retriever must name
// the superseder on a query read too, or a search answer says less about a
// replaced row once the lifecycle has linked it than it said before. The name
// is for the marker only: the order of the set is the one it has with no name.

// querySupersedeStore holds a replaced convention and its replacement, both
// matching "release tags", saved a day apart so the lexical guess has the
// chronology it needs.
func querySupersedeStore(t *testing.T) (s *Store, older, newer string) {
	t.Helper()
	s = testStore(t)
	ctx := context.Background()
	create := func(content string) string {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "convention", Content: content, Source: "manual", Importance: 0.7,
			Tags: []string{"release"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}
	older = create("Release tags follow semver: tag `vMAJOR.MINOR.PATCH` from main and run `make release` to publish.")
	newer = create("Release tags follow calendar versioning now: tag `YYYY.MM.N` from main and run `make release` to publish.")
	day := time.Now().UTC().Add(-48 * time.Hour)
	for id, at := range map[string]time.Time{older: day, newer: day.Add(24 * time.Hour)} {
		stamp := at.Format(StoredStampLayout)
		if _, err := s.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
			t.Fatalf("restamp %s: %v", id, err)
		}
	}
	return s, older, newer
}

func queryCandidateByID(set *CandidateSet, id string) (Candidate, bool) {
	for _, c := range set.Rows {
		if c.ID == id {
			return c, true
		}
	}
	return Candidate{}, false
}

// TestAQuerySupersededRowIsNamedByItsSuperseder is the defect: before the
// lifecycle the query read names the replacement as a lexical guess; once a live
// `supersedes` edge joins the pair the guess is off, and the read must name the
// superseder instead, so the row is never left with nothing.
func TestAQuerySupersededRowIsNamedByItsSuperseder(t *testing.T) {
	s, older, newer := querySupersedeStore(t)
	ctx := context.Background()
	req := candidateRequest("release tags", 10, time.Now().UTC())

	before, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if c, ok := queryCandidateByID(before, older); !ok || len(c.MaybeReplacedBy) != 1 || c.MaybeReplacedBy[0] != newer {
		t.Fatalf("fixture: with no edge the lexical guess must name the replacement: %+v (found %v)", c.MaybeReplacedBy, ok)
	}

	if err := s.CreateLink(ctx, newer, older, "supersedes", 1, "llm"); err != nil {
		t.Fatalf("link: %v", err)
	}
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	c, ok := queryCandidateByID(set, older)
	if !ok {
		t.Fatalf("the superseded row is not in the set: %v", candidateIDs(t, set))
	}
	if len(c.MaybeReplacedBy) != 0 {
		t.Errorf("MaybeReplacedBy = %v: a live edge switches the guess off", c.MaybeReplacedBy)
	}
	if len(c.SupersededBy) != 1 || c.SupersededBy[0] != newer {
		t.Errorf("SupersededBy = %v, want [%s]: a query read names the superseder the edge records", c.SupersededBy, newer)
	}
	if n, _ := queryCandidateByID(set, newer); len(n.SupersededBy) != 0 {
		t.Errorf("the superseder is named as superseded: %v", n.SupersededBy)
	}
}

// TestAQueryReadWithNoLiveSupersedeNamesNothing: the naming reads the demotion's
// own edge rules (supersedePenaltyRows), so no edge, a withdrawn edge
// (supersede withdraws by invalidating) and an edge between two conflicting
// scopes all leave the field empty.
func TestAQueryReadWithNoLiveSupersedeNamesNothing(t *testing.T) {
	cases := map[string]func(t *testing.T, s *Store, older, newer string){
		"no edge": func(*testing.T, *Store, string, string) {},
		"withdrawn": func(t *testing.T, s *Store, older, newer string) {
			if err := s.CreateLink(context.Background(), newer, older, "supersedes", 1, "llm"); err != nil {
				t.Fatalf("link: %v", err)
			}
			if n, err := s.InvalidateLink(context.Background(), newer, older, "supersedes"); err != nil || n != 1 {
				t.Fatalf("InvalidateLink = %d, %v", n, err)
			}
		},
		"scope conflict": func(t *testing.T, s *Store, older, newer string) {
			if err := s.CreateLink(context.Background(), newer, older, "supersedes", 1, "llm"); err != nil {
				t.Fatalf("link: %v", err)
			}
			for id, env := range map[string]string{newer: "development", older: "production"} {
				if _, err := s.db.Exec(`UPDATE memories SET scope = ? WHERE id = ?`, `{"environment":"`+env+`"}`, id); err != nil {
					t.Fatalf("scope %s: %v", id, err)
				}
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s, older, newer := querySupersedeStore(t)
			setup(t, s, older, newer)
			set, err := s.Candidates(context.Background(), candidateRequest("release tags", 10, time.Now().UTC()))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if len(set.Rows) < 2 {
				t.Fatalf("fixture: both rows must be returned: %v", candidateIDs(t, set))
			}
			for _, c := range set.Rows {
				if len(c.SupersededBy) != 0 {
					t.Errorf("%s SupersededBy = %v, want none", c.ID, c.SupersededBy)
				}
			}
		})
	}
}

// TestAQueryKeepForeverSupersededRowIsNamedButNotMoved: a `retention=persistent`
// row is never sunk by a supersedes edge, but the edge still switches the guess
// off, so it is named for its marker; the name moves nothing, so the set's
// order is the order it has when the demotion ignores the edge.
func TestAQueryKeepForeverSupersededRowIsNamedButNotMoved(t *testing.T) {
	s, older, newer := querySupersedeStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE memories SET retention = 'persistent' WHERE id = ?`, older); err != nil {
		t.Fatalf("retention: %v", err)
	}
	now := time.Now().UTC()
	req := candidateRequest("release tags", 10, now)
	before, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if err := s.CreateLink(ctx, newer, older, "supersedes", 1, "llm"); err != nil {
		t.Fatalf("link: %v", err)
	}
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	c, ok := queryCandidateByID(set, older)
	if !ok {
		t.Fatalf("the keep-forever row is not in the set: %v", candidateIDs(t, set))
	}
	if len(c.SupersededBy) != 1 || c.SupersededBy[0] != newer {
		t.Errorf("SupersededBy = %v, want [%s]: a keep-forever row is named for its marker", c.SupersededBy, newer)
	}
	if got, want := candidateIDs(t, set), candidateIDs(t, before); !sameIDs(got, want) {
		t.Errorf("order moved: %v -> %v (a keep-forever row is never sunk, and its name moves nothing)", want, got)
	}
}
