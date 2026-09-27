package resolve

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A 'supersedes'/'llm' edge whose endpoints name different environments is not a
// verdict that anything was replaced: the development row did not supersede the
// production row, because the two are not two wordings of one claim. The
// classifier that wrote the edge could not have known — it is handed the two
// note bodies and nothing else, and scope is a column beside the text — so an
// edge like this can be in any store that was consolidated before the writer
// learned to refuse the pair.
//
// Every reader has to cope with it. Read as sound, it is worse than the ranking
// penalty it causes: both reads below resolve a row, which is persisted,
// removes it from ranked injection everywhere, and is only undone by an
// explicit repair pass.

// TestRunIgnoresScopeConflictingSupersedesPiggyback: the piggyback stamps
// resolved_at on the older endpoint with no classifier call, so an edge that
// names two different places silently retires a live production fact on the
// next lifecycle run.
func TestRunIgnoresScopeConflictingSupersedesPiggyback(t *testing.T) {
	older := memory.Memory{ID: "older", Category: "gotcha",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "production"}}
	newer := memory.Memory{ID: "newer", Category: "gotcha",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "development"}}
	store := &fakeStore{
		candidates: []memory.Memory{older, newer},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "older", Relation: "supersedes", Source: "llm",
		}},
	}
	// The classifier would KEEP both rows; the piggyback is what must not fire.
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Superseded != 0 {
		t.Errorf("res.Superseded = %d, want 0: the edge joins a development row to a production one, so it asserts no replacement", res.Superseded)
	}
	if len(confirmed) != 0 {
		t.Errorf("confirmed = %v, want nothing: a scope-conflicting edge must not stamp resolved_at on the production row", confirmed)
	}
	if len(store.resolved) != 0 {
		t.Errorf("resolved_at written for %v, want nothing", store.resolved)
	}
}

// TestRunPiggybackKeepsSameScopeSupersedesEdge is the direction that would be
// wrong to over-correct: a same-scope edge is a real verdict and still retires
// the older endpoint for free.
func TestRunPiggybackKeepsSameScopeSupersedesEdge(t *testing.T) {
	older := memory.Memory{ID: "older", Category: "gotcha",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "production"}}
	newer := memory.Memory{ID: "newer", Category: "gotcha",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "production"}}
	store := &fakeStore{
		candidates: []memory.Memory{older, newer},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "older", Relation: "supersedes", Source: "llm",
		}},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Superseded != 1 {
		t.Errorf("res.Superseded = %d, want 1 (a same-scope edge is a real verdict)", res.Superseded)
	}
	if len(confirmed) != 1 || confirmed[0].ID != "older" {
		t.Errorf("confirmed = %v, want [older]", confirmed)
	}
}

// TestReassessIgnoresScopeConflictingSupersedesAssertion: the repair pass holds
// a row back from repair when the ordinary pass would re-stamp it for free. A
// scope-conflicting edge asserts nothing, so holding the row back would report
// "still demoted" for a retirement that should not exist, and the row could
// never be repaired by any pass.
func TestReassessIgnoresScopeConflictingSupersedesAssertion(t *testing.T) {
	superseded := memory.Memory{ID: "superseded", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "production"}}
	newer := memory.Memory{ID: "newer", Category: "gotcha", UpdatedAt: "2026-09-02 00:00:00",
		Content: "the api listen port is 8443", Scope: map[string]string{"environment": "development"}}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{superseded},
		candidates:      []memory.Memory{newer},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "superseded", Relation: "supersedes", Source: "llm",
		}},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, _, err := Reassess(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 0 {
		t.Errorf("res.Demoted = %d, want 0: the edge asserts no replacement across two environments, so the row is a repair candidate like any other", res.Demoted)
	}
}
