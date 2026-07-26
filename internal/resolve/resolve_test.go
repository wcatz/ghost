package resolve

import (
	"context"
	"errors"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// fakeClassifier resolves any memory whose content is in the drop set.
type fakeClassifier struct {
	drop map[string]bool
	// errOn, when set, is the content for which IsResolved returns err instead
	// of a normal answer. If err is set and errOn is empty, every call errors.
	errOn string
	err   error
}

func (f fakeClassifier) IsResolved(_ context.Context, content string) (bool, error) {
	if f.err != nil && (f.errOn == "" || f.errOn == content) {
		return false, f.err
	}
	return f.drop[content], nil
}

// fakeStore satisfies resolveStore with in-memory candidates.
type fakeStore struct {
	candidates []memory.Memory
	resolved   []string
	err        error // when set, returned by SetResolved instead of writing
}

func (s *fakeStore) ResolveCandidates(_ context.Context, _ string) ([]memory.Memory, error) {
	return s.candidates, nil
}
func (s *fakeStore) SetResolved(_ context.Context, ids []string) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.resolved = append(s.resolved, ids...)
	return len(ids), nil
}

func TestPrefilterKeepsOnlyPlausible(t *testing.T) {
	in := []memory.Memory{
		{ID: "1", Content: "Graph-expansion RESOLVED NO-GO after kill experiment"},
		{ID: "2", Content: "Ghost uses SQLite with FTS5 for storage"},
		{ID: "3", Content: "fixed in PR #210, dead ranking bonus removed"},
	}
	got := Prefilter(in)
	gotIDs := map[string]bool{}
	for _, m := range got {
		gotIDs[m.ID] = true
	}
	if !gotIDs["1"] || !gotIDs["3"] {
		t.Errorf("prefilter dropped a resolution-keyword memory: got %v", gotIDs)
	}
	if gotIDs["2"] {
		t.Errorf("prefilter kept a memory with no resolution keyword: got %v", gotIDs)
	}
}

func TestRunResolvesConfirmedEvidence(t *testing.T) {
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "keep", Content: "Graph-expansion RESOLVED NO-GO decision record"},
		{ID: "drop", Content: "kill experiment finding: 7.3% cross-session links, removed"},
		{ID: "noise", Content: "unrelated architecture note about workers"},
	}}
	cls := fakeClassifier{drop: map[string]bool{
		"kill experiment finding: 7.3% cross-session links, removed": true,
	}}

	// Dry run: nothing written.
	res, confirmed, err := Run(context.Background(), store, cls, "proj", false, nil)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if len(store.resolved) != 0 {
		t.Errorf("dry run wrote %v, want nothing", store.resolved)
	}
	if res.Confirmed != 1 || len(confirmed) != 1 || confirmed[0].ID != "drop" {
		t.Fatalf("dry run: confirmed=%d ids=%v, want 1 [drop]", res.Confirmed, confirmed)
	}
	// "noise" has no keyword → prefiltered out → never classified.
	if res.Candidates != 2 {
		t.Errorf("candidates after prefilter = %d, want 2 (keep, drop)", res.Candidates)
	}
	if res.Loaded != 3 {
		t.Errorf("res.Loaded = %d, want 3", res.Loaded)
	}

	// Apply: the confirmed evidence is written.
	res, _, err = Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if len(store.resolved) != 1 || store.resolved[0] != "drop" {
		t.Errorf("apply wrote %v, want [drop]", store.resolved)
	}
	if res.Resolved != 1 {
		t.Errorf("res.Resolved = %d, want 1", res.Resolved)
	}
}

func TestRunFailsFatallyOnClassifierError(t *testing.T) {
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "keep", Content: "Graph-expansion RESOLVED NO-GO decision record"},
		{ID: "drop", Content: "kill experiment finding: 7.3% cross-session links, removed"},
	}}
	// Both survive the prefilter (both contain keywords); classification fails
	// partway through the batch on the second one.
	cls := fakeClassifier{errOn: "kill experiment finding: 7.3% cross-session links, removed",
		err: errors.New("boom")}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err == nil {
		t.Fatalf("Run: want error, got nil (res=%+v confirmed=%v)", res, confirmed)
	}
	if len(store.resolved) != 0 {
		t.Errorf("store.resolved = %v, want empty — a partial pass must never be applied", store.resolved)
	}
}
