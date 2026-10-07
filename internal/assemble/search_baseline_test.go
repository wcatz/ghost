package assemble

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The two tests below pin the formatted answer of a scope-filtered search
// byte for byte. They were captured against the assembler BEFORE explain became
// a projection of its trace, and they must keep passing unedited: explain is not
// allowed to move a byte of the plain answer, its verdict line, or the advice a
// scope-emptied answer carries.

func baselineScoped() Request {
	req := hybridRequest()
	req.Scope = map[string]string{"environment": "production"}
	return req
}

func TestBaselineScopeFilteredAnswerBytes(t *testing.T) {
	dev := scopedCandidate("D1", map[string]string{"environment": "development"}, 0.95)
	prodA := scopedCandidate("P1", map[string]string{"environment": "production"}, 0.9)
	prodB := scopedCandidate("P2", map[string]string{"environment": "production"}, 0.8)
	prodC := scopedCandidate("P3", map[string]string{"environment": "production"}, 0.7)
	for _, c := range []*memory.Candidate{&prodA, &prodB, &prodC, &dev} {
		c.FTSRank = 0
	}
	res := run(t, &fakeRetriever{set: hybridSet(dev, prodA, prodB, prodC)}, baselineScoped())
	if got := itemIDs(res.Items); !eq(got, []string{"P1", "P2"}) {
		t.Fatalf("admitted %v, want P1,P2", got)
	}
	const want = "- [fact] `P1` (0.7 scope{environment=production}) «database configuration P1»\n- [fact] `P2` (0.7 scope{environment=production}) «database configuration P2»\n\n[ghost:outcome=answerable reason=floor_met floor_fts_rank=3 abstain_cosine=off candidates=4 admitted=2 legs=fts:ok,vector:ok tokens_est=14]\n"
	if res.Response != want {
		t.Errorf("response changed:\n got: %q\nwant: %q", res.Response, want)
	}
}

func TestBaselineScopeEmptiedAnswerBytes(t *testing.T) {
	dev := scopedCandidate("D1", map[string]string{"environment": "development"}, 0.95)
	res := run(t, &fakeRetriever{set: hybridSet(dev)}, baselineScoped())
	if len(res.Items) != 0 {
		t.Fatalf("admitted %v, want none", itemIDs(res.Items))
	}
	const want = "No sufficiently trustworthy memory found: nothing found matched the requested scope. The note below breaks the removals down per stage.\n\n(Note: the scope filter was applied to a finite search window, so further matches may exist beyond the retrieved candidates — raise the limit.)\n(Note: 1 candidate rows were removed and none reached the answer: predicates 1)\n[ghost:outcome=empty reason=all_out_of_scope floor_fts_rank=not_applied abstain_cosine=off candidates=1 admitted=0 legs=fts:ok,vector:ok tokens_est=0]\n"
	if res.Response != want {
		t.Errorf("response changed:\n got: %q\nwant: %q", res.Response, want)
	}
}

// TestBaselineScopeEmptiedPoolCarriesTheDropScopeAdvice is the store-level
// shape of a scope-emptied answer: scope narrowing removed every candidate
// before the window, so nothing came back at all and the absence note names the
// filters the request set.
func TestBaselineScopeEmptiedPoolCarriesTheDropScopeAdvice(t *testing.T) {
	res := run(t, &fakeRetriever{set: hybridSet()}, baselineScoped())
	const want = "Ghost memory: no match within the searched window — widen the limit or drop the scope filter. This is not evidence that nothing exists.\n\n(Note: the scope filter was applied to a finite search window, so further matches may exist beyond the retrieved candidates — raise the limit.)\n[ghost:outcome=empty reason=no_candidates floor_fts_rank=not_applied abstain_cosine=off candidates=0 admitted=0 legs=fts:ok,vector:ok tokens_est=0]\n"
	if res.Response != want {
		t.Errorf("response changed:\n got: %q\nwant: %q", res.Response, want)
	}
}
