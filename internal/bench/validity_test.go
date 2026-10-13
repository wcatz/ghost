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
// the validity_* rows reach the store with their claims intact, and the retrieval
// read the assembler's stage 2 consumes WITHHOLDS an out-of-window row before the
// window closes (#1053) while the rows it does return carry their stamps.
//
// The read is Candidates because that is the seam the assembler's stage 2
// evaluates. Since #1053 the legs filter validity in SQL before their LIMIT, so
// an expired or future row is not in the set at all — the stamp survival of those
// rows is asserted through SearchFTSAll, a browse reader that deliberately returns
// an out-of-window row marked rather than withheld. A test that read an expired
// row's stamp back through Candidates would now be asserting the old contract.
func TestBuiltinDatasetCarriesValidityIntoRetrieval(t *testing.T) {
	// Parallel: opens its own stores and reads no process-global state, so it
	// overlaps the corpus-wide tests instead of running ahead of them.
	t.Parallel()
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
	now := time.Now().UTC()

	for _, want := range wantValidity {
		t.Run(want.key, func(t *testing.T) {
			spec, ok := byKey[want.key]
			if !ok {
				t.Fatalf("corpus row %q is missing, so the validity fixture is not in the dataset at all", want.key)
			}
			// The stamp survived seeding: a browse read that does not withhold
			// returns the row and the value, whatever its window says.
			browse, err := store.SearchFTSAll(ctx, want.term, 100)
			if err != nil {
				t.Fatalf("SearchFTSAll: %v", err)
			}
			seeded := memoryWithContent(browse, spec.Content)
			if seeded == nil {
				t.Fatalf("%q is not among the browse results for %q, so the fixture never reaches the store", want.key, want.term)
			}
			assertValidityStamps(t, want, *seeded)

			// The stage-2 seam: Candidates returns a row only when its window
			// contains Now, and when it does the stamps travel with it.
			const window = 100
			set, err := store.Candidates(ctx, memory.CandidateRequest{
				ProjectID: ds.Project,
				Mode:      memory.AllProjects,
				Query:     want.term,
				Condition: memory.CondFTSOnly,
				Fetch:     memory.Fetch{FTSTopK: window, VectorTopK: window, Limit: window},
				Now:       now,
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
			if withheld(spec, now) {
				if got != nil {
					t.Errorf("%q is outside its window at %s but Candidates returned it: the legs must withhold it before the cut",
						want.key, now.Format("2006-01-02"))
				}
				return
			}
			if got == nil {
				t.Fatalf("%q is inside its window but is not among the %d candidates for %q",
					want.key, len(set.Rows), want.term)
			}
			assertValidityStamps(t, want, got.Memory)
		})
	}
}

// withheld reports whether a fixture row's window excludes it at now, by the one
// rule stage 2 and the leg predicates share.
func withheld(m *MemorySpec, now time.Time) bool {
	from, until := derefStamp(m.ValidFrom), derefStamp(m.ValidUntil)
	if until != nil && until.Before(now) {
		return true
	}
	return from != nil && from.After(now)
}

func memoryWithContent(rows []memory.Memory, content string) *memory.Memory {
	for i := range rows {
		if rows[i].Content == content {
			return &rows[i]
		}
	}
	return nil
}

// assertValidityStamps checks one returned row's validity triple against the
// fixture's declared claim, so a stamp that did not survive the trip is named.
func assertValidityStamps(t *testing.T, want struct {
	key                               string
	term                              string
	from, until, verified             string
	wantFrom, wantUntil, wantVerified bool
}, m memory.Memory) {
	t.Helper()
	for _, tc := range []struct {
		field string
		got   *string
		want  string
		set   bool
	}{
		{"valid_from", m.ValidFrom, want.from, want.wantFrom},
		{"valid_until", m.ValidUntil, want.until, want.wantUntil},
		{"verified_at", m.VerifiedAt, want.verified, want.wantVerified},
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
// other 545 rows are it.
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
