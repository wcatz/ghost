package supersede

// Every failure in the supersede package that names a stored memory id renders
// it through `memory.RefusalArg`. A stored id is not token-guarded — `ghost
// import` writes an artifact's id verbatim — so a pair whose endpoints are
// imported can carry a `ghp_`-shaped id, and a failure that interpolated it raw
// would put the whole thing on stderr and, on the detached lifecycle path,
// `lifecycle.log`.
//
// This file drives each converted site through the seam that reaches it, with a
// token-shaped id in the pair, and asserts the raw id is gone while the
// placeholder that names its format is present. It is a table rather than one
// case per site so a site that stops being reached is visible as a failure of
// exactly one row, and so a reviewer can add a row without restructuring a
// fixture.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// errHarnessDown is the failure every fake store returns, so a test asserts on
// the caller's sentence rather than on a transport detail.
var errHarnessDown = errors.New("harness down")

// renderStore is a canned store implementing every interface the three entry
// points take, so one fake can drive the scan, the pass, the repair and the
// named withdrawal. Each failure switch is a counter or a bool so a site behind
// an earlier read is reachable: failGetByIDsAt and failInvalidateAt count calls,
// which is how the second existence check and the second sweep are hit.
type renderStore struct {
	mems  []memory.Memory
	links []memory.Link

	failSearchAt         int
	failGetByIDsAt       int
	failGetLinks         bool
	failInvalidateAt     int
	failCreateSupersedes bool
	failCreateCauses     bool

	searchCalls, getByIDsCalls, invalidateCalls int
}

func (s *renderStore) GetAll(_ context.Context, _ string, _ int) ([]memory.Memory, error) {
	return s.mems, nil
}

func (s *renderStore) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	s.getByIDsCalls++
	if s.failGetByIDsAt != 0 && s.getByIDsCalls == s.failGetByIDsAt {
		return nil, errHarnessDown
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]memory.Memory, 0, len(ids))
	for _, m := range s.mems {
		if want[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *renderStore) GetEmbedding(_ context.Context, _ string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func (s *renderStore) SearchVectorScoped(_ context.Context, _ string, _ []float32, _ int, _ map[string]string) ([]memory.ScoredMemory, error) {
	s.searchCalls++
	if s.failSearchAt != 0 && s.searchCalls == s.failSearchAt {
		return nil, errHarnessDown
	}
	out := make([]memory.ScoredMemory, 0, len(s.mems))
	for _, m := range s.mems {
		out = append(out, memory.ScoredMemory{MemoryID: m.ID, Score: 0.95, ProjectID: m.ProjectID})
	}
	return out, nil
}

func (s *renderStore) CreateLinkJudged(_ context.Context, _, _, relation string, _ float32, _, _ string) error {
	if relation == string(RelationCauses) && s.failCreateCauses {
		return errHarnessDown
	}
	return nil
}

func (s *renderStore) CreateLinkUnopposed(_ context.Context, _, _, relation string, _ float32, _, _ string) (bool, error) {
	switch relation {
	case string(RelationSupersedes):
		if s.failCreateSupersedes {
			return false, errHarnessDown
		}
	case string(RelationCauses):
		if s.failCreateCauses {
			return false, errHarnessDown
		}
	}
	return true, nil
}

func (s *renderStore) InvalidateLink(_ context.Context, _, _, _ string) (int64, error) {
	s.invalidateCalls++
	if s.failInvalidateAt != 0 && s.invalidateCalls == s.failInvalidateAt {
		return 0, errHarnessDown
	}
	return 1, nil
}

func (s *renderStore) LinksByRelationSource(_ context.Context, _, relation, source string) ([]memory.Link, error) {
	var out []memory.Link
	for _, l := range s.links {
		if l.Relation == relation && l.Source == source {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *renderStore) SupersedeChecked(_ context.Context, _ string) (map[[2]string]memory.SupersedeCheck, error) {
	return map[[2]string]memory.SupersedeCheck{}, nil
}

func (s *renderStore) MarkSupersedeNeither(_ context.Context, _ string, _ map[[2]string]memory.SupersedeCheck) error {
	return nil
}

func (s *renderStore) GetLinks(_ context.Context, memoryID string) ([]memory.Link, error) {
	if s.failGetLinks {
		return nil, errHarnessDown
	}
	var out []memory.Link
	for _, l := range s.links {
		if l.SourceID == memoryID || l.TargetID == memoryID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *renderStore) MemoryIDsByIDPrefix(_ context.Context, _, prefix string) ([]string, error) {
	return s.idsWithPrefix(prefix), nil
}

func (s *renderStore) MemoryIDsByIDPrefixAnyProject(_ context.Context, prefix string) ([]string, error) {
	return s.idsWithPrefix(prefix), nil
}

func (s *renderStore) idsWithPrefix(prefix string) []string {
	var out []string
	for _, m := range s.mems {
		if strings.HasPrefix(m.ID, prefix) {
			out = append(out, m.ID)
		}
	}
	return out
}

func (s *renderStore) LinksInto(_ context.Context, _, memoryID, relation string) ([]memory.Link, error) {
	var out []memory.Link
	for _, l := range s.links {
		if l.TargetID != memoryID {
			continue
		}
		if relation != "" && l.Relation != relation {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// renderStoreWithPair is the ordinary fixture: one token-shaped newer note and
// one ordinary older one, so an id rendered raw is the token and a withheld one
// is detectable.
func renderStoreWithPair() *renderStore {
	return &renderStore{mems: []memory.Memory{
		renderMemory(tokenShapedStoredID, "the retry budget is 3 attempts", "2026-09-01 00:00:00"),
		renderMemory(ordinaryStoredID, "the retry budget is 5 attempts", "2026-01-01 00:00:00"),
	}}
}

// renderStoreWithLiveSupersedes adds the live edge the repair pass judges. The
// edge's stamp predates both endpoints, so the ordinary pass re-judges the pair
// rather than holding it quiet by skip-if-unchanged.
func renderStoreWithLiveSupersedes() *renderStore {
	s := renderStoreWithPair()
	s.links = []memory.Link{{
		SourceID: tokenShapedStoredID, TargetID: ordinaryStoredID,
		Relation: string(RelationSupersedes), Strength: 0.95, Source: "llm",
		CreatedAt: "2025-01-01 00:00:00",
	}}
	return s
}

// renderStoreWithTokenOlder is the same live edge with the token-shaped id on the
// OLDER end, which is the id the repair pass's causes read names.
func renderStoreWithTokenOlder() *renderStore {
	s := renderStoreWithPair()
	s.links = []memory.Link{{
		SourceID: ordinaryStoredID, TargetID: tokenShapedStoredID,
		Relation: string(RelationSupersedes), Strength: 0.95, Source: "llm",
		CreatedAt: "2025-01-01 00:00:00",
	}}
	return s
}

// renderStoreWithCausesCycle adds both directions of a 'causes' edge, which is
// what the pass reads a pair to be when it sweeps the reverse direction. The
// stamps predate both endpoints so the ordinary pass judges the pair.
func renderStoreWithCausesCycle() *renderStore {
	s := renderStoreWithPair()
	s.links = []memory.Link{
		{SourceID: ordinaryStoredID, TargetID: tokenShapedStoredID, Relation: string(RelationCauses), Source: "llm", CreatedAt: "2025-01-01 00:00:00"},
		{SourceID: tokenShapedStoredID, TargetID: ordinaryStoredID, Relation: string(RelationCauses), Source: "llm", CreatedAt: "2025-01-01 00:00:00"},
	}
	return s
}

func renderMemory(id, content, stamp string) memory.Memory {
	return memory.Memory{
		ID: id, ProjectID: "p", Category: "fact", Content: content,
		Importance: 0.7, Source: "mcp", CreatedAt: stamp, UpdatedAt: stamp,
	}
}

// renderFixedClassifier answers every pair with one verdict, so a case can pick
// the branch it needs without scripting per-content replies.
type renderFixedClassifier struct{ verdict Relation }

func (c renderFixedClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i := range out {
		out[i] = c.verdict
	}
	return out, nil
}

// assertWithheld is the one assertion every row shares: the failure happened, it
// did not carry the token, and it named the format instead.
func assertWithheld(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a failure, got nil")
	}
	if strings.Contains(err.Error(), tokenShapedStoredID) {
		t.Errorf("the failure echoed a token-shaped stored id: %s", err)
	}
	if !strings.Contains(err.Error(), "withheld") {
		t.Errorf("the failure did not withhold the token-shaped id by its format: %s", err)
	}
}

func TestStoredIDRenderingAcrossTheSupersedePackage(t *testing.T) {
	ctx := context.Background()
	newer, older := tokenShapedStoredID, ordinaryStoredID

	cases := []struct {
		name string
		site string
		run  func(t *testing.T) error
	}{
		{
			name: "select candidates search vector", site: "supersede.go SelectCandidates",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failSearchAt = 1
				_, err := SelectCandidates(ctx, s, "p", 0.9)
				return err
			},
		},
		{
			name: "run stale check", site: "supersede.go endpointsExist call",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failGetByIDsAt = 2 // the first is the pre-classify existence read
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationNeither}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run create supersedes link", site: "supersede.go create supersedes",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failCreateSupersedes = true
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationSupersedes}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate causes link", site: "supersede.go supersedes branch causes sweep",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 1
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationSupersedes}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate reverse causes link", site: "supersede.go supersedes branch reverse sweep",
			run: func(t *testing.T) error {
				s := renderStoreWithCausesCycle()
				s.failInvalidateAt = 2
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationSupersedes}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate supersedes link on a causes verdict", site: "supersede.go causes branch",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 1
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationCauses}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate reverse causes link on a causes verdict", site: "supersede.go causes branch reverse sweep",
			run: func(t *testing.T) error {
				s := renderStoreWithCausesCycle()
				s.failInvalidateAt = 2
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationCauses}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run create causes link", site: "supersede.go create causes",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failCreateCauses = true
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationCauses}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate supersedes link on a neither verdict", site: "supersede.go neither branch",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 1
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationNeither}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "run invalidate reversed supersedes link", site: "supersede.go reversed branch",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 1
				_, _, err := RunWith(ctx, s, renderFixedClassifier{RelationReversed}, "p", Options{Threshold: 0.9, Apply: true}, nil)
				return err
			},
		},
		{
			name: "sweep causes both ways first direction", site: "supersede.go sweepCausesBothWays",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 1
				_, err := sweepCausesBothWays(ctx, s, nil, newer, older)
				return err
			},
		},
		{
			name: "sweep causes both ways reverse direction", site: "supersede.go sweepCausesBothWays reverse",
			run: func(t *testing.T) error {
				s := renderStoreWithPair()
				s.failInvalidateAt = 2
				claims := map[pairKey][]liveEdge{
					newPairKey(newer, older): {{relation: RelationCauses, newer: older, older: newer}},
				}
				_, err := sweepCausesBothWays(ctx, s, claims, newer, older)
				return err
			},
		},
		{
			name: "reassess load links of", site: "reassess.go liveCausesPairs",
			run: func(t *testing.T) error {
				s := renderStoreWithTokenOlder()
				s.failGetLinks = true
				_, _, err := ReassessWith(ctx, s, renderFixedClassifier{RelationNeither}, "p", ReassessOptions{}, nil)
				return err
			},
		},
		{
			name: "reassess withdraw supersedes link", site: "reassess.go withdraw supersedes",
			run: func(t *testing.T) error {
				s := renderStoreWithLiveSupersedes()
				s.failInvalidateAt = 1
				_, _, err := ReassessWith(ctx, s, renderFixedClassifier{RelationNeither}, "p", ReassessOptions{Apply: true}, nil)
				return err
			},
		},
		{
			name: "reassess withdraw causes link", site: "reassess.go withdraw causes",
			run: func(t *testing.T) error {
				s := renderStoreWithLiveSupersedes()
				s.failInvalidateAt = 2
				_, _, err := ReassessWith(ctx, s, renderFixedClassifier{RelationNeither}, "p", ReassessOptions{Apply: true}, nil)
				return err
			},
		},
		{
			name: "withdraw a named link", site: "withdraw.go part-way write failure",
			run: func(t *testing.T) error {
				s := renderStoreWithLiveSupersedes()
				s.failInvalidateAt = 1
				_, err := Withdraw(ctx, s, "p", []WithdrawPair{{Source: newer, Target: older}}, true, nil)
				return err
			},
		},
		{
			name: "withdraw a self link", site: "withdraw.go self-link refusal",
			run: func(t *testing.T) error {
				s := renderStoreWithLiveSupersedes()
				_, err := Withdraw(ctx, s, "p", []WithdrawPair{{Source: newer, Target: newer}}, true, nil)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatalf("%s did not fail, so this row asserts nothing about it", tc.site)
			}
			assertWithheld(t, err)
		})
	}
}

// TestRelationClassifierWithholdsTokenShapedStoredIDsInChunkErrors drives the two
// classifier sites the single-pair case above does not reach: the batched chunk
// prefix in ClassifyBatch and the per-pair fallback inside classifyChunk.
func TestRelationClassifierWithholdsTokenShapedStoredIDsInChunkErrors(t *testing.T) {
	t.Run("batched chunk prefix", func(t *testing.T) {
		cls := NewRelationClassifier(&fakeProvider{err: errHarnessDown})
		cls.SetRetryDelay(0)
		_, err := cls.ClassifyBatch(context.Background(), []Candidate{
			{NewerID: tokenShapedStoredID, OlderID: ordinaryStoredID, NewerContent: "n1", OlderContent: "o1"},
			{NewerID: "cccccccccccccccccccccccccccccccc", OlderID: "dddddddddddddddddddddddddddddddd", NewerContent: "n2", OlderContent: "o2"},
		})
		if err == nil {
			t.Fatal("want the chunk failure, got nil")
		}
		if !strings.Contains(err.Error(), "pairs 1-2") {
			t.Errorf("the failure did not name the failing chunk: %s", err)
		}
		assertWithheld(t, err)
	})

	t.Run("per-pair fallback", func(t *testing.T) {
		// The batch reply is unparseable, so classifyChunk falls back to the
		// single-pair path, whose call then fails.
		fp := &flakyClassifyProvider{calls: 0}
		cls := NewRelationClassifier(fp)
		cls.SetRetryDelay(0)
		_, err := cls.ClassifyBatch(context.Background(), []Candidate{
			{NewerID: tokenShapedStoredID, OlderID: ordinaryStoredID, NewerContent: "n1", OlderContent: "o1"},
			{NewerID: "cccccccccccccccccccccccccccccccc", OlderID: "dddddddddddddddddddddddddddddddd", NewerContent: "n2", OlderContent: "o2"},
		})
		if err == nil {
			t.Fatal("want the fallback failure, got nil")
		}
		assertWithheld(t, err)
	})
}

// flakyClassifyProvider answers the first (batch) call with an unparseable
// sentence and fails every call after it, so classifyChunk's per-pair fallback is
// the path that produces the error.
type flakyClassifyProvider struct{ calls int }

func (p *flakyClassifyProvider) Classify(_ context.Context, _, _ string) (string, error) {
	p.calls++
	if p.calls == 1 {
		return "banana", nil
	}
	return "", errHarnessDown
}
