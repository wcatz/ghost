package memory

// Retriever contract (see docs/superpowers/specs/2026-09-25-context-assembler-design.md,
// Decision 1). The DTOs live here rather than in internal/assemble because the
// method is implemented by *Store, and a method cannot name a type from a
// package that imports it. The dependency is one-way: internal/assemble imports
// this package, never the reverse.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProjectMode is the storage-level distinction the retrieval SQL requires: a
// project search also admits `_global` rows, an unresolved project admits only
// them, and a cross-project search has no project predicate at all.
type ProjectMode string

const (
	ProjectScoped ProjectMode = "scoped"       // project_id = ? OR '_global'
	GlobalOnly    ProjectMode = "global_only"  // project_id = '_global'
	AllProjects   ProjectMode = "all_projects" // no project predicate
)

// Condition selects which retrieval legs run. It is part of the retriever
// contract rather than a SearchParams field because a nil query vector already
// means the vector leg is not attempted, which is a different statement from
// asking for vector-only retrieval.
type Condition string

const (
	CondHybrid     Condition = "hybrid"      // both legs, fused
	CondFTSOnly    Condition = "fts_only"    // keyword leg alone
	CondVectorOnly Condition = "vector_only" // vector leg alone; requires a query vector
)

// CandidateRequest is one retrieval: what to fetch, from where, and the clock
// every time-dependent decision is made against. Now is required and is used
// end to end — the candidate path never reads the wall clock, so a ranking and
// the trace describing it cannot disagree about what time it was.
type CandidateRequest struct {
	ProjectID string
	Mode      ProjectMode
	Query     string
	QueryVec  []float32
	Scope     map[string]string
	Category  string
	Condition Condition
	// Params is the caller's unresolved fusion parameters. The Store applies
	// its configured vector floor to them here rather than trusting them, so a
	// caller cannot bypass search.min_similarity.
	Params SearchParams
	Now    time.Time
	// AsOf asks for the set as it stood at that instant, and it makes the whole
	// retrieval historical: the rows are the versions memory_history recorded at
	// or before it, the keyword leg matches THAT text, and no vector leg runs at
	// all (see candidatesAsOf). nil is a current read, which is the default and
	// the only shape every caller before #647 could produce.
	//
	// AsOf is authoritative over Now, not a second opinion about it: the assembler
	// binds both to the same instant, and a caller that reached the store directly
	// and set them differently would have two clocks in one retrieval. The store
	// reads the instant from here for everything — the history bound, the decay
	// age and the order it returns — so the ranking and the rows it ranks cannot
	// disagree about which day it is.
	AsOf *time.Time
	// Fetch is the retrieval depth. Limit is the window: the rows that rank
	// into the answer before any predicate is applied. The returned set is
	// wider than that, so a predicate can be evaluated over rows the window
	// would have cut.
	Fetch Fetch
	// Explain asks for the retrieval to record the facts that decided it and
	// carry them back on the CandidateSet — the pre-scope excluded rows, the
	// vector-floor-dropped rows, the per-candidate ranking facts and the
	// resolved fusion knobs — so the assembler can render an explanation that
	// is a projection of THIS ranking rather than a second search.
	//
	// It costs nothing when false, and in that shape it is the whole point:
	// the production path allocates no trace and changes neither its scores
	// nor its order, byte for byte. Recording happens only here, inside the
	// store's own ranking seam, so explain cannot drift from the search that
	// ran — a re-derivation elsewhere would be a second chance to disagree
	// with it, which is the failure this field exists to remove.
	//
	// It is a request, not a knob of SearchParams, because it changes what
	// the store RETURNS rather than how it ranks: the ranking path is
	// identical either way, and the trace's stamps land wherever the same
	// stages already run.
	//
	// Explain cannot describe a historical read. The AsOf path keeps its own
	// row versions and ranks nothing, so there is no ranking to project;
	// validateCandidateRequest refuses the combination. It likewise demands a
	// query — a passive retrieval runs no legs and scores no candidate, so
	// there is nothing to explain.
	Explain bool
	// Passive carries the selection policies for an empty query, one per bucket,
	// and is populated only for passive retrieval. An empty query with NO policy is
	// refused (ErrPassiveUnsupported) rather than served as an empty set, which
	// would read as a store holding nothing; a policy that names no bucket, states
	// no over-fetch, overstates the ceiling or repeats a bucket is refused for the
	// same reason in a different shape — see validatePassivePolicies.
	//
	// It cannot be combined with AsOf, and the reason is that the two select over
	// different things: a passive policy chooses among live rows, a historical read
	// chooses among recorded versions. Dispatch checks AsOf first, so without that
	// refusal a passive+as_of request would be answered by the historical path with
	// its policies discarded and a window its caller never stated.
	Passive []SlicePolicy
}

// Fetch is the retrieval depth. FTSTopK and VectorTopK are the per-leg depths
// (production fetches twice the window from each leg); Limit is the window
// itself, not a trim applied to the result.
type Fetch struct {
	FTSTopK, VectorTopK int
	Limit               int // Candidates returns a wider untrimmed set
}

// SlicePolicy is one passive bucket's selection rules: how wide to fetch, in
// what order, which categories are behavioural, and whether a near-duplicate
// loser is dropped rather than demoted. The buckets differ by policy, not by
// accident, so each carries its own.
type SlicePolicy struct {
	Bucket             string
	Order              string // decay or pinned_importance_updated
	TwoPass            bool
	BehaviorFloor      int
	BehaviorCategories []string
	CategoryWeights    map[string]float64
	CategoryCaps       map[string]int
	OverFetch          int
	DemotionThreshold  float64
	// IncludeGlobal admits `_global` rows into this bucket's read, which is what
	// the query path calls memory.ProjectScoped and what every whole-project
	// listing has always read: `WHERE project_id = ? OR project_id = '_global'`,
	// with ONE cap over the union.
	//
	// It exists because a bucket names one project and two buckets cannot express
	// a union. A project slice capped at 20 plus a `_global` slice capped at 20
	// admits 40 rows where the caller asked for 20, and the caller's `limit`
	// argument says "max memories to return" — so the alternative is not a
	// tidier policy, it is a different and wrong answer to the same question.
	//
	// FALSE by default, and the default is the shipped session-start shape: those
	// two buckets are disjoint on purpose, and a global row arriving through the
	// project bucket would spend the `_global` slice's slots.
	//
	// A request that mixes `_global` into one bucket AND fetches it in another is
	// refused at both seams: the rows would come back twice, which is the same
	// defect the repeated-bucket refusal already names.
	IncludeGlobal bool
	// ItemCap is the rows this bucket will finally admit. The two-pass selection
	// fills a POOL of twice it before the near-duplicate demotion runs, which is
	// the shipped shape: a window wider than the cap has rows to trade when a
	// demoted one drops out, and a window narrower than the cap has nothing to
	// trade, so the demotion is skipped (see DemoteOnlyWhenOverCap). 0 means the
	// cap is the over-fetch itself, i.e. the bucket admits its whole window.
	ItemCap int
	// DemoteOnlyWhenOverCap gates the near-duplicate demotion on the selected set
	// being WIDER than ItemCap. The project bucket sets it, the `_global` bucket
	// does not — and that difference is observable rather than tidiness: the
	// demotion is a REORDER, so on a set that fits entirely under the cap it can
	// only change the order of rows the answer shows in full, and the shipped
	// loaders both skip it there.
	DemoteOnlyWhenOverCap bool
	// ExcludeSeen is reserved for the project-context bucket, which is the one
	// passive policy that must not repeat a row the project bucket already
	// showed. Nothing reads it yet, deliberately: a field that silently did
	// nothing would be worse than an absent one, so it is stated as unread until
	// the surface that needs it lands, and the migration of that surface is
	// where it gets honoured rather than a silent no-op here.
	ExcludeSeen bool
	// DropDemotedLosers asks for a near-duplicate loser to be REMOVED rather than
	// ranked last. It is what the `_global` bucket does and the project bucket
	// does not, and the difference is observable in a rendered block rather than
	// being a policy preference: one bucket spends no slot on a restatement of a
	// row it is already showing.
	DropDemotedLosers bool
}

// DroppedLoser is one near-duplicate loser the retriever removed rather than
// ranked last — from a collapsed query window or a passive bucket — with the
// ids of the rows it lost to (sorted, never empty). The Candidate is the row as
// the retriever scored it, so a consumer reads the same project, score and age
// it would have read had the row been returned.
type DroppedLoser struct {
	Candidate
	LostTo []string
	// StandIns are the ids among LostTo the removal chose over this row only
	// because they are LATER (the query path's chronology rule, earlierRowLoses):
	// by rank this row would have been kept and they removed. Each stands in for
	// it, so a later admission stage judges a stand-in by this row's evidence as
	// well as its own (assemble's standInEvidence). Empty for a rank verdict.
	StandIns []string
}

// CandidateSet is one retrieval's rows plus the facts about how they were
// retrieved. Widened reports that the set is larger than the requested window,
// which is the assembler's evidence that it could filter before closing.
type CandidateSet struct {
	Rows []Candidate
	// DroppedLosers are the near-duplicate losers the retriever REMOVED instead
	// of ranking last (a query window, which collapses its pairs, or a passive
	// bucket whose policy sets DropDemotedLosers), each with the ids of the rows
	// it lost to. They are not in Rows, and the assembler cannot rediscover them:
	// the removal happened over a window whose edges it never saw. Carrying them
	// is what lets stage 6 put each one in the trace and the retrieval record
	// with its reason. Empty on every read in which the window held no
	// near-duplicate edge to act on.
	DroppedLosers []DroppedLoser
	Edges         []LinkEdge
	EdgesStatus   EdgeStatus
	// Legs is keyed by leg name: "fts" and "vector".
	Legs    map[string]LegStatus
	Widened bool
	// Unrecorded counts the in-scope memories a historical read could not place
	// at the requested instant because no version of them is recorded at or
	// before it — every memory predating the history table (schema v17). They
	// are absent from Rows because the store cannot say what they held then, and
	// they are counted here because a shorter set that says nothing about the gap
	// reads as the whole truth. Zero on every current read.
	Unrecorded int
	// ValidityExcluded counts the rows a validity predicate removed before the
	// window closed, so a set that came back empty can say whether it was empty
	// of valid rows or EMPTY of rows. The passive path sets it for a bucket whose
	// fetch returned nothing: the count is capped at PassiveEligibleCountCap, so a
	// value equal to that cap is a LOWER BOUND, matching PassiveEligibleCount.
	// Only whether it is greater than zero is consumed downstream (the
	// all_invalid outcome), so the capped flag from the passive read is not
	// carried here. The query path sets it from the leg that withheld the rows:
	// the keyword leg counts what its SQL predicate removed when it returns
	// nothing (#1053), and the vector scan counts the above-floor matches it
	// withheld before the cut, so a semantic-only or hybrid empty whose every
	// match retired reads `all_invalid` too rather than as absence. All are zero
	// on a read whose predicate admitted everything.
	ValidityExcluded int
	// PinnedBeyond is, per project (the row's own, so a union bucket's `_global` rows are counted under `_global`), how many eligible pinned rows never
	// entered the window: the window is a LIMIT, and a bucket holding more pinned
	// rows than the window has places reads only the first of them. Set only for
	// a bucket whose whole window is pinned (the only case in which a pinned row
	// can be past it); absent otherwise. It is what lets the assembler say how many
	// pinned rows a cap cut when it never saw some of them. The window is the
	// over-fetch the fetch read and not the selected set: the selected set is the
	// window plus the replacements of pinned rows, which are fetched past it so
	// they can hold a slot beside the row they replaced.
	PinnedBeyond map[string]int
	// WindowExtra is, per passive bucket, how many rows the set carries beyond the
	// bucket's over-fetch: the replacements of pinned rows, fetched past the LIMIT
	// so they can hold a slot. A caller deriving how many eligible rows lie past the
	// window must count them as inside it, or it counts them twice.
	WindowExtra map[string]int

	// --- explain-only diagnostics. nil/zero unless the request asked for
	// explain (CandidateRequest.Explain); building them is what that flag
	// buys, and with it false these fields are never touched, so the plain
	// retrieval path allocates none of them. They exist so the assembler can
	// render an explanation that is a PROJECTION of this one ranking —
	// every row below is a row this retrieval examined or decided over, and
	// every fact rides on it rather than being recomputed by a second search.

	// Excluded is the pre-scope fused candidate pool the returned set does NOT
	// carry, in the pool's fused-score order (which Rows is not). It is the
	// rows scope narrowing removed before window selection, plus the eligible
	// rows the window and its tail never reached; together with Rows and
	// FloorDropped it partitions the pool every leg surfaced, so an
	// explanation can account for every candidate the search saw. A row in it
	// is explained by the same trace verdict that excluded it: scope_matched
	// false is the scope complaint, everything else is the result window.
	Excluded []Memory
	// FloorDropped is what the vector similarity floor removed from the
	// VECTOR leg without the keyword leg retrieving the row either — the
	// candidates the floor cut out of the retrieval entirely, in the raw
	// vector-leg order the floor saw them. (A dual-leg row the floor cut from
	// the vector side still reaches fusion on its keyword term and so lives
	// in Rows or Excluded, with its floor_dropped fact on the trace.) It is
	// why "your query matched nothing strongly enough" is reported rather
	// than read as a candidate that never existed. Hydrated, as Rows and
	// Excluded are, on the same snapshot as the rest of the read.
	FloorDropped []Memory
	// RankFacts is the ranking path's own per-candidate record, keyed by
	// memory id, covering every row in Rows, Excluded and FloorDropped. The
	// explain projection reads every scoring fact from it — leg ranks, the
	// pre-status fused base, status factor, scope verdict, reservation
	// exchange, decay, floor and window-scoped penalties — so the numbers it
	// reports are the numbers that ranked, not a re-computation. A missing
	// entry is a row the ranking genuinely never scored.
	RankFacts map[string]*RankFact
	// ExplainKnobs is the resolved fusion configuration this ranking ran
	// under, so the projection can name what the scores mean without
	// re-deriving the floor or the weights.
	ExplainKnobs ExplainKnobs
	// floorDroppedIDs holds the vector-only floor-dropped ids in raw leg
	// order while the retrieval runs; Candidates hydrates them into
	// FloorDropped once the read is settled. Unexported because it is
	// intermediate state, not a fact a caller can act on.
	floorDroppedIDs []string
}

// ExplainKnobs is the resolved fusion configuration one ranking ran under. It is
// carried on the CandidateSet so the explain projection can name what a score
// MEANS without re-deriving it: every fused base is the sum, over the legs that
// retrieved the row, of weight/(RRFK+rank+1), with FTSWeight on the keyword leg
// and VecWeight on the vector leg, and VectorFloor is the cosine the vector leg
// was cut on. They are the values the fusion and the floor READ, copied from the
// parameters the ranking ran with, so a number reported against them is the
// number that decided.
type ExplainKnobs struct {
	RRFK                 int
	FTSWeight, VecWeight float64
	VectorFloor          float64
}

// Candidate is one hydrated row with the scoring facts fusion produced for it.
// The facts are carried rather than re-derived, so a trace cannot report a
// different number from the one that ranked the row.
type Candidate struct {
	Memory
	// FTSRank and VectorRank are 0-based within their leg, and -1 when that
	// leg did not retrieve the row: rank 0 is a real first place, and an
	// absent leg must not look like one.
	FTSRank, VectorRank int
	VectorScore         float64
	// FetchedBy is the passive policy that retrieved this row, and it is empty
	// on every query-mode row.
	//
	// It exists because a row's OWN project is not always the bucket that read
	// it: a policy with IncludeGlobal set fetches `_global` rows under a project
	// bucket, and the caller's cap belongs to the POLICY. Without this field the
	// assembler's per-slice cap cannot be applied to such a row at all — a
	// `_global` row admits no slice under its own name and is therefore unbounded
	// — which turns "at most N rows" into "at most N project rows, plus however
	// many globals happened to be nearby". That is the failure mode this field
	// removes, and it is why the cap is keyed on the retriever's answer rather
	// than re-derived from the row.
	FetchedBy string
	// SupersededBy is, for a passive row, pinned or not, the ids of the rows in
	// the same window that supersede it through a live `supersedes` edge, read
	// with the demotion's own edge rules (supersedePenaltyRows) except that a
	// keep-forever (retention-exempt) target, which the demotion never sinks, is
	// named too: a marker withholds nothing. The assembler marks the row on its line
	// when a named superseder is on the page: a live edge switches the lexical
	// guess (MaybeReplacedBy) off for its pair, and resolve never retires a
	// convention or a preference, so this is the only signal the edge leaves. Only
	// a PINNED, non-exempt row's name moves anything else: a pin guarantees a slot and not a
	// rank above the row that replaced it, so such a row is ordered directly
	// behind its superseder and the superseder is reserved too. On a query read
	// it names the same superseders among the edge scope (querySupersededBy),
	// for the marker only.
	SupersededBy []string
	// MaybeReplacedBy is the ids of the rows in the same set that look like this
	// row's replacement (lexicalReplacements): a newer row of the same project,
	// the same scope and a similar text that names the same thing with a
	// different value, with no
	// live `supersedes` or `contradicts` edge between the two. It is a lexical
	// guess, newest first. On a passive read the assembler also orders those rows
	// ahead of this one (assemble's arrangeReplacements); on a query read it moves
	// them ahead for display only, after the relevance verdict is decided
	// (assemble's displayReplacements). Empty when no row qualifies.
	MaybeReplacedBy []string
	// FloorReserved says the passive two-pass selection's behavioural floor
	// reserved this row a slot. The assembler's replacement arrangement never
	// displaces such a row to make room. Passive reads only.
	FloorReserved bool
	// Base is the fused score the window was cut on, after status demotion.
	// Decay is the category-and-age multiplier, and Score is the product the
	// decay order ranked on. Supersede and near-duplicate demotion is a
	// reordering, not a score, so it is expressed by the returned order.
	Base, Decay, Score float64
	AgeDays            float64
	// KeywordReserved says the keyword reservation admitted this row into the
	// window: its fused score is below the cut by design. The assembler's
	// relevance cutoff (#954) keeps such a row rather than undo the reservation.
	// It is set on query-mode rows only and does not need an explain request.
	KeywordReserved bool
	// Unembedded says the vector leg could not have retrieved this row: it
	// carries no embedding the leg could score — no vector at all, one stamped
	// with a different identity, or one whose width differs from the query's.
	// It is false whenever the vector leg is not in play (no embedder, no query
	// vector, a failed leg), because then no row is judged against that leg. The
	// leg-consensus stage reads it so a row the vector leg could never see is
	// not penalised for lacking vector agreement (#1319): a row saved moments
	// ago has no embedding yet, and its silence must not read as a second leg's
	// dissent.
	Unembedded bool
	// Evidence counts the records supporting this memory, read in the SAME
	// snapshot as the rows. It rides here because the assembler may reach the
	// store through this one read and no other, so a fact only this read can
	// supply has to travel with the row.
	//
	// Nothing ranks on it: stage 4's multiplier stays 1.0 until a measured change
	// justifies one (#673), and a row with no evidence is not demoted for that. It
	// is carried so the trace can report what supports a memory, which is a
	// question a reader of a search result has and no other field answers.
	Evidence EvidenceCounts
	// Usefulness is #648's negative evidence about this memory: what the
	// retrieval audit recorded against it. It rides here for the same reason
	// Evidence does — the assembler may reach the store through this one read —
	// and a memory with repeated contradicted verdicts may be demoted on it.
	Usefulness UsefulnessEvidence
}

// LinkEdge is one edge between two candidates. Stages 5 and 6 read it to
// reorder and record conflicts; the assembler never queries for it itself.
type LinkEdge struct {
	From, To, Relation string
	Strength           float64
}

// LegStatus is what one retrieval leg did. CoverageComplete is the reason an
// empty answer may claim absence: a leg that skipped unembedded or
// dimension-mismatched rows, or that returned exactly as many rows as it was
// asked for, cannot support that claim.
type LegStatus struct {
	Attempted, Available bool
	Err                  string
	Truncated            bool
	Applicable           bool
	// Eligible counts what the leg returned. DimMismatch counts the rows the
	// vector leg could not compare against the query vector, whether the width
	// differs or the vector belongs to another recorded space — free, because
	// the scan already counted them.
	//
	// Expected, Indexed, Unembedded and CoverageComplete are the coverage
	// reconciliation, and they are zero and false in v1 on purpose. Filling them
	// means two COUNT(*) scans over the project on every vector search, for a
	// field nothing reads until the abstention work may claim absence; a row
	// with no embedding is invisible to the leg's own scan, so the counts cannot
	// be derived from what it already read. A leg that cannot support a coverage
	// claim must not make one, which is why the field is false rather than
	// optimistically true.
	Expected, Indexed, Eligible, DimMismatch, Unembedded int
	CoverageComplete                                     bool
}

// EdgeStatus is the edge lookup's own outcome, kept separate from the rows so a
// failed lookup can never read as "no edges exist": "ok" (the query ran, and
// may have returned none), "unavailable" (a successful query returned no
// edges) or "err" (the query failed).
type EdgeStatus struct {
	Status, Err string
	// Chunks is how many queries the edge read took. The read is chunked, so a
	// pair whose endpoints land in different chunks is never read, and a caller
	// whose window exceeds the chunk therefore gets a partial picture. A whole-set
	// claim ("no link joins two of these candidates") is only true at one chunk, so
	// the count travels with the status rather than staying a detail of the loop.
	// Zero means no query ran, which is the same thing: with fewer than two
	// candidates no pair can exist.
	Chunks int
}

// edge statuses, as reported in EdgeStatus.Status.
const (
	edgesOK          = "ok"
	edgesUnavailable = "unavailable"
	edgesErr         = "err"
	// edgesNotApplicable is a retrieval that does not read the link graph at all,
	// as opposed to one that read it and found nothing. A historical read is the
	// case: memory_links records when an edge was invalidated, never what the
	// graph looked like at an instant, so the present graph would be an import of
	// the present into a past answer. It is a status rather than a reused one
	// because "no edge joins these candidates" is a claim, and a retrieval that
	// made no such claim must not render it — the conflict stage reads every
	// status but "err" as grounds to say something.
	edgesNotApplicable = "not_applicable"
)

// ErrPassiveUnsupported is returned for an empty-query request. Passive
// retrieval is specification, not absence: the session-start policies land with
// the migration that moves that surface onto this seam, and until then an
// honest error beats an empty result set.
var ErrPassiveUnsupported = errors.New("passive retrieval is not served by Candidates yet")

// edgeChunkIDs bounds the placeholder list in one edge query. Both endpoints of
// an edge are named, so a candidate set of n ids needs 2n placeholders, and
// SQLite caps a statement's variables well below what a wide window would need.
const edgeChunkIDs = 200

// Candidates runs retrieval for one request and returns a wider, untrimmed set
// with the scoring facts, leg statuses and edges the assembler needs.
//
// What "untrimmed" means here is deliberate. The window is selected exactly as
// production search selects it — fusion, status demotion, the keyword
// reservation, hydration with its deleted-row backfill, decay over the window,
// then the supersede and near-duplicate demotions over the window — and the
// rows that selection discarded are returned after it, in the same decay order.
// So with no predicate, closing the set to Fetch.Limit reproduces the
// production search exactly, and a predicate can still reach rows the window
// cut. Re-deriving the window's own decisions from the wider pool instead
// would drop the reservation and change the demotions' scope, so it is not done.
//
// Everything runs in one read transaction on the store's read handle: the legs,
// hydration, the edge load and the penalty lookups have to describe the same
// database state, and a concurrent save must not be able to delete a row
// between the leg that selected it and the read that hydrates it.
func (s *Store) Candidates(ctx context.Context, req CandidateRequest) (*CandidateSet, error) {
	if err := validateCandidateRequest(req); err != nil {
		return nil, err
	}
	if s.readDB == nil {
		s.warnNoReadHandle()
	}
	handle := s.readHandle()
	if handle == nil {
		return nil, fmt.Errorf("candidates: store has no database handle")
	}

	// Read the in-memory knobs under the lock BEFORE opening the snapshot
	// transaction. A store with no read handle opens that transaction on the
	// primary pool, which is pinned to one connection, and a writer holds the
	// store mutex while it waits for that connection. Taking the lock after
	// the transaction would let the reader hold the connection while waiting
	// for the writer's lock — the classic inversion. Reading here and
	// releasing first removes the overlap.
	s.mu.RLock()
	demotion := s.demotionThreshold
	floor := s.vectorMinSimilarity
	s.mu.RUnlock()

	p := req.Params
	if p.RRFK == 0 && p.FTSWeight == 0 && p.VecWeight == 0 {
		// A caller that passes no parameters gets production's, resolved here
		// rather than at the call site so the floor below is the only one that
		// can apply.
		p = DefaultSearchParams()
	}
	p.ProjectID = req.ProjectID
	p.MinSimilarity = floor
	p.Scope = req.Scope

	ftsTopK := req.Fetch.FTSTopK
	vecTopK := req.Fetch.VectorTopK
	if ftsTopK <= 0 {
		ftsTopK = req.Fetch.Limit * 2
	}
	if vecTopK <= 0 {
		vecTopK = req.Fetch.Limit * 2
	}

	tx, err := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("candidates: begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	identity := s.embeddingIdentityValue()
	// A store bound to the transaction: every read below has to come from the
	// snapshot, and the primary handle's single connection is held by the
	// transaction itself.
	//
	// Every knob that steers retrieval is carried across, or this store would
	// run a different search than the one it is standing in for: without the
	// embedding identity the vector leg here scores the foreign-space vectors
	// the real search excludes, and without the floor or the demotion threshold
	// the rows it returns are ones no caller could reproduce. The foreign-vector
	// warning gate crosses for the mirror-image reason: it is process state
	// whose whole point is that a re-embed is reported once, and a store with a
	// fresh gate would repeat the line on every search that skips those rows.
	cand := &Store{
		db:                  s.db,
		snapshot:            tx,
		logger:              s.logger,
		demotionThreshold:   demotion,
		vectorMinSimilarity: floor,
		foreignWarned:       s.foreignWarned,
		// The corpus scratch crosses for the same reason: this store runs the
		// real vector leg, and a pool of its own would be a pool of one that is
		// garbage the moment this retrieval returns — a corpus-sized allocation
		// on every search, which is what the pool exists to avoid.
		scratch: s.scratch,
		// And the corpus cache, which Candidates may use only because it reads
		// the epoch inside this transaction and takes a hit only on a match.
		vcache: s.vcache,
	}
	cand.embeddingIdentity.Store(identityPtr(identity))

	set := &CandidateSet{Legs: map[string]LegStatus{}}
	if req.AsOf != nil {
		// The whole retrieval is historical, so it does not share a single step
		// with the current path: the rows are versions, the keyword leg matches
		// their text, the vector leg cannot run and the link graph is not read.
		// It is a branch rather than a parameter because every one of those is a
		// different query, not a different value in the same one.
		return cand.candidatesAsOf(ctx, req, p, ftsTopK, set)
	}
	if req.Query == "" {
		// A passive retrieval runs no leg, so the statuses keep their zero
		// values: not applicable, not attempted, not available. That is the honest
		// report, and the abstention floor reads it — a leg that never ran must
		// not leave an arm holding a value it never compared.
		return cand.candidatesPassive(ctx, req, set)
	}
	// The trace is created before the legs run, so every stamp the
	// ranking stages write lands in it: the leg ranks in fusion, the scope
	// verdict in narrowing (for the DROPPED rows too), the reservation
	// exchange in window selection, the factor and clock in decay, the floor
	// verdict in the legs, and the penalty counts with their counterparts in
	// both demotions. Only an explain request pays for it; a nil trace costs
	// the stages one nil check and allocates nothing, which is what keeps the
	// production path byte-identical with explain off.
	if req.Explain {
		p.trace = newSearchTrace()
	}

	fts, vec, err := cand.runCandidateLegs(ctx, req, p, ftsTopK, vecTopK, set)
	if err != nil {
		return nil, err
	}
	// A leg the condition made applicable but the request could not run (a
	// hybrid search with no query vector) is a skip, not a failure, so it does
	// not count towards "every leg failed".
	if err := set.totalLegFailure(); err != nil {
		return nil, err
	}

	fused := fuseCandidatePool(fts.rows, vec.rows, p)
	var preScope []string
	if req.Explain {
		// The pool's fused-score order, captured BEFORE scope narrowing
		// removes the out-of-scope rows: the pre-scope exclusions are reported
		// in this order because it is the order this ranking actually fused,
		// not the order the survivors came back in.
		preScope = poolIDsOf(fused)
	}
	pool := scopeEligiblePool(fused, p)
	if len(pool) == 0 {
		set.EdgesStatus = EdgeStatus{Status: edgesUnavailable}
		if req.Explain {
			if err := cand.fillExplain(ctx, set, req, p, preScope); err != nil {
				return nil, err
			}
		}
		return set, nil
	}

	scores := make(map[string]float64, len(pool))
	for _, c := range pool {
		scores[c.id] = c.score
	}

	window := selectWindow(pool, req.Fetch.Limit, p)
	// The same seam fuseAndRank calls, so a test can fail the hydration read at
	// the same boundary: a read failure has to surface as an error, not as an
	// empty window. (It cannot model a delete here the way it does there — this
	// path reads inside one snapshot, so a concurrent delete is simply not
	// observable.)
	beforeHybridHydrate(window.IDs)
	hydrated, err := hydrateWindow(ctx, cand, window, pool)
	if err != nil {
		return nil, err
	}
	selected := selectHydratedWindow(hydrated, window, poolIDsOf(pool))
	selected = decayRank(selected, scores, p, req.Fetch.Limit, req.Now)
	selected = cand.demoteSuperseded(ctx, selected, p)
	// The near-duplicate verdicts are read once more, by the removal itself: the
	// loser leaves the set here rather than being ranked last, and it reports
	// what it removed so the assembler can file the verdict (#926).
	kept, removed, lostTo, standIns := cand.dropNearDuplicates(ctx, selected, p, req.Now)
	selected = kept
	droppedIDs := make([]string, len(removed))
	for i, m := range removed {
		droppedIDs[i] = m.ID
	}

	tail, err := cand.hydrateTail(ctx, pool, selected, droppedIDs, scores, req.Fetch.Limit, req.Now)
	if err != nil {
		return nil, err
	}

	// The tail is the pool's next rows, which the window removal did not see:
	// a near-duplicate cluster whose members straddle the window and the pool
	// would otherwise return two rows of one group (window [A], tail [C] of a
	// cluster the window removed B from). The removal runs once more over the
	// combined rows, window first, so every pair spans the two halves in rank
	// order and the trace says which row each loss went to.
	//
	// The survivors are partitioned by MEMBERSHIP, never by index: position
	// picks the loser, but the demotion rules FLIP that decision when the
	// positioned loser is pinned or retention-exempt and the winner is not
	// (demoteResults' protected rule), and the removal flips it again when the
	// positioned loser is the later row (earlierRowLoses), so a protected or a
	// later tail row can beat the window's own representative. A slice at len(selected) would then keep the
	// loser in Rows while filing it as dropped, and cut the real winner out of
	// the answer with no verdict anywhere — both the row-accounting loss and
	// the contradiction DroppedLosers exists to prevent. Each half keeps its
	// own order either way, so the representative's rank, score and position
	// are untouched by this pass.
	if len(tail) > 0 {
		combined := append(append([]Memory{}, selected...), tail...)
		keptAll, passLosers, lostToPass, standInsPass := cand.dropNearDuplicatesOver(ctx, combined, len(selected), p, req.Now)
		if len(passLosers) > 0 {
			inWindow := make(map[string]bool, len(selected))
			for _, m := range selected {
				inWindow[m.ID] = true
			}
			keptWindow := make([]Memory, 0, len(selected))
			keptTail := make([]Memory, 0, len(tail))
			for _, m := range keptAll {
				if inWindow[m.ID] {
					keptWindow = append(keptWindow, m)
				} else {
					keptTail = append(keptTail, m)
				}
			}
			selected, tail = keptWindow, keptTail
			for _, m := range passLosers {
				removed = append(removed, m)
				droppedIDs = append(droppedIDs, m.ID)
				lostTo[m.ID] = lostToPass[m.ID]
				if ids := standInsPass[m.ID]; len(ids) > 0 {
					if standIns == nil {
						standIns = make(map[string][]string)
					}
					standIns[m.ID] = ids
				}
			}
		}
	}

	rows := make([]Candidate, 0, len(selected)+len(tail))
	for _, m := range append(selected, tail...) {
		c := candidateOf(m, scores, fts, vec, req.Now)
		c.KeywordReserved = window.Reserved[m.ID]
		rows = append(rows, c)
	}
	set.Rows = rows
	set.Widened = len(rows) > req.Fetch.Limit

	// The removed losers, each with the ids it lost to: not in Rows, and the
	// assembler cannot rediscover them — the edge verdict that removed them was
	// read over a window it never saw. Filing them here, before the explain
	// buckets are built, is what lets stage 6 put each one in the trace, the
	// retrieval record and explain with its reason.
	//
	// A loser carries the same admission facts a kept row does (the keyword
	// reservation here, the embedding state below), because a stand-in for it is
	// judged by its evidence (DroppedLoser.StandIns).
	for _, m := range removed {
		c := candidateOf(m, scores, fts, vec, req.Now)
		c.KeywordReserved = window.Reserved[m.ID]
		set.DroppedLosers = append(set.DroppedLosers, DroppedLoser{
			Candidate: c,
			LostTo:    lostTo[m.ID],
			StandIns:  standIns[m.ID],
		})
	}

	// The explain fields are filled next, from the same snapshot the ranking
	// read: the excluded and floor-dropped rows are hydrated here, and the
	// ranking facts were already stamped by the stages and are carried over
	// without re-derivation. Distinct from the edge and evidence reads below,
	// which describe the returned rows; the explain buckets are not part of
	// them because no caller decides or reports on them past the projection.
	if req.Explain {
		if err := cand.fillExplain(ctx, set, req, p, preScope); err != nil {
			return nil, err
		}
	}

	// One id scope for the two reads that need one: the edge load and the evidence
	// counts ask about the same rows, and edgeScopeIDs is the statement of which
	// rows those are (the returned set, bounded by twice the window).
	scope := edgeScopeIDs(rows, req.Fetch.Limit)
	edges, status := cand.loadCandidateEdges(ctx, scope)
	set.Edges, set.EdgesStatus = edges, status

	// The replacements no lifecycle has related yet, NAMED and not reordered: the
	// order of a query answer is what its relevance verdict is decided on (the
	// cutoff reads the top row, the leg-consensus stage each leg's first place),
	// so the retriever must not rearrange it; the assembler moves a replacement
	// ahead for display only after that verdict. Asked over the rows
	// the edges were read for, and not at all when that read failed, so an
	// explicit relation always gets to overrule it.
	if status.Status != edgesErr {
		scoped := rows[:len(scope)]
		if repl := lexicalReplacements(scoped, edges, req.Now); len(repl) > 0 {
			for i := range scoped {
				scoped[i].MaybeReplacedBy = repl[scoped[i].ID]
			}
		}
	}
	// The pairs a live `supersedes` edge has switched the guess off for, named so
	// the line still says the row was replaced (querySupersededBy). Marker only.
	if named := cand.querySupersededBy(ctx, rows[:len(scope)]); len(named) > 0 {
		for i := range scope {
			rows[i].SupersededBy = named[rows[i].ID]
		}
	}

	// The evidence counts, read on the same snapshot as everything above it: a
	// count taken after the transaction closed could describe a save the rows in
	// this set predate, and the trace would then report support for a state of
	// the corpus it did not retrieve.
	//
	// A failure here is returned rather than degraded into zeroes. The counts are
	// inert for ranking, so a caller would not notice a silent zero — but a trace
	// that says "no recorded evidence" because the read failed is a false claim
	// about a memory, which is the one thing this read must never produce.
	counts, err := evidenceCountsFor(ctx, cand.queryDB(), scope)
	if err != nil {
		return nil, fmt.Errorf("candidates: evidence counts: %w", err)
	}
	for i := range rows {
		rows[i].Evidence = counts[rows[i].ID]
	}

	// #648: the audit's negative evidence rides with the row, the same way the
	// evidence counts do, so the assembler can demote a memory the audit has
	// contradicted repeatedly without a second read. It is read per project,
	// because UsefulnessByMemory is project-scoped; a row whose project has no
	// negative evidence simply carries the zero value.
	usefulness := map[string]UsefulnessEvidence{}
	projects := map[string]struct{}{}
	for i := range rows {
		projects[rows[i].ProjectID] = struct{}{}
	}
	for proj := range projects {
		if strings.TrimSpace(proj) == "" {
			continue
		}
		ev, uErr := s.usefulnessByMemory(ctx, tx, proj)
		if uErr != nil {
			if isMissingTable(uErr) {
				continue
			}
			return nil, fmt.Errorf("candidates: usefulness evidence: %w", uErr)
		}
		for id, v := range ev {
			usefulness[id] = v
		}
	}
	for i := range rows {
		rows[i].Usefulness = usefulness[rows[i].ID]
	}

	// A row the vector leg could never have retrieved — no embedding it could
	// score: no vector at all, one from another identity, or one of another
	// width — must not read as a row that leg ranked low (#1319). The read is
	// taken only when the vector leg actually ran and answered, because only
	// then does the leg-consensus gate ask the question: with the leg skipped
	// or failed the gate is a pass-through and the field is never read. rows
	// shares its backing array with set.Rows, so the marks below reach the
	// returned set.
	if vecStatus := set.Legs["vector"]; vecStatus.Applicable && vecStatus.Available {
		judged := rows
		if len(set.DroppedLosers) > 0 {
			judged = make([]Candidate, 0, len(rows)+len(set.DroppedLosers))
			judged = append(judged, rows...)
			for _, l := range set.DroppedLosers {
				judged = append(judged, l.Candidate)
			}
		}
		unembedded, err := unembeddedAmong(ctx, cand.queryDB(), judged, identity, len(req.QueryVec))
		if err != nil {
			return nil, fmt.Errorf("candidates: unembedded rows: %w", err)
		}
		for i := range rows {
			rows[i].Unembedded = unembedded[rows[i].ID]
		}
		for i := range set.DroppedLosers {
			set.DroppedLosers[i].Unembedded = unembedded[set.DroppedLosers[i].ID]
		}
	}
	return set, nil
}

// unembeddedAmong returns the candidates that carry no embedding the vector leg
// could score: no vector at all, one stamped with a different identity, or one
// whose width differs from the query's. The three cases are the two filters the
// scan applies, and they are applied in its order — identity first, then width —
// because a row the leg saw is a row the leg ranked, and everything else is one
// it could never have considered.
//
// An empty identity means "any recorded vector counts", which is the state of a
// store with no embedding model configured: the identity filter is dropped, but
// a row with no vector is still invisible to the leg and is still reported.
//
// width is the query's own float32 count, the same value the scan compares a
// stored blob's length against. It is only reached with a width, because the
// caller's guard requires the vector leg to have answered, which requires a
// query vector to embed.
//
// It is the rule UnembeddedMemoryIDs applies, narrowed to one retrieval's rows
// and read from the same snapshot as those rows. Chunked like the edge and
// evidence reads, because SQLite caps a statement's bound parameters.
func unembeddedAmong(ctx context.Context, db Queryer, candidates []Candidate, identity string, width int) (map[string]bool, error) {
	out := make(map[string]bool, len(candidates))
	if len(candidates) == 0 {
		return out, nil
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	head := `SELECT m.id FROM memories m LEFT JOIN memory_embeddings e ON e.memory_id = m.id`
	var prefix []any
	if identity != "" {
		head += ` AND e.model = ?`
		prefix = append(prefix, identity)
	}
	if width > 0 {
		// The scan's own division: a blob is len/4 float32s, and a blob that is
		// not a multiple of four is compared at its floor.
		head += ` AND length(e.embedding)/4 = ?`
		prefix = append(prefix, width)
	}
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(prefix)+len(chunk))
		args = append(args, prefix...)
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, head+
			` WHERE m.id IN (`+strings.Join(ph, ",")+`) AND e.memory_id IS NULL`, args...)
		if err != nil {
			return nil, fmt.Errorf("read unembedded rows: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("scan unembedded rows: %w", err)
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("iterate unembedded rows: %w", err)
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
}

// totalLegFailure reports an error when every leg that actually ran failed.
//
// The distinction is the whole point. One leg failing while another completes is
// a partial retrieval: the zero set goes back with both statuses, and the caller
// says the search was incomplete. Every applicable leg failing is not a result
// at all — there is nothing to report — and an empty set for it would let a
// dropped keyword index read as "this store holds no memory matching that".
func (c *CandidateSet) totalLegFailure() error {
	attempted, failed := 0, 0
	var causes []string
	for _, name := range []string{"fts", "vector"} {
		leg := c.Legs[name]
		if !leg.Attempted {
			continue
		}
		attempted++
		if !leg.Available {
			failed++
			causes = append(causes, name+" leg: "+leg.Err)
		}
	}
	if attempted == 0 || failed < attempted {
		return nil
	}
	return fmt.Errorf("candidates: every applicable retrieval leg failed (%s)", strings.Join(causes, "; "))
}

// validateCandidateRequest rejects a request the store cannot serve honestly,
// before any query runs. It is the store's own half of the contract: Run
// validates the same conditions, and Candidates is reachable directly by the
// bench harness and by tests.
func validateCandidateRequest(req CandidateRequest) error {
	if req.Now.IsZero() {
		return errors.New("candidates: Now is required: the candidate path does not read the wall clock")
	}
	switch req.Mode {
	case ProjectScoped, GlobalOnly, AllProjects:
	default:
		return fmt.Errorf("candidates: unknown project mode %q", req.Mode)
	}
	switch req.Condition {
	case CondHybrid, CondFTSOnly, CondVectorOnly:
	default:
		return fmt.Errorf("candidates: unknown condition %q", req.Condition)
	}
	if req.Condition == CondVectorOnly && len(req.QueryVec) == 0 {
		return errors.New("candidates: vector-only retrieval requires a query vector")
	}
	if req.AsOf != nil {
		// The AsOf checks come BEFORE the passive branch on purpose. Candidates is
		// reachable directly — the bench harness and tests call it without going
		// through assemble.Run — so a guard that only the assembler's copy of the
		// contract applies is a guard half the callers miss. Placing them here is
		// what keeps a zero instant from being answered with the empty set that
		// reads as "nothing existed at year 1".
		if req.AsOf.IsZero() {
			return errors.New("candidates: AsOf is required to name an instant; the zero time is not one")
		}
		if req.Explain {
			// Refused rather than served with the explain fields silently left
			// empty. An explanation is a projection of the CURRENT ranking: the
			// trace records what the ranking did, and a historical read ranks no
			// current candidate at all — it selects among recorded versions the
			// scoring path never saw. The assembler refuses the combination
			// before reaching the store; this is the defence in depth for the
			// callers that reach Candidates directly.
			return errors.New("candidates: explain is a projection of the current ranking and cannot describe a " +
				"historical (as_of) read, whose versions were never ranked — ask for one or the other")
		}
		if req.Query == "" {
			// Refused rather than routed to the historical path, which would
			// silently DISCARD the passive policies and answer the question with a
			// different window (Fetch.Limit) while the caller believed its
			// per-bucket over-fetches were in force. A historical read is keyword
			// retrieval over recorded versions, and a passive one is a selection
			// policy over live rows; there is no combination of the two here yet,
			// and answering one as the other would report a block neither policy
			// describes.
			return errors.New("candidates: a passive request cannot also be a historical (as_of) read: the passive " +
				"policies select over live rows and a historical read selects over recorded versions — use one or the other")
		}
		if req.Condition == CondVectorOnly {
			// Refused rather than downgraded. An embedding records the text a
			// memory holds now, so a vector leg over a historical content set has
			// nothing to compare: the vectors for the versions the read chooses
			// between were never computed. Answering with the keyword leg instead
			// would hand a caller rows it cannot tell came from a leg it did not
			// ask for.
			return errors.New("candidates: vector-only retrieval is not available for a historical (as_of) read: " +
				"embeddings record current content only, so a past content set has no vectors — ask for hybrid or fts_only")
		}
	}
	if len(req.Passive) > 0 && req.Query != "" {
		// The mirror of the AsOf refusal above, on the sibling axis, and refused
		// for the same reason. Dispatch sends a request with a query down the FUSION
		// path, which never reads Passive — so the per-bucket over-fetches would be
		// discarded and the window would be Fetch.Limit, a number this caller never
		// stated. A caller that believes its policies are in force must not be
		// answered without them.
		return errors.New("candidates: a request carrying passive policies must have an empty query: a query is answered by the " +
			"fusion path, which reads no policy and sizes its own window — a passive request is the shape that uses them")
	}
	if req.Query == "" {
		if req.Explain {
			// A passive retrieval runs no legs and scores no candidate, so an
			// explanation of its ranking has nothing to explain. Refused rather
			// than silently served with the explain fields empty: serving it
			// would let a caller believe the returned rows carry ranking facts
			// that the retrieval never computed.
			return errors.New("candidates: explain requires a query: a passive retrieval scores no candidate, so there is no ranking to explain")
		}
		// A passive request is sized by its policies rather than by Fetch.Limit,
		// so the window check does not apply to it. What does apply is the
		// per-policy bound: a passive fetch runs at every session start, and a
		// policy with no over-fetch is a store scan.
		return validatePassivePolicies(req.Passive)
	}
	if req.Fetch.Limit <= 0 {
		return fmt.Errorf("candidates: fetch limit must be positive, got %d", req.Fetch.Limit)
	}
	return nil
}

// ftsLeg and vecLeg are the two legs' output plus the per-leg rank and score
// indexes the candidates need, so a fact about a row does not cost a second
// lookup.
type ftsLeg struct {
	rows  []Memory
	index map[string]candidateLegFact
}

type vecLeg struct {
	rows  []ScoredMemory
	index map[string]candidateLegFact
}

type candidateLegFact struct {
	rank  int
	score float64
}

// runCandidateLegs executes the legs the condition selects, recording each
// leg's status. A leg that fails is recorded, not returned: a keyword leg that
// errored while the vector leg returned rows is a partial retrieval, and the
// assembler reports that rather than calling it an empty store.
//
// The error return is the keyword leg's validity-exclusion count, which is read
// on the same snapshot as the legs. It is not folded into the leg's own status:
// a failed count would otherwise mark the keyword leg unavailable, which tells a
// caller a search it cannot reproduce failed.
func (s *Store) runCandidateLegs(ctx context.Context, req CandidateRequest, p SearchParams, ftsTopK, vecTopK int, set *CandidateSet) (ftsLeg, vecLeg, error) {
	project := req.ProjectID
	if req.Mode == GlobalOnly {
		// The legs filter on `project_id = ? OR project_id = '_global'`, so a
		// global-only request names _global explicitly instead of relying on an
		// empty id matching nothing: every mode then issues the same statement,
		// and no row can be pulled in by an empty project id.
		project = "_global"
	}
	var out ftsLeg
	ftsStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondFTSOnly}
	if ftsStatus.Applicable {
		var rows []Memory
		var err error
		if req.Mode == AllProjects {
			rows, err = s.searchFTSValidating(ctx, "", req.Query, ftsTopK, req.Now, true)
		} else {
			rows, err = s.searchFTSValidating(ctx, project, req.Query, ftsTopK, req.Now, false)
		}
		ftsStatus.Attempted = true
		ftsStatus.Available = err == nil
		ftsStatus.Eligible = len(rows)
		ftsStatus.Truncated = err == nil && len(rows) >= ftsTopK
		if err != nil {
			ftsStatus.Err = err.Error()
		} else {
			out.index = make(map[string]candidateLegFact, len(rows))
			for i, m := range rows {
				// The first hit wins a repeated id, matching how explain
				// indexes the same leg.
				if _, seen := out.index[m.ID]; !seen {
					out.index[m.ID] = candidateLegFact{rank: i}
				}
			}
			out.rows = rows
		}
		// A keyword leg sees every row in scope, so it is complete coverage
		// exactly when it ran and was not cut short.
		ftsStatus.CoverageComplete = ftsStatus.Available && !ftsStatus.Truncated
	}
	set.Legs["fts"] = ftsStatus

	// #1053 moved the window predicate into the keyword leg's SQL, so a query
	// whose every match is outside its window comes back with no candidate and no
	// stage having run. The count is taken ONLY then, and it is what keeps the
	// empty reason honest: rows were found and withheld as out of date
	// (`all_invalid`), rather than nothing matching (`no_candidates`). The query
	// path's counterpart to passiveValidityExcluded, gated on emptiness for the
	// same reason that probe is.
	if ftsStatus.Applicable && ftsStatus.Available && len(out.rows) == 0 {
		n, err := s.keywordValidityExcluded(ctx, req, project, req.Mode == AllProjects)
		if err != nil {
			return out, vecLeg{}, err
		}
		set.ValidityExcluded += n
	}

	var vec vecLeg
	vecStatus := LegStatus{Applicable: req.Condition == CondHybrid || req.Condition == CondVectorOnly}
	if vecStatus.Applicable && len(req.QueryVec) > 0 {
		vecStatus.Attempted = true
		var facts vectorLegFacts
		var raw []ScoredMemory
		var err error
		if req.Mode == AllProjects {
			raw, facts, err = s.searchVectorAllLegAt(ctx, req.QueryVec, vecTopK, req.Now)
		} else {
			raw, facts, err = s.searchVectorLegAt(ctx, project, req.QueryVec, vecTopK, nil, req.Now)
		}
		vecStatus.Available = err == nil
		if err != nil {
			vecStatus.Err = err.Error()
		} else {
			kept := filterVectorFloor(raw, p.MinSimilarity)
			vecStatus.Truncated = len(raw) >= vecTopK
			// DimMismatch counts the rows the leg could not compare against the
			// query vector: a different width, or a recorded identity from
			// another vector space (a model or task-prefix change). Both leave
			// the leg silent about that row. It is free — the scan already
			// counted them — so it is reported, and it is what tells a leg
			// that skipped every row from one that scored some.
			vecStatus.DimMismatch = facts.mismatched + facts.foreign
			// A scan that read rows and skipped every one computed no cosine:
			// the leg did not run, whatever the store held. It is reported as
			// never attempted, which the outcome line prints as `not_run`, so
			// a model or width change that has not been re-embedded yet reads
			// as the keyword-only search it is rather than as `vector:ok`.
			// DimMismatch keeps the count. A store with no vector at all
			// skipped nothing and stays attempted, as before.
			if facts.usable == 0 && vecStatus.DimMismatch > 0 {
				vecStatus.Attempted = false
				vecStatus.Available = false
			}
			vecStatus.Eligible = len(kept)
			// #1053 moved the window predicate into the vector scan too, so a
			// semantic-only (or hybrid) query whose every match above the
			// similarity floor is out of its window comes back with no candidate
			// and no stage having run. The scan counted those rows as it scored
			// them, so the count is free and exact; it is added to the set only
			// when the leg itself came back empty, mirroring the keyword leg's
			// probe, so it cannot describe a non-empty leg as withheld. A vector
			// match is a cosine rather than a term, so there is no separate
			// statement that could count what the leg would have retrieved — the
			// count lives in the scan because that is the only place the
			// similarity is known.
			if len(raw) == 0 {
				set.ValidityExcluded += facts.validityExcluded
			}
			// CoverageComplete stays false on purpose, and the reason is a cost
			// decision rather than an oversight. Reconciling it needs the rows
			// the leg could have seen against the rows that carry a vector —
			// two COUNT(*) scans over the project on every hybrid search, on
			// the live tool path, for a field no consumer in the tree reads yet.
			// What is missing is not derivable from the scan: a row with no
			// embedding is invisible to it, so "every row I looked at was
			// usable" says nothing about the rows I never saw. A leg that cannot
			// support a coverage claim must not make one, which is why this
			// reads false while the counts read zero. The reconciliation lands
			// with the abstention work, the first thing that may claim absence
			// — at which point the scans are worth their two COUNT(*).
			vecStatus.CoverageComplete = false
			vec.index = make(map[string]candidateLegFact, len(kept))
			for i, sm := range kept {
				vec.index[sm.MemoryID] = candidateLegFact{rank: i, score: float64(sm.Score)}
			}
			vec.rows = kept
			if req.Explain {
				// The floor's per-row verdict lands in the trace here, from the
				// one helper both ranking paths share, so explain reads the
				// floor the ranking applied rather than re-applying it. The
				// vector-only drops are also collected for the explain payload
				// in raw leg order: a candidate the floor cut from the vector
				// side that the keyword leg never retrieved is out of the
				// retrieval entirely, and that is a distinct, diagnosable
				// outcome the survivor list would otherwise hide — "your query
				// matched no memory strongly enough" must not read as "no such
				// candidate existed".
				stampVectorFloor(p.trace, raw, kept, p.ProjectID)
				if len(kept) < len(raw) {
					keptSet := make(map[string]bool, len(kept))
					for _, v := range kept {
						keptSet[v.MemoryID] = true
					}
					for _, v := range raw {
						if keptSet[v.MemoryID] {
							continue
						}
						if _, inFTS := out.index[v.MemoryID]; inFTS {
							continue
						}
						set.floorDroppedIDs = append(set.floorDroppedIDs, v.MemoryID)
					}
				}
			}
		}
	}
	set.Legs["vector"] = vecStatus
	return out, vec, nil
}

// keywordValidityExcluded counts the rows that matched the query but lie outside
// their validity window — the rows the keyword leg's SQL predicate removed
// before its LIMIT (#1053).
//
// It is the query path's counterpart to passiveValidityExcluded, and for the same
// reason: the predicate now runs in the leg's own statement, so a set that came
// back empty carries NO stage verdict, and "the window came back empty"
// (no_candidates) and "rows were found and withheld as out of date" (all_invalid)
// are different facts a caller acts on differently — the tool's reason vocabulary
// allows only no_candidates to claim the store holds nothing matching.
//
// This counts the KEYWORD leg's population. The vector leg answers the same
// question from its own scan — a vector match is a cosine rather than a term, so
// there is no cheap statement that counts the rows it would have retrieved, and
// the only place the similarity is known is the scoring loop, which counts the
// above-floor matches it withheld (vectorLegFacts.validityExcluded). The two
// counts are summed by the query path when their legs come back empty, so a
// semantic-only or hybrid query whose every match retired is `all_invalid` too,
// not an absence.
//
// The population is the leg's own — the same predicate, the same bindings, the
// same project clause, and no resolved_at or scope filter, both of which the
// keyword leg applies later rather than in its statement — so the count cannot
// describe rows the leg never considered.
func (s *Store) keywordValidityExcluded(ctx context.Context, req CandidateRequest, project string, allProjects bool) (int, error) {
	query, args := keywordValidityExcludedStatement(project, sanitizeFTS(req.Query, s.logger), req.Now, allProjects)
	var n int
	if err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("candidates: keyword validity exclusion count: %w", err)
	}
	return n, nil
}

// poolIDsOf lists the fused pool in rank order, which is the order a window
// that lost rows to a concurrent delete backfills from.
func poolIDsOf(pool []*hybridCandidate) []string {
	ids := make([]string, len(pool))
	for i, c := range pool {
		ids[i] = c.id
	}
	return ids
}

// fillExplain builds the explain-only fields on a candidate set the caller
// asked to explain. It runs after Rows is final, or instead of window
// selection when scope narrowed the pool to nothing, and it hydrates the two
// excluded buckets over the SAME snapshot the ranking read — the explain
// payload must describe the same database state as the search it explains.
//
// The ranking facts are not read at all: the stages stamped them into the
// trace while they ran, and they are carried over verbatim. An explanation
// built by re-computing any of those numbers would be a second chance to
// disagree with the ranking, which is the failure this seam exists to remove.
func (s *Store) fillExplain(ctx context.Context, set *CandidateSet, req CandidateRequest, p SearchParams, preScope []string) error {
	// Excluded lives in the pre-scope pool the returned set does not carry,
	// in fused order: scope-narrowed rows first (in the order the pool fused),
	// then the eligible rows the window and its tail never reached. The
	// per-row verdict that decides which reason it carries is on the trace.
	inRows := make(map[string]bool, len(set.Rows))
	for _, c := range set.Rows {
		inRows[c.ID] = true
	}
	excludedIDs := make([]string, 0, len(preScope))
	for _, id := range preScope {
		if !inRows[id] {
			excludedIDs = append(excludedIDs, id)
		}
	}
	excluded, err := s.GetByIDs(ctx, excludedIDs)
	if err != nil {
		return fmt.Errorf("candidates: hydrate explain exclusions: %w", err)
	}
	set.Excluded = orderByIDs(excluded, excludedIDs)

	dropped, err := s.GetByIDs(ctx, set.floorDroppedIDs)
	if err != nil {
		return fmt.Errorf("candidates: hydrate explain floor drops: %w", err)
	}
	set.FloorDropped = orderByIDs(dropped, set.floorDroppedIDs)

	// decayRank orders only the window, so a row it never ordered carries no
	// recorded factor (DecayFactor has a 0.15 floor, so a recorded 0.0 is
	// unambiguously "never ordered"). What it carries instead is the factor it
	// WOULD have been ordered by: the tail row's own candidate fact, which
	// candidateOf computed at the ranking's clock, and for a row that never
	// reached a window the same function over the same clock. It is the one
	// documented exception to "every number is the one the ranking used" — a
	// field is better than a gap, and the value is the one the ranking would use.
	for _, c := range set.Rows {
		if t := p.trace.row(c.ID); t != nil && t.Decay == 0 {
			t.Decay, t.AgeDays = c.Decay, c.AgeDays
		}
	}
	for _, bucket := range [][]Memory{set.Excluded, set.FloorDropped} {
		for _, m := range bucket {
			if t := p.trace.row(m.ID); t != nil && t.Decay == 0 {
				t.AgeDays = ageDays(m.CreatedAt, req.Now)
				t.Decay = DecayFactor(m.Category, m.Retention, m.Pinned, t.AgeDays)
			}
		}
	}

	// The trace IS the record of the ranking: every per-candidate fact the
	// projection will read was stamped here, at the stage that decided it, so
	// the projection reads the same numbers the ranking used by construction.
	set.RankFacts = p.trace.rows
	// Knobs describe the ranking, so they are the parameters the ranking ran
	// with — the floor the store actually applied and the weights fusion read —
	// copied rather than re-resolved.
	set.ExplainKnobs = ExplainKnobs{
		RRFK:        p.RRFK,
		FTSWeight:   p.FTSWeight,
		VecWeight:   p.VecWeight,
		VectorFloor: float64(p.MinSimilarity),
	}
	return nil
}

// orderByIDs reorders hydrated rows to match an id list. GetByIDs does not
// preserve order, and the explain buckets are reported in the order the
// retrieval saw each row — fused order for the pool, raw leg order for the
// floor drops — so the two are reconciled here. A row the snapshot no longer
// holds is absent from the result, exactly as if it had never been a
// candidate.
func orderByIDs(hydrated []Memory, ids []string) []Memory {
	byID := make(map[string]Memory, len(hydrated))
	for _, m := range hydrated {
		byID[m.ID] = m
	}
	out := make([]Memory, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

// hydrateWindow materialises the selected window, falling back to the wider
// pool when a window id no longer exists. It is the same two reads production
// search performs, in the same order, for the same reason: the leg results are
// an earlier snapshot, and a row deleted since then has to disappear rather than
// be emitted from stale leg data.
//
// A read failure is returned, not degraded into a short window. The tail is
// hydrated by a second, independent read, so a transient failure on this one
// that the other survives would otherwise produce a candidate set missing the
// entire selected window — the strongest rows gone, the answer built from the
// tail alone, and "no matching memories" reported for a store that plainly has
// matches. A retrieval failure and an empty result must not look alike.
func hydrateWindow(ctx context.Context, s *Store, window HybridWindow, pool []*hybridCandidate) ([]Memory, error) {
	if len(window.IDs) == 0 {
		return nil, nil
	}
	hydrated, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate window: %w", err)
	}
	if len(hydrated) == len(window.IDs) {
		return selectHydratedWindow(hydrated, window, nil), nil
	}
	poolIDs := poolIDsOf(pool)
	backfill, err := s.GetByIDs(ctx, poolIDs)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate window backfill: %w", err)
	}
	return selectHydratedWindow(backfill, window, poolIDs), nil
}

// hydrateTail returns the rows the window cut, in the same decay order the
// window is returned in, so a predicate that removes a window row is backfilled
// by the row that would have ranked next.
//
// dropped names the window rows the retriever REMOVED as near-duplicate losers
// (#926). They are excluded for the same reason selected is: the pool walks in
// rank order, so a loser still eligible there would be re-added by its own
// backfill and would fill the very slot its removal freed. Excluding it is what
// hands that slot to the next distinct row.
//
// The tail is ordered but not demoted in the sense of a reorder; the
// retriever's removal runs over the combined set (window + tail) in the
// candidates path after hydration, so the returned membership is decided there.
func (s *Store) hydrateTail(ctx context.Context, pool []*hybridCandidate, selected []Memory, dropped []string, scores map[string]float64, limit int, now time.Time) ([]Memory, error) {
	taken := make(map[string]bool, len(selected)+len(dropped))
	for _, m := range selected {
		taken[m.ID] = true
	}
	for _, id := range dropped {
		taken[id] = true
	}
	ids := make([]string, 0, limit)
	for _, c := range pool {
		if len(ids) == limit {
			break
		}
		if !taken[c.id] {
			taken[c.id] = true
			ids = append(ids, c.id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("candidates: hydrate tail: %w", err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		si, sj := tailScore(rows[i], scores, now), tailScore(rows[j], scores, now)
		if si != sj {
			return si > sj
		}
		if scores[rows[i].ID] != scores[rows[j].ID] {
			return scores[rows[i].ID] > scores[rows[j].ID]
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

// tailScore is the score the tail is ordered on: the same base × category-and-
// age decay the window is ordered on, so a backfilled row ranks as if it had
// been selected.
func tailScore(m Memory, scores map[string]float64, now time.Time) float64 {
	return scores[m.ID] * DecayFactor(m.Category, m.Retention, m.Pinned, ageDays(m.CreatedAt, now))
}

// candidateOf pairs a hydrated row with the facts fusion produced for it.
func candidateOf(m Memory, scores map[string]float64, fts ftsLeg, vec vecLeg, now time.Time) Candidate {
	c := Candidate{Memory: m}
	c.Base = scores[m.ID]
	c.AgeDays = ageDays(m.CreatedAt, now)
	c.Decay = DecayFactor(m.Category, m.Retention, m.Pinned, c.AgeDays)
	c.Score = c.Base * c.Decay
	// -1 marks a leg that did not retrieve the row; rank 0 is a real first
	// place, and a cosine of 0 is a real measurement of "no similarity".
	c.FTSRank, c.VectorRank, c.VectorScore = -1, -1, -1
	if f, ok := fts.index[m.ID]; ok {
		c.FTSRank = f.rank
	}
	if f, ok := vec.index[m.ID]; ok {
		c.VectorRank = f.rank
		c.VectorScore = f.score
	}
	return c
}

// edgeScopeIDs is the id set the edge load covers: the returned rows, bounded by
// the window width. A link to a row beyond that bound cannot affect any
// decision the caller can reach, because no closure admits that many rows.
func edgeScopeIDs(rows []Candidate, window int) []string {
	limit := len(rows)
	if window > 0 && limit > window*2 {
		limit = window * 2
	}
	ids := make([]string, limit)
	for i := range limit {
		ids[i] = rows[i].ID
	}
	return ids
}

// loadCandidateEdges loads every valid edge whose two endpoints are candidates.
// A failed lookup is reported rather than swallowed: an empty edge set and a
// failed one mean different things to the stages that read it, and only the
// status can tell them apart.
func (s *Store) loadCandidateEdges(ctx context.Context, ids []string) ([]LinkEdge, EdgeStatus) {
	if len(ids) < 2 {
		return nil, EdgeStatus{Status: edgesUnavailable}
	}
	var edges []LinkEdge
	chunks := 0
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunks++
		chunk, err := s.edgesWithin(ctx, ids[start:end])
		if err != nil {
			s.logger.Debug("candidate edge load failed", "error", err)
			return nil, EdgeStatus{Status: edgesErr, Err: err.Error(), Chunks: chunks}
		}
		edges = append(edges, chunk...)
	}
	if len(edges) == 0 {
		// unavailable means "the query ran and found nothing". At more than one
		// chunk it is only true of the chunks that ran, so the count comes with it
		// and the caller can tell a complete read from a partial one — which
		// is the difference between "these candidates do not contradict" and "a
		// contradiction across a query boundary would not have been read".
		return nil, EdgeStatus{Status: edgesUnavailable, Chunks: chunks}
	}
	return edges, EdgeStatus{Status: edgesOK, Chunks: chunks}
}

// edgesWithin reads the valid edges among a set of candidate ids.
func (s *Store) edgesWithin(ctx context.Context, ids []string) ([]LinkEdge, error) {
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	// Both endpoint lists name the same ids, so the pair of IN clauses is the
	// same set twice.
	for pass := range 2 {
		for i, id := range ids {
			if pass == 0 {
				ph[i] = "?"
			}
			args = append(args, id)
		}
	}
	list := strings.Join(ph, ",")
	rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(`
		SELECT source_id, target_id, relation, strength FROM memory_links
		WHERE invalidated_at IS NULL
		  AND source_id IN (%s) AND target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("candidate edges: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var edges []LinkEdge
	for rows.Next() {
		var e LinkEdge
		var strength float64
		if err := rows.Scan(&e.From, &e.To, &e.Relation, &strength); err != nil {
			return nil, fmt.Errorf("candidate edges: %w", err)
		}
		e.Strength = strength
		edges = append(edges, e)
	}
	return edges, rows.Err()
}
