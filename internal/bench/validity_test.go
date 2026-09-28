package bench

import (
	"context"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The validity fixture rows exist so the corpus can tell a retrieval stage that
// honours valid_from/valid_until from one that ignores it. That is only true if
// the stamps survive the trip from the JSONL through Seed into the store and out
// through the retrieval read the assembler's stage 2 consumes — a corpus column
// that never leaves the loader exercises nothing.

// wantValidity is one fixture row's expected claim. Dates are compared as
// instants, not as text: the corpus states a date and the store holds what the
// writer was given, and this test is about the claim surviving, not about which
// layout a layer chose to keep it in. term is a lexical handle unique to that
// row, because a retrieval read has to be able to find the row before it can
// prove anything about what came back with it.
var wantValidity = []struct {
	key                               string
	term                              string
	from, until, verified             string
	wantFrom, wantUntil, wantVerified bool
}{
	{key: "validity_expired_dbsync_window", term: "decommissioned", from: "2020-01-01", until: "2020-06-01", wantFrom: true, wantUntil: true},
	{key: "validity_future_mirror_launch", term: "cutover", from: "2099-01-01", until: "2199-12-31", wantFrom: true, wantUntil: true},
	{key: "validity_open_backup_horizon", term: "snapshots", from: "2024-01-01", until: "2199-12-31", wantFrom: true, wantUntil: true},
	{key: "validity_current_wallet_policy", term: "governance", from: "2024-01-01", verified: "2026-01-01", wantFrom: true, wantVerified: true},
}

// instant reads a stored stamp as a time, failing when it is absent.
func instant(t *testing.T, key, field string, s *string) time.Time {
	t.Helper()
	if s == nil {
		t.Fatalf("%s.%s is NULL, want a stored stamp", key, field)
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02", time.RFC3339} {
		if at, err := time.Parse(layout, *s); err == nil {
			return at
		}
	}
	t.Fatalf("%s.%s = %q, which is not a readable stamp", key, field, *s)
	return time.Time{}
}

// TestBuiltinDatasetCarriesValidityIntoRetrieval is the fixture's own guarantee:
// the validity_* rows reach the store with their claims intact and come back out
// of the retrieval read with them still attached.
//
// The read is Candidates rather than SearchHybrid because that is the seam the
// assembler's stage 2 evaluates — it is the shape a stage-2 consumer receives,
// so a test over a different read would leave the actual consumer unproven. Each
// row is fetched with a term only it contains, so the assertion is about that row
// and not about whichever rows happen to rank near it.
func TestBuiltinDatasetCarriesValidityIntoRetrieval(t *testing.T) {
	ds, vecs := loadTestdataDataset(t)
	// The connection as well as the store: this branch's Seed takes the db so it
	// can backdate created_at, which is not something the store's API can do, and
	// the headline dataset is seeded through the same call. The test's assertions
	// are unchanged; only the fixture construction follows Seed's signature.
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	if _, err := Seed(ctx, store, db, ds, vecs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	byKey := map[string]*MemorySpec{}
	for i := range ds.Memories {
		byKey[ds.Memories[i].Key] = &ds.Memories[i]
	}

	for _, want := range wantValidity {
		t.Run(want.key, func(t *testing.T) {
			spec, ok := byKey[want.key]
			if !ok {
				t.Fatalf("corpus row %q is missing, so the validity fixture is not in the dataset at all", want.key)
			}
			const window = 100
			set, err := store.Candidates(ctx, memory.CandidateRequest{
				ProjectID: ds.Project,
				Mode:      memory.AllProjects,
				Query:     want.term,
				Condition: memory.CondFTSOnly,
				Fetch:     memory.Fetch{FTSTopK: window, VectorTopK: window, Limit: window},
				Now:       time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			var got *memory.Candidate
			for i := range set.Rows {
				if set.Rows[i].Content == spec.Content {
					got = &set.Rows[i]
				}
			}
			if got == nil {
				t.Fatalf("%q is not among the %d candidates for %q, so the fixture never reaches retrieval",
					want.key, len(set.Rows), want.term)
			}
			c := *got
			for _, tc := range []struct {
				field string
				got   *string
				want  string
				set   bool
			}{
				{"valid_from", c.ValidFrom, want.from, want.wantFrom},
				{"valid_until", c.ValidUntil, want.until, want.wantUntil},
				{"verified_at", c.VerifiedAt, want.verified, want.wantVerified},
			} {
				if !tc.set {
					if tc.got != nil {
						t.Errorf("%s.%s = %q, want NULL — a row that states no claim must not acquire one in the corpus", want.key, tc.field, *tc.got)
					}
					continue
				}
				if gotAt, wantAt := instant(t, want.key, tc.field, tc.got), stampInstant(t, tc.want); !gotAt.Equal(wantAt) {
					t.Errorf("%s.%s = %v, want %v — the stamp did not survive seeding", want.key, tc.field, gotAt, wantAt)
				}
			}
		})
	}
}

// stampInstant parses a fixture date the way the test declares it.
func stampInstant(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("fixture date %q is not a date: %v", s, err)
	}
	return at
}

// TestValidityFixtureCoversEveryStage2State is the reason there are four rows
// rather than one. Stage 2 reads a claim in four distinct ways, and a fixture
// that exercised only one of them would let a filter that excludes everything
// with a validity column — or nothing at all — pass.
//
// The states are derived from the rows against the current clock rather than
// listed, so a fixture whose dates drifted out of the coverage fails here instead
// of going quietly useless. One row per state is asserted too: a fifth row that
// duplicated a state would leave the coverage exactly as it was while making the
// corpus bigger for the graded conditions, which is cost without signal. The
// fifth state — unset, meaning no claim at all — is not asserted, because the
// other 551 rows are it.
func TestValidityFixtureCoversEveryStage2State(t *testing.T) {
	ds, _ := loadTestdataDataset(t)
	now := time.Now().UTC()

	got := map[string]bool{}
	dated := 0
	for _, m := range ds.Memories {
		if !hasValidity(m) {
			continue
		}
		dated++
		from, until, verified := derefStamp(m.ValidFrom), derefStamp(m.ValidUntil), derefStamp(m.VerifiedAt)
		switch {
		case until != nil && until.Before(now):
			got["expired"] = true
		case from != nil && from.After(now):
			got["future"] = true
		case (from != nil || until != nil) && verified == nil:
			got["unverified"] = true
		default:
			got["valid"] = true
		}
	}
	for _, want := range []string{"expired", "future", "unverified", "valid"} {
		if !got[want] {
			t.Errorf("no fixture row reads as %q against a %s clock; the corpus cannot tell a filter that honours validity from one that ignores it", want, now.Format("2006-01-02"))
		}
	}
	if len(got) != dated {
		t.Errorf("%d dated rows cover %d of the four states; each row earns its place by covering one the others do not", dated, len(got))
	}
}

func hasValidity(m MemorySpec) bool {
	return m.ValidFrom != nil || m.ValidUntil != nil || m.VerifiedAt != nil
}

func derefStamp(s *string) *time.Time {
	if s == nil {
		return nil
	}
	at, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil
	}
	return &at
}
