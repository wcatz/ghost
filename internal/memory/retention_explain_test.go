package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExplainNamesTheSessionDecay: "bounded" is a claim about the arithmetic and
// "named in explain" is a claim about the diagnosis, and a reader who cannot see
// the signal in explain has no way to check the arithmetic against a result. So
// explain reports the tier, reports the multiplier the tier contributed, and —
// when any candidate carries one — says in words that the factor is there and
// what it is bounded by.
//
// The note is per-search rather than per-row because a factor is only a signal if
// some row has it: a corpus with no session memories should not be told about
// decay it is not applying.
func TestExplainNamesTheSessionDecay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two rows that match the same keywords and are NOT near-duplicates of each
	// other, one of them session-tier, so the difference in their factors is
	// visible in one explanation. The unlike wording is load-bearing: two
	// paraphrases fold, and a fold raises the surviving row's tier, which would
	// leave nothing for this test to measure.
	sessionID, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the staging cluster drains its queue before a rolling restart of the shed pods", "mcp", 0.7, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the session row: %v", err)
	}
	durableID, _, _, err := s.Upsert(ctx, testProject, "fact",
		"production replicas lag whenever the ingest queue backs up, so drain it first", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("save the durable row: %v", err)
	}
	// Old enough that the tier factor has actually bitten: a brand-new session row
	// is worth 1.0 and would report a factor of 1.0, which proves nothing.
	setCreatedAtDaysAgo(t, s, sessionID, 30)
	setCreatedAtDaysAgo(t, s, durableID, 30)

	ex, err := s.ExplainSearch(ctx, testProject, "cluster drains queue", nil, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}

	rows := map[string]ExplainRow{}
	for _, r := range ex.Rows {
		rows[r.ID] = r
	}
	sessionRow, ok := rows[sessionID]
	if !ok {
		t.Fatalf("the session row is not in the explanation at all: %+v", ex.Rows)
	}
	durableRow, ok := rows[durableID]
	if !ok {
		t.Fatalf("the durable row is not in the explanation at all: %+v", ex.Rows)
	}

	if sessionRow.Retention != RetentionSession {
		t.Errorf("explain does not report the tier: retention = %q, want %q", sessionRow.Retention, RetentionSession)
	}
	if durableRow.Retention != RetentionProject {
		t.Errorf("the durable row's tier reads %q, want %q", durableRow.Retention, RetentionProject)
	}
	if durableRow.RetentionFactor != 1.0 {
		t.Errorf("a durable row's retention factor = %v, want 1.0: adopting tiers must not re-rank an existing corpus", durableRow.RetentionFactor)
	}
	if sessionRow.RetentionFactor >= 1.0 || sessionRow.RetentionFactor <= 0 {
		t.Errorf("the session row's retention factor = %v, want a strict fraction of 1.0", sessionRow.RetentionFactor)
	}
	// The factor has to be the one that ranked, not a second computation of it:
	// decay_factor is the whole multiplier, so the tier half has to be inside it.
	if want := RetentionDecayFactor(RetentionSession, sessionRow.AgeDays); sessionRow.RetentionFactor != want {
		t.Errorf("retention_factor = %v, want %v for age %v", sessionRow.RetentionFactor, want, sessionRow.AgeDays)
	}
	if got, want := sessionRow.DecayFactor, DecayFactor("fact", RetentionSession, false, sessionRow.AgeDays); got != want {
		t.Errorf("decay_factor = %v, want %v — the tier factor is not inside the decay that ranked", got, want)
	}
	if sessionRow.DecayFactor >= durableRow.DecayFactor {
		t.Errorf("a 30-day session row (%v) does not decay below its durable twin (%v)", sessionRow.DecayFactor, durableRow.DecayFactor)
	}

	var named bool
	for _, note := range ex.Notes {
		if strings.Contains(note, "session") && strings.Contains(note, "decay") {
			named = true
		}
	}
	if !named {
		t.Errorf("explain does not NAME the session decay in a note: %v", ex.Notes)
	}
}

// TestExplainSaysNothingAboutATierItIsNotApplying: a corpus with no session
// memories has no session decay, and a diagnosis that describes a signal nothing
// ranked is a diagnosis with one more thing to read past.
func TestExplainSaysNothingAboutATierItIsNotApplying(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a durable note about the tunnel", Source: "manual", Importance: 0.7,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ex, err := s.ExplainSearch(ctx, testProject, "tunnel", nil, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	for _, note := range ex.Notes {
		if strings.Contains(note, "retention") || strings.Contains(note, "session") {
			t.Errorf("explain mentions a retention signal for a corpus with no session row: %q", note)
		}
	}
	for _, r := range ex.Rows {
		if r.RetentionFactor != 1.0 {
			t.Errorf("row %s reports a retention factor of %v with no session row in the corpus", r.ID, r.RetentionFactor)
		}
	}
}

// TestTheExplainTierFactorIsTheRankingOne: the multiply lives in DecayFactor, so
// this is the statement that a row's reported factor cannot drift from the one
// that ranked it. It is cheap and it is the whole reason the field exists.
func TestTheExplainTierFactorIsTheRankingOne(t *testing.T) {
	for _, age := range []float64{0, 1, 7, 45, 900} {
		for _, cat := range []string{"fact", "pattern", "gotcha"} {
			durable := DecayFactor(cat, RetentionProject, false, age)
			session := DecayFactor(cat, RetentionSession, false, age)
			if want := categoryDecay(cat, age) * RetentionDecayFactor(RetentionSession, age); session != want {
				t.Errorf("category=%s age=%v: session decay %v, want the category curve times the tier factor %v", cat, age, session, want)
			}
			if durable != categoryDecay(cat, age) {
				t.Errorf("category=%s age=%v: a durable row's decay %v changed with tiers", cat, age, durable)
			}
		}
	}
	// The clock bound explain and the ranking share, so the age a row is judged on
	// is the age the report used: both come from the same instant. Pinned stays a
	// full exemption, above the tier, or a keep-forever row with a stale pin would
	// start decaying.
	if got := DecayFactor("fact", RetentionSession, true, 900); got != 1.0 {
		t.Errorf("a pinned session row's decay = %v, want 1.0: the pin is a full exemption", got)
	}
	_ = time.Now
}
