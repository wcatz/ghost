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
	candidates        []memory.Memory
	links             []memory.Link
	resolved          []string
	err               error // when set, returned by SetResolved instead of writing
	setResolvedCalled bool
}

func (s *fakeStore) ResolveCandidates(_ context.Context, _ string) ([]memory.Memory, error) {
	return s.candidates, nil
}
func (s *fakeStore) SetResolved(_ context.Context, ids []string) (int, error) {
	s.setResolvedCalled = true
	if s.err != nil {
		return 0, s.err
	}
	s.resolved = append(s.resolved, ids...)
	return len(ids), nil
}
func (s *fakeStore) LinksByRelationSource(_ context.Context, _, _, _ string) ([]memory.Link, error) {
	return s.links, nil
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

// TestPrefilterCatchesEstimateAndPostmortemPhrasings: eval-cycle corpus
// evidence (finding F2, issue #336) showed concluded-work phrasings like cost
// estimates and downtime estimates slipping past the keyword net.
func TestPrefilterCatchesEstimateAndPostmortemPhrasings(t *testing.T) {
	in := []memory.Memory{
		{ID: "est", Content: "Cost estimate from May: Fly.io projected $148/mo at current traffic."},
		{ID: "downtime", Content: "Migration plan estimated a 20-minute downtime window for final cutover; cutover completed 07-19."},
		{ID: "pm", Content: "Postmortem (concluded): the deploy failure was a stale compose hash."},
		{ID: "inv", Content: "Investigation note: slow queue drain was a missing XACK; fixed in v0.9.4."},
		{ID: "loc", Content: "PR locator: compose file split landed in PR #405. Reference only."},
	}
	got := Prefilter(in)
	if len(got) != len(in) {
		dropped := map[string]bool{}
		for _, m := range got {
			dropped[m.ID] = true
		}
		for _, m := range in {
			if !dropped[m.ID] {
				t.Errorf("prefilter dropped resolved-evidence memory %s: %q", m.ID, m.Content)
			}
		}
	}
}

// TestRunSupersedesEdgePiggyback: the older endpoint of a live
// 'supersedes'/'llm' link is demoted deterministically — no LLM call — even
// though its content carries no resolution keyword.
func TestRunSupersedesEdgePiggyback(t *testing.T) {
	older := memory.Memory{ID: "older", Category: "gotcha",
		Content: "Reward snapshot import miscalculates: unrelated live-looking gotcha without keywords"}
	newer := memory.Memory{ID: "newer", Category: "gotcha",
		Content: "superseded the snapshot import row; never use the old calculation on import"}
	store := &fakeStore{
		candidates: []memory.Memory{older, newer},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "older", Relation: "supersedes", Source: "llm",
		}},
	}
	// Never called: the classifier returns KEEP for everything, but the piggyback
	// shouldn't need it.
	cls := fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", false, nil)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if res.Superseded != 1 {
		t.Fatalf("res.Superseded = %d, want 1", res.Superseded)
	}
	if len(confirmed) != 1 || confirmed[0].ID != "older" {
		t.Fatalf("confirmed = %v, want [older]", confirmed)
	}

	res, _, err = Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if len(store.resolved) != 1 || store.resolved[0] != "older" {
		t.Fatalf("apply wrote %v, want [older]", store.resolved)
	}
	if res.Resolved != 1 {
		t.Errorf("res.Resolved = %d, want 1", res.Resolved)
	}
}

// TestRunCorrectionPairing: a newer correction demotes an OLDER prefilter-passing
// candidate sharing rare subject tokens, while the correction itself and an
// unrelated live memory stay KEEP.
func TestRunCorrectionPairing(t *testing.T) {
	older := memory.Memory{ID: "old-report", Category: "gotcha", UpdatedAt: "2026-09-19 20:00:00",
		Content: "root cause: ledgerstate/imported_reward_inputs.go never sets CalculationVersion on imported reward_snapshot rows; unusable (closed)"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 21:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	unrelated := memory.Memory{ID: "live", Category: "gotcha", UpdatedAt: "2026-09-19 22:00:00",
		Content: "active pool operator: rotate blslib keys monthly; keep offline signing keys cold"}
	// old-report and correction share ≥3 rare subject tokens
	// (ledgerstate/imported_reward_inputs.go, calculationversion, reward_snapshot,
	// imported), so pairing fires; "live" shares none and stays KEEP.
	store := &fakeStore{candidates: []memory.Memory{older, correction, unrelated}}
	// LLM would keep everything; only deterministic pairing demotes.
	cls := fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", false, nil)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if res.Corrected != 1 {
		t.Fatalf("res.Corrected = %d, want 1", res.Corrected)
	}
	got := map[string]bool{}
	for _, m := range confirmed {
		got[m.ID] = true
	}
	if !got["old-report"] {
		t.Errorf("confirmed = %v, want old-report demoted", confirmed)
	}
	if got["correction"] || got["live"] {
		t.Errorf("confirmed = %v, correction/live must stay KEEP", confirmed)
	}
}

// TestRunCorrectionPairingSkipsLiveGotcha: F6AB3B79 regression — a memory
// sharing rare tokens with a correction but NOT passing the prefilter is never
// demoted; only prefilter-passing candidates are.
func TestRunCorrectionPairingSkipsLiveGotcha(t *testing.T) {
	live := memory.Memory{ID: "live", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
		Content: "re-bootstrap gotcha: ledgerstate/imported_reward_inputs.go mithril CalculationVersion never set on imported reward_snapshot rows — dingo-core-mithril-sync restarts"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 00:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	// "live" is strictly OLDER than the correction and shares its rare tokens,
	// but passes no resolution keyword, so it is not a candidate and must survive.
	store := &fakeStore{candidates: []memory.Memory{live, correction}}
	cls := fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Corrected != 0 {
		t.Errorf("res.Corrected = %d, want 0 (live gotcha must not be demoted)", res.Corrected)
	}
	if len(confirmed) != 0 {
		t.Errorf("confirmed = %v, want nothing demoted", confirmed)
	}
	if len(store.resolved) != 0 {
		t.Errorf("applied %v, want nothing written", store.resolved)
	}
}
