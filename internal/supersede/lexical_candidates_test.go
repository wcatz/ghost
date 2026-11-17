package supersede

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The recategorised-replacement storyline's pair, word for word: the old cache
// is a fact, the team's change is a decision. Their embeddings are orthogonal,
// so the cosine scan cannot propose them at any threshold.
const (
	lexOlder = "Lookup responses are cached in Memcached: internal/cache dials memcached-01.corp.example:11211 and every entry lives 300s."
	lexNewer = "Decision: lookup responses are cached in-process with ristretto instead of Memcached; internal/cache keeps a 64 MB budget per pod, entries still live 300s, and memcached-01 is being shut down."
)

func addLexical(t *testing.T, store *memory.Store, db *sql.DB, category, content, stamp string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, "p", memory.Memory{Category: category, Content: content, Importance: 0.7, Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at=?, updated_at=? WHERE id=?`, stamp, stamp, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSelectCandidatesProposesAReplacementCosineCannotSee: the cosine scan drops
// a replacement the agent reworded around the value (and filed under another
// category), so the classifier was never asked about the pair the arc annotates.
// The lexical replacement guess is a second candidate source; the classifier
// still decides, so this only makes sure the pair reaches it.
func TestSelectCandidatesProposesAReplacementCosineCannotSee(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := addLexical(t, store, db, "fact", lexOlder, "2026-01-01 00:00:00", []float32{1, 0, 0})
	newer := addLexical(t, store, db, "decision", lexNewer, "2026-07-01 00:00:00", []float32{0.4, 0.9, 0})

	sel, err := SelectCandidates(ctx, store, "p", 0.7)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	for _, c := range sel.Candidates {
		if c.NewerID == newer && c.OlderID == older {
			return
		}
	}
	t.Fatalf("candidate set %+v does not propose %s -> %s: the reworded, recategorised replacement never reaches the classifier", sel.Candidates, newer, older)
}

// TestSelectCandidatesRescuesEveryAnnotatedReplacementBelowTheFloor: all three
// truth-change arcs' pairs score below the default cosine floor. Each one is
// proposed by the lexical rescue, so a real classifier is asked about it instead
// of the edge being lost in candidate generation.
func TestSelectCandidatesRescuesEveryAnnotatedReplacementBelowTheFloor(t *testing.T) {
	pairs := []struct {
		name, older, newer string
	}{
		{"renamed endpoint",
			"Partners submit shipment events with POST /v1/ingest, served by ingestHandler in api/ingest.go; the body is a JSON array of up to 500 events.",
			"The shipment-event endpoint was renamed: partners now call POST /v2/events, served by eventsHandler in api/events.go, and the old /v1/ingest path returns 410 Gone."},
		{"recategorised cache", lexOlder, lexNewer},
		{"regressed fix",
			"Fixed in commit 4f2a9c1: the exporter initialises the per-tenant totals map before the loop, so a tenant with no usage rows no longer crashes the nightly export; the job has been green since.",
			"Regression: the nightly export panics again with 'assignment to entry in nil map' when a tenant has no usage rows; the tenant-loader refactor in commit 9be71d0 dropped the totals-map initialisation that 4f2a9c1 added, so the 4f2a9c1 change no longer holds."},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			older := addLexical(t, store, db, "fact", tc.older, "2026-01-01 00:00:00", []float32{1, 0, 0})
			newer := addLexical(t, store, db, "decision", tc.newer, "2026-07-01 00:00:00", []float32{0.4, 0.9, 0})
			sel, err := SelectCandidates(ctx, store, "p", 0.7)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range sel.Candidates {
				if c.NewerID == newer && c.OlderID == older {
					return
				}
			}
			t.Fatalf("candidate set %+v does not propose %s -> %s", sel.Candidates, newer, older)
		})
	}
}

// TestLexicalRescueHonoursValidityAtTheInjectedClock: an expired row is not a
// party to the lexical guess, so the rescue does not propose a pair one side of
// which no longer states a live claim. The clock is injected so the boundary is
// testable without sleeping.
func TestLexicalRescueHonoursValidityAtTheInjectedClock(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := addLexical(t, store, db, "fact", lexOlder, "2026-01-01 00:00:00", []float32{1, 0, 0})
	newer := addLexical(t, store, db, "decision", lexNewer, "2026-07-01 00:00:00", []float32{0.4, 0.9, 0})
	if _, err := db.ExecContext(ctx, `UPDATE memories SET valid_until='2026-08-01 00:00:00' WHERE id=?`, newer); err != nil {
		t.Fatal(err)
	}

	afterExpiry := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	beforeExpiry := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	proposed := func(now time.Time) bool {
		replacementNow = func() time.Time { return now }
		sel, err := SelectCandidates(ctx, store, "p", 0.7)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range sel.Candidates {
			if c.NewerID == newer && c.OlderID == older {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { replacementNow = time.Now })
	if proposed(afterExpiry) {
		t.Error("an expired row was rescued into a candidate: validity is not read at the injected clock")
	}
	if !proposed(beforeExpiry) {
		t.Error("a live row was not rescued at a clock before its expiry: the fixture no longer reaches the rescue")
	}
}

// TestRescuedEdgeKeepsItsBelowFloorSimilarityAsStrength: a rescued pair was
// proposed by the lexical guess, not by cosine, but its honest score is still
// the link strength — no invented value. Nothing on the read side gates a
// 'supersedes' link on strength: only 'related' links are compared with the
// demotion threshold, and the supersede penalty counts live edges. The demotion
// assertion is what makes that explicit rather than assumed.
func TestRescuedEdgeKeepsItsBelowFloorSimilarityAsStrength(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := addLexical(t, store, db, "fact", lexOlder, "2026-01-01 00:00:00", []float32{1, 0, 0})
	newer := addLexical(t, store, db, "decision", lexNewer, "2026-07-01 00:00:00", []float32{0.4, 0.9, 0})
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}

	res, _, err := Run(ctx, store, cls, "p", 0.7, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d edges, want 1", res.Created)
	}
	links, err := store.LinksByRelationSource(ctx, "p", "supersedes", "llm")
	if err != nil {
		t.Fatal(err)
	}
	var strength float32
	found := false
	for _, l := range links {
		if l.SourceID == newer && l.TargetID == older {
			strength, found = l.Strength, true
		}
	}
	if !found {
		t.Fatalf("links %+v do not hold the rescued edge %s -> %s", links, newer, older)
	}
	// cosine({1,0,0},{0.4,0.9,0}) = 0.4 / sqrt(0.97) ≈ 0.406: below the
	// 0.7 floor, and the score the scan actually computed.
	if strength <= 0.4 || strength >= 0.7 {
		t.Errorf("rescued edge strength = %.4f, want the below-floor cosine in (0.4, 0.7)", strength)
	}
	penalties, err := memory.SupersedePenalties(ctx, db, []string{newer, older}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if penalties[older] != 1 {
		t.Errorf("supersede penalty on the older row = %d, want 1: a below-floor strength must not keep the edge from demoting", penalties[older])
	}
}

// TestRunClassifiesARecategorisedReplacementWithNoCosine: the same pair end to
// end — a SUPERSEDES verdict writes the edge even though no vector proposed it.
func TestRunClassifiesARecategorisedReplacementWithNoCosine(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := addLexical(t, store, db, "fact", lexOlder, "2026-01-01 00:00:00", []float32{1, 0, 0})
	newer := addLexical(t, store, db, "decision", lexNewer, "2026-07-01 00:00:00", []float32{0.4, 0.9, 0})
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}

	res, _, err := Run(ctx, store, cls, "p", 0.7, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d edges, want 1: the recategorised replacement was not judged", res.Created)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Errorf("live supersedes edges = %v, want the one from the lexical pair", pairs)
	}
}
