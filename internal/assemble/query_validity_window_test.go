package assemble

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestValidityFiltersBeforeTheQueryWindowCloses is #1053. A query search with a
// small limit reported nothing while valid matches existed, because expired rows
// outranked them: the legs fetched a depth the window and stage 2 filtered after
// the cut, so an expired row filled every slot and the valid match ranked below
// the fetch was never seen. The legs must apply the window predicate BEFORE their
// LIMIT — the mirror of the passive path (#892) and of what
// docs/architecture.md states, that filters precede window closure.
func TestValidityFiltersBeforeTheQueryWindowCloses(t *testing.T) {
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
	create := func(content string, validUntil *string) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual",
			Importance: 0.7, ValidUntil: validUntil,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	expired := "2020-01-01 00:00:00"
	if _, ok := memory.ParseStamp(expired); !ok {
		t.Fatalf("the fixture's expiry %q is not a stamp the store can read", expired)
	}

	// Twelve expired rows that match every query term several times, so bm25
	// ranks all of them above the valid rows — which is the whole shape of the
	// bug: they outrank the matches the caller can use.
	for i := range 12 {
		create(fmt.Sprintf(
			"kubernetes upgrade runbook procedure: kubernetes upgrade runbook procedure step %d, run the kubernetes upgrade procedure from the runbook", i),
			&expired)
	}
	// Three valid rows (no window, so their window contains Now) that state the
	// same subject but match fewer query terms, so they rank below the expired
	// rows and below the leg's depth.
	var valid []string
	for i := range 3 {
		valid = append(valid, create(fmt.Sprintf(
			"current guidance: the upgrade procedure for the cluster is in the wiki page%d", i), nil))
	}

	req := Request{
		ProjectID: "proj",
		Query:     "kubernetes upgrade runbook procedure",
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 3},
		Condition: CondFTSOnly,
		Now:       time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}
	res := run(t, s, req)

	if len(res.Items) != 3 {
		t.Fatalf("items = %d (%v), want the 3 valid rows: expired rows must not spend the window", len(res.Items), itemIDs(res.Items))
	}
	for _, id := range itemIDs(res.Items) {
		if id != valid[0] && id != valid[1] && id != valid[2] {
			t.Errorf("item %s is an expired row; want only the valid matches", id)
		}
	}
	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: valid matches exist", res.Outcome)
	}
}

// TestValidityFiltersTheVectorLegBeforeTheCut is the vector half of #1053: the
// cosine leg must withhold an out-of-window row before its top-k cut for the
// same reason the keyword leg does, or a vector search with a small limit hides
// the valid rows ranked just below the expired ones.
func TestValidityFiltersTheVectorLegBeforeTheCut(t *testing.T) {
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
	// A query vector and per-row vectors that put every expired row strictly
	// closer than every valid one, so the expired rows fill the leg's depth
	// unless validity narrows the scan first.
	query := []float32{1, 0.5, 0, 0}
	add := func(content string, validUntil *string, vec []float32) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual",
			Importance: 0.7, ValidUntil: validUntil,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%q): %v", content, err)
		}
		return id
	}
	expired := "2020-01-01 00:00:00"
	for i := range 12 {
		add(fmt.Sprintf("expired row %d", i), &expired,
			[]float32{1, 0.5, 0.001 * float32(i+1), 0})
	}
	var valid []string
	for i := range 3 {
		valid = append(valid, add(fmt.Sprintf("valid row %d", i), nil,
			[]float32{1, 0.5, 0.05 + 0.001*float32(i+1), 0}))
	}

	req := Request{
		ProjectID: "proj",
		Query:     "some query",
		QueryVec:  query,
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 3},
		Condition: CondVectorOnly,
		Now:       time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}
	res := run(t, s, req)

	if len(res.Items) != 3 {
		t.Fatalf("items = %d (%v), want the 3 valid rows: expired rows must not spend the vector depth", len(res.Items), itemIDs(res.Items))
	}
	for _, id := range itemIDs(res.Items) {
		if id != valid[0] && id != valid[1] && id != valid[2] {
			t.Errorf("item %s is an expired row; want only the valid matches", id)
		}
	}
}
