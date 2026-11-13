package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// fakeClassifier returns an explicit verdict for each content, and counts how
// many times the batch path was invoked (one count per IsResolvedBatch call,
// whatever the chunking).
type fakeClassifier struct {
	drop    map[string]bool
	unknown map[string]bool
	// errOn, when set, is the content for which IsResolvedBatch returns err
	// instead of a normal answer. If err is set and errOn is empty, every call
	// errors.
	errOn string
	err   error
	// truncate, when set, drops the last verdict so a classifier returning
	// fewer verdicts than contents can be exercised.
	truncate bool
	calls    int
	// askedFor records every content passed to IsResolvedBatch, so a test can
	// assert WHICH notes reached the harness and not just how many calls it
	// took: a scoped pass that asked about a row it was told to leave alone is
	// billed work and a wrong answer either way.
	askedFor []string
}

func (f *fakeClassifier) IsResolvedBatch(_ context.Context, contents []string) ([]Verdict, error) {
	f.calls++
	f.askedFor = append(f.askedFor, contents...)
	if f.err != nil {
		for _, c := range contents {
			if f.errOn == "" || f.errOn == c {
				return nil, f.err
			}
		}
	}
	out := make([]Verdict, len(contents))
	for i, c := range contents {
		switch {
		case f.unknown[c]:
			out[i] = VerdictUnknown
		case f.drop[c]:
			out[i] = VerdictResolved
		default:
			out[i] = VerdictKeep
		}
	}
	if f.truncate && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

// fakeStore satisfies resolveStore with in-memory candidates.
type fakeStore struct {
	candidates        []memory.Memory
	links             []memory.Link
	resolved          []string
	err               error // when set, returned by SetResolvedJudged instead of writing
	setResolvedCalled bool
	// moved is the content the store holds for an id when it differs from the
	// candidate's — a rewrite landing between the read and the classify, which is
	// exactly what SetResolvedJudged refuses to stamp (#1052).
	moved           map[string]string
	kept            map[string]string   // pre-seeded resolve KEEP cache
	markedKept      []map[string]string // hashes passed to MarkResolveKept, one per call
	markErr         error               // when set, returned by MarkResolveKept
	alreadyResolved []memory.Memory     // ResolvedCandidates pool (reassess)
	cleared         []string            // ids passed to ClearResolved
	clearErr        error               // when set, returned by ClearResolved
	// known holds every row GetByIDs can serve. It defaults to the union of
	// both candidate pools so a test that does not care about endpoint scopes
	// does not have to restate its rows; scopeCompatibleSupersedes needs the
	// source endpoint, which the candidate pools do not always contain.
	known []memory.Memory
	// usefulness is the flag-evidence map the store would have read,
	// and usefulnessCalls counts the READS — the property under test is that a
	// pass over N memories asks once, so a per-memory query cannot pass.
	usefulness      map[string]memory.FlagEvidence
	usefulnessCalls int
	usefulnessErr   error
}

func (s *fakeStore) FlagsByMemory(_ context.Context, _ string) (map[string]memory.FlagEvidence, error) {
	s.usefulnessCalls++
	if s.usefulnessErr != nil {
		return nil, s.usefulnessErr
	}
	return s.usefulness, nil
}

func (s *fakeStore) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	pool := s.known
	if len(pool) == 0 {
		pool = append(append([]memory.Memory{}, s.candidates...), s.alreadyResolved...)
	}
	var out []memory.Memory
	for _, m := range pool {
		if want[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeStore) ResolveCandidates(_ context.Context, _ string) ([]memory.Memory, error) {
	return s.candidates, nil
}
func (s *fakeStore) SetResolvedJustified(_ context.Context, judged []memory.Memory, _ map[string]memory.ResolveJustification) ([]string, []string, error) {
	s.setResolvedCalled = true
	if s.err != nil {
		return nil, nil, s.err
	}
	var stamped, stale []string
	for _, m := range judged {
		if current, ok := s.moved[m.ID]; ok && current != m.Content {
			stale = append(stale, m.ID)
			continue
		}
		stamped = append(stamped, m.ID)
	}
	s.resolved = append(s.resolved, stamped...)
	return stamped, stale, nil
}
func (s *fakeStore) LinksByRelationSource(_ context.Context, _, _, _ string) ([]memory.Link, error) {
	return s.links, nil
}
func (s *fakeStore) ResolveKeptHashes(_ context.Context, _ string) (map[string]string, error) {
	out := make(map[string]string, len(s.kept))
	for id, hash := range s.kept {
		out[id] = hash
	}
	return out, nil
}
func (s *fakeStore) ResolvedCandidates(_ context.Context, _ string) ([]memory.Memory, error) {
	return s.alreadyResolved, nil
}
func (s *fakeStore) ClearResolved(_ context.Context, _ string, ids []string) (int, error) {
	if s.clearErr != nil {
		return 0, s.clearErr
	}
	s.cleared = append(s.cleared, ids...)
	return len(ids), nil
}
func (s *fakeStore) MarkResolveKept(_ context.Context, _ string, hashes map[string]string) error {
	if s.markErr != nil {
		return s.markErr
	}
	if s.kept == nil {
		s.kept = make(map[string]string)
	}
	for id, hash := range hashes {
		s.kept[id] = hash
	}
	s.markedKept = append(s.markedKept, hashes)
	return nil
}

func TestRunResolvesConfirmedEvidence(t *testing.T) {
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "keep", Content: "Graph-expansion RESOLVED NO-GO decision record"},
		{ID: "drop", Content: "kill experiment finding: 7.3% cross-session links, removed"},
		{ID: "noise", Content: "unrelated architecture note about workers"},
	}}
	cls := &fakeClassifier{drop: map[string]bool{
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
	cls := &fakeClassifier{drop: map[string]bool{}}

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
	cls := &fakeClassifier{drop: map[string]bool{}}

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

func TestContentHashVersionsExplicitKeepCache(t *testing.T) {
	const content = "kill experiment finding: 7.3% cross-session links, removed"
	sum := sha256.Sum256([]byte(keepCacheHashVersion + "\x00" + content))
	want := hex.EncodeToString(sum[:])
	if got := ContentHash(content); got != want {
		t.Errorf("ContentHash(%q) = %q, want %q", content, got, want)
	}
}

// TestRunReasksV2CachedKeepVerdicts: the cache key is versioned, and issue #640
// changed what a KEEP verdict means (a bare RESOLVED is now a KEEP, imperatives
// and open markers veto before the call). A row cached under the old prefix was
// judged by the old rules, so it must be re-asked rather than trusted.
func TestRunReasksV2CachedKeepVerdicts(t *testing.T) {
	content := "kill experiment finding: 7.3% cross-session links, removed"
	v2 := sha256.Sum256([]byte("v2\x00" + content))
	store := &fakeStore{
		candidates: []memory.Memory{{ID: "kept", Content: content}},
		kept:       map[string]string{"kept": hex.EncodeToString(v2[:])},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, _, err := Run(context.Background(), store, cls, "proj", false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 — a v2 cache entry must be re-asked", cls.calls)
	}
	if res.Skipped != 0 {
		t.Errorf("res.Skipped = %d, want 0 for a stale-prefix cache entry", res.Skipped)
	}
}

// TestRunRecordsKeepHashesOnlyOnApply: a cache miss goes through one batched
// call; explicit KEEP verdicts are hash-cached only when apply is set, so
// dry-run remains side-effect-free.
func TestRunRecordsKeepHashesOnlyOnApply(t *testing.T) {
	keepContent := "kill experiment finding: 7.3% cross-session links, removed"
	dropContent := "fixed in PR #210, dead ranking bonus removed"
	store := &fakeStore{candidates: []memory.Memory{
		{ID: "keep", Content: keepContent},
		{ID: "drop", Content: dropContent},
	}}
	cls := &fakeClassifier{drop: map[string]bool{dropContent: true}}

	res, _, err := Run(context.Background(), store, cls, "proj", false, nil)
	if err != nil {
		t.Fatalf("Run dry: %v", err)
	}
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 batched call", cls.calls)
	}
	if res.Skipped != 0 {
		t.Errorf("res.Skipped = %d, want 0 on a cache miss", res.Skipped)
	}
	if len(store.markedKept) != 0 {
		t.Errorf("dry run recorded %v, want nothing", store.markedKept)
	}

	_, _, err = Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if len(store.markedKept) != 1 || len(store.markedKept[0]) != 1 {
		t.Fatalf("apply markedKept = %v, want exactly one KEEP hash", store.markedKept)
	}
	if got := store.markedKept[0]["keep"]; got != ContentHash(keepContent) {
		t.Errorf("recorded hash = %q, want %q", got, ContentHash(keepContent))
	}
	if _, ok := store.markedKept[0]["drop"]; ok {
		t.Errorf("resolved verdict must not be KEEP-cached: %v", store.markedKept[0])
	}
}
