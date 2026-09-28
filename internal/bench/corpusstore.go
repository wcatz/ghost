package bench

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// A seeded corpus row gets two things from the fixture rather than from the
// clock: its id, and its created_at. Both of them are read by the ranking, and
// both of them used to be drawn per run, which is what made a benchmark
// unreproducible at a grid point where the two legs are weighted equally (#708).
// They live together because the invariant is one sentence — the store a
// benchmark measures is a function of the corpus it seeded — and because a
// seeder that fixed one of them and not the other would be half a fix.
//
// The rows go in through memory.CreateWithIDFromCorpus, not store.Create: an id
// the caller computed, and the corpus rule on verified_at. Those are two separate
// things and the second is not a detail of the first — a dataset's verified_at is
// the dataset's claim, and a `verified` record stamped with the store's clock
// would say the check happened now. The graded corpus carries such a claim
// (testdata/memories.jsonl), so this is not hypothetical.

// corpusID is the store id a seeded corpus row is written under: a pure function
// of the project it belongs to and the key the fixture names it by, and nothing
// else — no clock, no counter, no randomness. That is what makes a benchmark
// reproducible, and it is not a small property (#708).
//
// The store breaks tied fused scores by memory id (fuseCandidatePool), which is
// correct and deliberate: it is what makes production result order stable when
// map iteration and an unstable sort are not. But Seed went through store.Create,
// so every id came from the id column's `hex(randomblob(16))` default and the
// benchmark threw away the one input that tie-break reads. A grid point weighting
// its two legs EQUALLY collides often enough for that to matter: over runs of one
// binary its NDCG@10 took four values and its paired interval crossed zero, while
// the other five grid points were byte-identical every time. So the ids became a
// function of the corpus, and production keeps the order it had.
//
// The readable form is deliberate, and a digest would be the wrong one here: the
// tie-break is a string comparison, so a tie is resolved BY THE DATASET'S OWN KEY
// ORDER — two rows the corpus knows by name, decided by the names it gave them.
// Hashing the key would make that reproducible and unreadable, and a benchmark
// whose tie-break cannot be read off the corpus is one that has to be trusted
// rather than checked. `bench:` on the front keeps a seeded row from looking like
// a user's.
//
// The project is part of the input because a store can hold more than one corpus
// (seedMaintenance puts rows in `_global` beside the project's own), and the key
// is unique only within a dataset — see the duplicate-key refusal in Seed, which
// is what makes the pair unique and therefore what makes a repeat seed a loud
// primary-key error rather than a silently halved corpus.
func corpusID(project, key string) string {
	return fmt.Sprintf("%s%s:%s", corpusIDPrefix, project, key)
}

// corpusIDPrefix marks a row this package seeded. A bench store is thrown away
// with the run, so the prefix buys no lookup by itself; what it buys is that a
// dump of a bench store says which rows are fixture and which are something
// else, and that a corpus id can never collide with a `hex(randomblob(16))` one.
const corpusIDPrefix = "bench:"

// corpusStamp is the instant ONE seeding pass gives every row it writes.
//
// This is the second half of #708 and it was found by measurement, not by
// reading: with the ids already derived from the corpus, two seeds of the same
// corpus still disagreed on the same grid point, on a query where two rows tie
// on the fused score EXACTLY — a keyword rank-1 hit and a vector rank-1 hit at
// equal leg weights, 0.5/62 each. A tie falls to the id, so with fixed ids the
// order should have been settled… and it was, right up to the last stage.
// decayRank re-sorts the window by base × DecayFactor, and the factor is
// `MAX(floor, 1/(1+ageDays/τ))` with a per-category τ. Two rows in DIFFERENT
// categories therefore have different factors, and which is larger depends on
// their ages — at an age of seconds, on whether the two rows' created_at
// timestamps (second resolution) happen to differ:
//
//	f(A) > f(B)  <=>  age(B) > (tau_B/tau_A) · age(A)
//
// With a seed that took under three seconds, a row created one second LATER than
// its tied partner is younger enough to beat it. So the pair's order depended on
// where a wall-clock second boundary happened to fall inside the seed loop: in
// one store `obsidian_one_way` (architecture, τ=45) and `ouroboros_ntp`
// (dependency, τ=30) swapped places between runs of one binary, with identical
// ids and identical leg ranks on both sides.
//
// One stamp for the corpus removes the boundary, because then every candidate
// carries the same age and only the CATEGORY can order a tied pair: same category
// gives equal factors and the id decides, different categories are ordered by τ
// and by their floors, which are properties of the category. The order is then a
// function of the corpus whatever the wall clock says — and the corpus's claim
// that decay is inert on it (docs/benchmarks.md, TestDecayDoesNotPerturbGradedBench)
// stops being approximately true and becomes structurally true.
//
// The stamp is the seeding pass's own instant, not a fixed constant, so the ages
// a fixture asks for stay realistic: ageDays offsets this one instant, exactly as
// backdate offset `datetime('now')`. A constant date would also have worked and
// would have moved the published numbers, because an age of months puts every
// decaying category on its floor and changes what a tied pair does.
type corpusStamp struct {
	at string // SQLite datetime text: "YYYY-MM-DD HH:MM:SS" in UTC
}

// newCorpusStamp takes the instant a seeding pass runs at, truncated to the
// second because that is the resolution the memories.created_at column stores —
// keeping sub-second precision here would only make two rows differ again.
func newCorpusStamp() corpusStamp {
	return corpusStamp{at: time.Now().UTC().Format("2006-01-02 15:04:05")}
}

// apply writes the stamp onto one row, offset by the age the fixture asked for,
// and re-derives updated_at with it so a backdated row does not claim it was
// touched after it was created.
//
// Raw SQL on the bench-owned store, and it has to be: Create always stamps now,
// and a store cannot be asked through its API to write created_at.
func (s corpusStamp) apply(ctx context.Context, db *sql.DB, id string, ageDays int) error {
	offset := fmt.Sprintf("-%d days", ageDays)
	if _, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime(?, ?), updated_at = datetime(?, ?) WHERE id = ?`,
		s.at, offset, s.at, offset, id); err != nil {
		return err
	}
	return nil
}
