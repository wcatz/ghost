package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestAQueryWhoseOnlyMatchRetiredStaysAllInvalid is the other half of #1053.
func TestAQueryWhoseOnlyMatchRetiredStaysAllInvalid(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	expired := "2020-01-01 00:00:00"
	if _, err := s.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the kubernetes upgrade runbook is on the wiki", Source: "manual",
		Importance: 0.7, ValidUntil: &expired,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := Request{
		ProjectID: "proj",
		Query:     "kubernetes upgrade runbook",
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 3},
		Condition: CondFTSOnly,
		Now:       time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}
	res := run(t, s, req)

	if len(res.Items) != 0 {
		t.Fatalf("items = %v, want none: the only match is out of its window", itemIDs(res.Items))
	}
	if res.Outcome != OutcomeEmpty {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeEmpty)
	}
	if res.Reason != "all_invalid" {
		t.Errorf("reason = %q, want all_invalid: the query matched, and the rows it matched were "+
			"withheld as out of date, which is not the same fact as nothing matching", res.Reason)
	}
	if !strings.Contains(res.Abstention, "withheld as out of date") {
		t.Errorf("abstention = %q, want it to say the candidates were withheld as out of date", res.Abstention)
	}
}

// expiredSemanticOnlyStore builds a store holding one expired row whose stored
// embedding IS the query vector but whose text shares no term with the query — a
// match only the vector leg can see. It is the fixture for the two reason tests
// below: the keyword leg has nothing to withhold, so the empty reason has to come
// from the vector leg's own count. The window is closed (2020) and Now is 2026.
func expiredSemanticOnlyStore(t *testing.T, queryVec []float32) *memory.Store {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	expired := "2020-01-01 00:00:00"
	id, err := s.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the quarterly banana harvest is in the ledger",
		Source: "manual", Importance: 0.7, ValidUntil: &expired,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.StoreEmbedding(ctx, id, queryVec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	return s
}

// assertRetiredMatchWithheld is the shared assertion for a query whose only
// match is out of its window: the result is empty, and its reason says rows were
// found and withheld as out of date rather than that nothing matched.
func assertRetiredMatchWithheld(t *testing.T, res Result, path string) {
	t.Helper()
	if len(res.Items) != 0 {
		t.Fatalf("%s: items = %v, want none: the only match is out of its window", path, itemIDs(res.Items))
	}
	if res.Outcome != OutcomeEmpty {
		t.Errorf("%s: outcome = %q, want %q", path, res.Outcome, OutcomeEmpty)
	}
	if res.Reason != "all_invalid" {
		t.Errorf("%s: reason = %q, want all_invalid: the query's only match was withdrawn as out of "+
			"date, which is not the same fact as nothing matching", path, res.Reason)
	}
	if !strings.Contains(res.Abstention, "withheld as out of date") {
		t.Errorf("%s: abstention = %q, want it to say the candidates were withheld as out of date", path, res.Abstention)
	}
}

// TestAVectorOnlyQueryWhoseOnlyMatchRetiredStaysAllInvalid is the semantic-only
// half of the reason #1053 has to preserve.
func TestAVectorOnlyQueryWhoseOnlyMatchRetiredStaysAllInvalid(t *testing.T) {
	qvec := []float32{1, 0, 0, 0}
	s := expiredSemanticOnlyStore(t, qvec)
	res := run(t, s, Request{
		ProjectID: "proj",
		Query:     "kubernetes upgrade runbook",
		QueryVec:  qvec,
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 3},
		Condition: CondVectorOnly,
		Now:       time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	})
	assertRetiredMatchWithheld(t, res, "vector-only")
}
