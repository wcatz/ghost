package assemble

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The abstention contract of #580: retrieval stops being a list that is either
// long or literally empty. A result carries a verdict (answerable, weak, empty),
// a reason from a closed vocabulary, a machine line a harness can parse and a
// human sentence that tells it whether to rely on the rows. The tests below pin
// each rule separately, because the whole point of the change is that the
// interesting cases are distinguishable from one another.

// ranked is a candidate with an explicit keyword rank, which is what Arm A
// evaluates. -1 is the retriever's "the keyword leg did not retrieve this row".
func ranked(id string, ftsRank int, content string) memory.Candidate {
	c := candidate(id, "proj", "fact", content, 0.5)
	c.FTSRank = ftsRank
	return c
}

// vectorScored is a candidate carrying the vector leg's cosine, for the arm-B
// half of the floor.
func vectorScored(id string, ftsRank int, score float64, content string) memory.Candidate {
	c := ranked(id, ftsRank, content)
	c.VectorScore = score
	return c
}

// hybridSet is a candidate set whose two applicable legs both ran and answered.
// It is the only state in which a floor verdict is made from both arms.
//
// The coverage flags mirror what memory.Candidates reports rather than what a
// test would like them to be: the keyword leg vouches for itself when it was not
// cut off at the window, and the vector leg vouches for nothing until its
// expected/indexed/unembedded counts are reconciled. A helper that set both true
// would let an absence claim pass on a corpus the real leg cannot vouch for.
func hybridSet(rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	set.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}
	return set
}

// weakSet is a candidate set whose two applicable legs both ran and answered,
// holding rows that cleared neither arm — the one state in which a result can
// be weak. A real store cannot produce it from a query with a keyword hit (the
// best keyword row is always rank 0), which is why the shape is built here.
func weakSet(rows ...memory.Candidate) *memory.CandidateSet {
	return hybridSet(rows...)
}

// ftsOnlySet is a candidate set whose vector leg was not applicable, so the
// request had no vector to compare against.
func ftsOnlySet(rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	set.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
		"vector": {Applicable: false},
	}
	return set
}

// hybridRequest is a request that reaches both legs.
func hybridRequest() Request {
	req := baseRequest()
	req.Condition = CondHybrid
	req.QueryVec = []float32{0.1, 0.2, 0.3}
	return req
}

// TestAStrongKeywordMatchStaysAnswerable is #580's regression test. The floor
// must be a no-op for a clear keyword hit: withholding a row the search found
// on the exact words the caller asked for is worse than returning a weak one.
func TestAStrongKeywordMatchStaysAnswerable(t *testing.T) {
	set := hybridSet(ranked("A1", 0, "helm deploy runs the release job"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: rank 0 is a clear keyword hit", res.Outcome)
	}
	if res.Reason != "floor_met" {
		t.Errorf("reason = %q, want floor_met", res.Reason)
	}
	if res.Abstention != "" {
		t.Errorf("abstention = %q, want none: an answerable result withholds nothing", res.Abstention)
	}
	if !strings.Contains(res.Response, "outcome=answerable") {
		t.Errorf("response carries no machine verdict:\n%s", res.Response)
	}
	if strings.Contains(res.Response, "do not rely") || strings.Contains(res.Response, "trustworthy") {
		t.Errorf("an answerable result must not carry abstention copy:\n%s", res.Response)
	}
}

// TestTheFtsRankFloorIsInclusiveAtThree pins both edges of Arm A. Rank 3 is the
// last rank that satisfies it and rank 4 the first that does not, and the -1
// sentinel is a row the keyword leg never retrieved rather than a very good
// match. An off-by-one here either withholds a real hit or admits a weak one.
//
// The sentinel's case is `no_floor_arm` rather than weak, and that is the point
// of the first row: a threshold applied to a sentinel is a comparison nobody
// made, so with arm B off there is nothing to have failed it.
func TestTheFtsRankFloorIsInclusiveAtThree(t *testing.T) {
	for _, tc := range []struct {
		rank int
		want Outcome
		why  string
	}{
		{rank: -1, want: OutcomeAnswerable, why: reasonNoFloorArm}, // never retrieved, so never compared
		{rank: 0, want: OutcomeAnswerable, why: reasonFloorMet},
		{rank: 3, want: OutcomeAnswerable, why: reasonFloorMet},
		{rank: 4, want: OutcomeWeak, why: reasonBelowFloor},
	} {
		set := hybridSet(ranked("A1", tc.rank, "database configuration pooling"))
		res := run(t, &fakeRetriever{set: set}, hybridRequest())
		if res.Outcome != tc.want {
			t.Errorf("fts rank %d: outcome = %q, want %q", tc.rank, res.Outcome, tc.want)
		}
		if res.Reason != tc.why {
			t.Errorf("fts rank %d: reason = %q, want %q", tc.rank, res.Reason, tc.why)
		}
	}
}

// TestAWeakResultStillReturnsItsRows: weak is a warning, not a filter. It
// withholds no row — only the response-fit post-pass may remove a row, and it
// records that it did — so the caller can see the weak candidates and judge
// them, with the sentence telling it not to rely on them.
func TestAWeakResultStillReturnsItsRows(t *testing.T) {
	set := hybridSet(ranked("A1", 6, "database configuration pooling"), ranked("A2", 7, "database configuration retry"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeWeak {
		t.Fatalf("outcome = %q, want weak: no admitted row cleared either arm", res.Outcome)
	}
	if res.Reason != "below_floor" {
		t.Errorf("reason = %q, want below_floor", res.Reason)
	}
	if !eq(itemIDs(res.Items), []string{"A1", "A2"}) {
		t.Errorf("items = %v, want both rows: weak withholds nothing", itemIDs(res.Items))
	}
	if !strings.Contains(res.Abstention, "do not rely") {
		t.Errorf("abstention = %q, want a sentence telling the caller not to rely on the rows", res.Abstention)
	}
	if !strings.Contains(res.Response, "outcome=weak") || !strings.Contains(res.Response, "reason=below_floor") {
		t.Errorf("response does not carry the weak verdict and its reason:\n%s", res.Response)
	}
}

// TestAnUnavailableVectorBackendKeepsKeywordHitsAnswerable is the acceptance
// criterion about the embedder. A leg that could not run, and a leg that ran and
// failed, are neither of them evidence that a match is weak: labelling a clear
// keyword hit below_floor because Ollama was down would turn an outage into a
// claim about the corpus.
//
// What the verdict is allowed to be is narrower than "answerable", though, and
// the three ranks pin the boundary. A rank inside the keyword arm is judged by it
// and answers floor_met, which is the case the outage must not touch. A rank
// beyond it is judged by the same arm and answers weak — a real keyword-relevance
// verdict, not a claim the outage manufactured, and the line says the vector arm
// did not run. A row the keyword leg never retrieved carries no value at all, so
// nothing could have been compared and it is `no_floor_arm`. A leg that ran and
// broke is different again: that is an incident, and it suppresses the floor
// outright whatever the ranks say.
func TestAnUnavailableVectorBackendKeepsKeywordHitsAnswerable(t *testing.T) {
	neverRan := memory.LegStatus{Applicable: true, Attempted: false}
	ranAndBroke := memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}
	for _, tc := range []struct {
		leg        memory.LegStatus
		rank       int
		want       Outcome
		wantReason string
	}{
		// The outage must not touch a clear keyword hit.
		{leg: neverRan, rank: 0, want: OutcomeAnswerable, wantReason: reasonFloorMet},
		{leg: neverRan, rank: 3, want: OutcomeAnswerable, wantReason: reasonFloorMet},
		// Beyond the arm, the keyword arm judged it and it did not clear.
		{leg: neverRan, rank: 4, want: OutcomeWeak, wantReason: reasonBelowFloor},
		{leg: neverRan, rank: -1, want: OutcomeAnswerable, wantReason: reasonNoFloorArm},
		// An incident suppresses the floor whatever the ranks are.
		{leg: ranAndBroke, rank: 0, want: OutcomeAnswerable, wantReason: reasonRetrievalPartial},
		{leg: ranAndBroke, rank: 4, want: OutcomeAnswerable, wantReason: reasonRetrievalPartial},
		{leg: ranAndBroke, rank: -1, want: OutcomeAnswerable, wantReason: reasonRetrievalPartial},
	} {
		set := setOf(ranked("A1", tc.rank, "database configuration pooling"))
		set.Legs = map[string]memory.LegStatus{
			"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
			"vector": tc.leg,
		}
		res := run(t, &fakeRetriever{set: set}, hybridRequest())

		if res.Outcome != tc.want || res.Reason != tc.wantReason {
			t.Errorf("fts rank %d, vector %+v: outcome/reason = %q/%q, want %q/%q",
				tc.rank, tc.leg, res.Outcome, res.Reason, tc.want, tc.wantReason)
		}
	}
}

// TestAVectorLegThatWasNeverApplicableIsNotAFailure: a request that asked for
// keyword-only retrieval has no vector leg to have failed, and the line says
// so rather than reporting a leg that was never asked for. The verdict is the
// keyword arm's own — a rank-0 hit clears it — and nothing about the absent leg
// reaches it.
func TestAVectorLegThatWasNeverApplicableIsNotAFailure(t *testing.T) {
	set := ftsOnlySet(ranked("A1", 0, "database configuration pooling"))

	res := run(t, &fakeRetriever{set: set}, baseRequest())

	if res.Outcome != OutcomeAnswerable || res.Reason != reasonFloorMet {
		t.Errorf("outcome/reason = %q/%q, want answerable/floor_met: the keyword arm judged this and it "+
			"cleared, with or without a vector leg", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Machine, "vector:absent") {
		t.Errorf("machine line = %q, want the vector leg reported as absent", res.Machine)
	}
	if strings.Contains(res.Machine, "retrieval_partial") {
		t.Errorf("machine line = %q, want no partial modifier: nothing failed", res.Machine)
	}
}

// TestTheMachineLineNamesBothLegsAndThePartialModifier: an incomplete retrieval
// has to be visible on the line a harness parses, not only in prose. The leg
// status says which leg, and the modifier says the answer is not whole.
//
// The row carries the keyword leg's -1, which is what a survivor of a failed
// keyword leg actually looks like — there is no rank to judge. That is the
// shape that makes the case worth testing: the vector leg answered, so on a
// first reading it is in play, and a floor that reads it that way would label a
// row weak because the keyword index is broken.
func TestTheMachineLineNamesBothLegsAndThePartialModifier(t *testing.T) {
	set := hybridSet(ranked("A1", -1, "database configuration pooling"))
	set.Legs["fts"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "no such table: memories_fts"}

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if !strings.Contains(res.Machine, "legs=fts:failed,vector:ok") {
		t.Errorf("machine line = %q, want the failed leg named", res.Machine)
	}
	// The modifier is asserted as the trailing token rather than as a substring,
	// because the reason already carries the same word here. The two answer
	// different questions — the reason why this is answerable, the modifier that
	// the retrieval was incomplete — and a test that cannot tell them apart
	// would pass with either one deleted.
	if !strings.HasSuffix(res.Machine, "retrieval_partial]") {
		t.Errorf("machine line = %q, want the partial modifier as its own field", res.Machine)
	}
	if res.Outcome != OutcomeAnswerable || res.Reason != reasonRetrievalPartial {
		t.Errorf("outcome/reason = %q/%q, want answerable/retrieval_partial: no floor verdict can be made "+
			"from a path that failed", res.Outcome, res.Reason)
	}
}

// TestAnEmptyResultFromAPartialRetrievalCarriesTheModifierToo: the modifier is
// not a restatement of the reason. Here a stage emptied the set, so the reason
// names the stage — and the retrieval was incomplete as well, which is the fact
// the modifier carries and the reason cannot.
func TestAnEmptyResultFromAPartialRetrievalCarriesTheModifierToo(t *testing.T) {
	row := candidate("A1", "proj", "fact", "the old deploy policy", 0.9)
	row.ValidUntil = stringPtr("2020-01-01 00:00:00")
	set := hybridSet(row)
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeEmpty || res.Reason != reasonAllInvalid {
		t.Fatalf("outcome/reason = %q/%q, want empty/all_invalid: validity removed the only row",
			res.Outcome, res.Reason)
	}
	if !strings.HasSuffix(res.Machine, "retrieval_partial]") {
		t.Errorf("machine line = %q, want the partial modifier: the reason names the stage, so the "+
			"line would otherwise report an empty result from a search that never finished", res.Machine)
	}
	if !hasNote(res.Notes, "vector leg failed") {
		t.Errorf("notes %v do not name the leg that failed", res.Notes)
	}
}

// TestTheMachineLineIsTheLastLine: the verdict has to be reachable without
// parsing prose, and a trailing line is the only position where an agent
// reading bottom-up meets it.
func TestTheMachineLineIsTheLastLine(t *testing.T) {
	set := hybridSet(ranked("A1", 0, "database configuration pooling"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Machine == "" {
		t.Fatal("machine line is empty")
	}
	if !strings.HasPrefix(res.Machine, "[ghost:outcome=") {
		t.Errorf("machine line = %q, want the [ghost:outcome=…] shape", res.Machine)
	}
	if got := strings.TrimSpace(res.Response[strings.LastIndex(res.Response, "[ghost:outcome="):]); got != res.Machine {
		t.Errorf("trailing line = %q, want exactly the machine line %q", got, res.Machine)
	}
	if res.Bytes != len(res.Response) {
		t.Errorf("Bytes = %d, want len(Response) = %d: a byte count that is not the response length bounds nothing", res.Bytes, len(res.Response))
	}
}

// TestAnEmptyResultCausedByExclusionsDoesNotSayNoMatchingMemories is the
// wording rule from the 2026-09-27 product direction. Rows were found and then
// withheld, so "no matching memories" is a claim about the store that the
// evidence contradicts: the caller would go looking for a memory to save
// instead of a stale one to refresh.
func TestAnEmptyResultCausedByExclusionsDoesNotSayNoMatchingMemories(t *testing.T) {
	row := candidate("A1", "proj", "fact", "the old deploy policy", 0.9)
	row.ValidUntil = stringPtr("2020-01-01 00:00:00")

	res := run(t, &fakeRetriever{set: ftsOnlySet(row)}, baseRequest())

	if res.Reason != "all_invalid" {
		t.Fatalf("reason = %q, want all_invalid", res.Reason)
	}
	if strings.Contains(res.Response, "No matching memories found") {
		t.Errorf("a result emptied by an exclusion must not claim nothing matched:\n%s", res.Response)
	}
	if !strings.Contains(strings.ToLower(res.Response), "no sufficiently trustworthy memory found") {
		t.Errorf("response = %q, want the trustworthy-memory wording", res.Response)
	}
}

// TestAnEmptyStoreStillSaysNothingMatched: over complete coverage the absence
// sentence is exactly right, and the new wording must not swallow it. A caller
// that cannot tell the two cases apart has learned nothing.
func TestAnEmptyStoreStillSaysNothingMatched(t *testing.T) {
	// A keyword-only request whose leg answered and was not truncated searched
	// everything it could see, which is the only state in which absence is a
	// statement about the store.
	res := run(t, &fakeRetriever{set: ftsOnlySet()}, baseRequest())

	if res.Reason != "no_candidates" {
		t.Fatalf("reason = %q, want no_candidates", res.Reason)
	}
	if !strings.Contains(res.Response, "No matching memories found") {
		t.Errorf("response = %q, want the absence sentence for an empty store", res.Response)
	}
}

// TestAConfiguredCosineNeverRendersAsTheTokenOffAssigns: the machine line gives
// `abstain_cosine` three states, and a number means the arm is in force. Three
// decimals is the field's width, but a threshold below 0.0005 rounds to
// `0.000` — the same text a reader parses as "a threshold of zero", which every
// row clears and which the glossary hands to `off`. An arm in force that renders
// as the number `off` means is a line that says the opposite of what happened.
func TestAConfiguredCosineNeverRendersAsTheTokenOffAssigns(t *testing.T) {
	set := hybridSet(
		vectorScored("A1", 9, 0.0001, "database configuration pooling"),
		vectorScored("A2", 8, 0.0002, "database configuration retries"),
	)
	req := hybridRequest()
	req.AbstainCosine = 0.0004

	res := run(t, &fakeRetriever{set: set}, req)

	if !res.Trace.Floors.VectorArmOn {
		t.Fatalf("precondition: a cosine of 0.0004 did not arm the vector leg: %+v", res.Trace.Floors)
	}
	if res.Outcome != OutcomeWeak {
		t.Fatalf("precondition: outcome = %q, want weak: only a weak result proves the arm ran", res.Outcome)
	}
	if strings.Contains(res.Machine, "abstain_cosine=0.000 ") || strings.HasSuffix(res.Machine, "abstain_cosine=0.000") {
		t.Errorf("machine line = %q, which renders a threshold in force as the number `off` is given", res.Machine)
	}
	if !strings.Contains(res.Machine, "abstain_cosine=0.0004") {
		t.Errorf("machine line = %q, want the configured threshold at a precision that states it", res.Machine)
	}
}

// TestTheReasonForAnUnavailableVectorLegIsNamed: a search whose vector leg could
// not RUN cannot report absence, so the reason has to name the leg rather than
// claiming the corpus was searched — and the sentence has to be true of the
// state the reason stands for, which is a leg that never started.
func TestTheReasonForAnUnavailableVectorLegIsNamed(t *testing.T) {
	set := ftsOnlySet()
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: false, Available: false}

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeEmpty || res.Reason != "vector_backend_unavailable" {
		t.Errorf("outcome/reason = %q/%q, want empty/vector_backend_unavailable", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Abstention, "incomplete") {
		t.Errorf("abstention = %q, want it to say the search was incomplete", res.Abstention)
	}
	// The sentence's whole claim is that the keyword leg was the only one that
	// ran, so it is only true while the vector leg never started. It was the
	// load-bearing clause and it was the false one.
	if !strings.Contains(res.Abstention, "only the keyword leg ran") {
		t.Errorf("abstention = %q, want it to say the keyword leg was the only one that ran", res.Abstention)
	}
}

// TestAVectorLegThatRanAndFailedIsAnIncidentNotAMissingEmbedder: a leg that
// started and broke is not a machine with no embedder, and the two sentences
// ask the caller for opposite things — fix the machine once, or retry. The
// assembler's own non-empty branch draws exactly this line (`retrieval_partial`
// before a configuration reason), so the empty branch has to draw it too.
func TestAVectorLegThatRanAndFailedIsAnIncidentNotAMissingEmbedder(t *testing.T) {
	set := ftsOnlySet()
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeEmpty || res.Reason != reasonRetrievalFailed {
		t.Errorf("outcome/reason = %q/%q, want empty/retrieval_failed: a leg that ran and broke is an "+
			"incident, not a machine with no embedder", res.Outcome, res.Reason)
	}
	for _, diagnoses := range []string{"backend was unavailable", "could not run"} {
		if strings.Contains(res.Abstention, diagnoses) {
			t.Errorf("abstention = %q, which tells the caller their vector leg never ran (%q) when it "+
				"ran and failed", res.Abstention, diagnoses)
		}
	}
	if !strings.Contains(res.Abstention, "retry") {
		t.Errorf("abstention = %q, want the incident's next step", res.Abstention)
	}
}

// TestArmBIsOffUntilMeasured: the vector arm ships disabled. Enabling it by
// default would be a threshold nobody measured, and the bench data says the
// no-answer and answerable cosine distributions overlap — a constant cannot
// separate them. `abstain_cosine=off` is what distinguishes "no threshold" from
// "a threshold of zero", which every row clears.
func TestArmBIsOffUntilMeasured(t *testing.T) {
	set := hybridSet(vectorScored("A1", -1, 0.99, "database configuration pooling"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	// The row carries a 0.99 cosine and no keyword rank. With arm B off nothing
	// can compare that cosine and there is no keyword rank either, so no arm holds
	// a value: the honest verdict is that no floor could be applied, not that the
	// row fell below one. What this rules out is the row being ACCEPTED on its
	// cosine, which is the failure the off-by-default exists to prevent.
	if res.Outcome != OutcomeAnswerable || res.Reason != reasonNoFloorArm {
		t.Errorf("outcome/reason = %q/%q, want answerable/no_floor_arm: a high cosine cannot satisfy an "+
			"arm that is off, and nothing else could judge the row either", res.Outcome, res.Reason)
	}
	if res.Trace.Floors.VectorArmOn {
		t.Error("VectorArmOn is true with AbstainCosine 0, so the arm would be evaluated against a zero threshold")
	}
	if !strings.Contains(res.Machine, "abstain_cosine=off") {
		t.Errorf("machine line = %q, want abstain_cosine=off", res.Machine)
	}

	req := hybridRequest()
	req.AbstainCosine = 0.5
	on := run(t, &fakeRetriever{set: set}, req)
	if on.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: 0.99 clears a configured cosine of 0.5", on.Outcome)
	}
	if !on.Trace.Floors.VectorArmOn {
		t.Error("VectorArmOn is false with AbstainCosine 0.5")
	}
	if !strings.Contains(on.Machine, "abstain_cosine=0.500") {
		t.Errorf("machine line = %q, want the configured threshold rendered", on.Machine)
	}
}

// TestTheFloorsRecordTheThresholdTheyUsed: a reader that has to re-derive a
// verdict cannot, so the trace carries the exact numbers that produced it.
func TestTheFloorsRecordTheThresholdTheyUsed(t *testing.T) {
	req := hybridRequest()
	req.AbstainCosine = 0.62

	res := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 0, "pooling"))}, req)

	f := res.Trace.Floors
	if f.FTSRankMax != 3 {
		t.Errorf("FTSRankMax = %d, want 3", f.FTSRankMax)
	}
	if f.VectorCosine != 0.62 {
		t.Errorf("VectorCosine = %v, want the caller's 0.62", f.VectorCosine)
	}
}

// TestATokenEstimateIsReportedPerItemAndInTotal: callers budget in tokens and
// the assembler measures in bytes, so it reports the conversion as an estimate
// rather than leaving each caller to invent its own. Bytes stay the budget unit;
// nothing here can be mistaken for a tokenizer's count because the line says
// `tokens_est`.
func TestATokenEstimateIsReportedPerItemAndInTotal(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	rows := []memory.Candidate{
		candidate("A1", "proj", "fact", strings.Repeat("x", 40), 0.9), // 10
		candidate("A2", "proj", "fact", strings.Repeat("y", 9), 0.8),  // 3 (2.25 rounded up)
	}

	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)

	if res.Items[0].Tokens != 10 {
		t.Errorf("item 0 tokens = %d, want 10 for 40 bytes", res.Items[0].Tokens)
	}
	if res.Items[1].Tokens != 3 {
		t.Errorf("item 1 tokens = %d, want 3 for 9 bytes: a fraction of a token is still a token the caller pays for", res.Items[1].Tokens)
	}
	if res.Tokens != 13 {
		t.Errorf("total tokens = %d, want 13", res.Tokens)
	}
	if !strings.Contains(res.Machine, "tokens_est=13") {
		t.Errorf("machine line = %q, want the total token estimate", res.Machine)
	}
}

// TestAFractionOfATokenIsStillAToken: rounding down would report a short memory
// as free, and a caller budgeting from this would overspend.
func TestAFractionOfATokenIsStillAToken(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5

	res := run(t, &fakeRetriever{set: ftsOnlySet(candidate("A1", "proj", "fact", "hi", 0.9))}, req)

	if res.Items[0].Tokens != 1 {
		t.Errorf("tokens = %d for a 2-byte memory, want 1", res.Items[0].Tokens)
	}
}

// TestTheResponseFitPassDropsTheLowestRankedRow: the complete response has to
// fit the caller's byte cap, and the only rows it may remove for that are the
// lowest-ranked ones — dropping a strong hit to keep a weak one would invert
// the ranking the pipeline just produced.
func TestTheResponseFitPassDropsTheLowestRankedRowAndFits(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	rows := []memory.Candidate{
		ranked("A1", 0, "first row text"),
		ranked("A2", 1, "second row text"),
		ranked("A3", 2, "third row text"),
	}
	set := ftsOnlySet(rows...)

	full := run(t, &fakeRetriever{set: set}, req)
	if len(full.Items) != 3 {
		t.Fatalf("precondition: %d items, want 3", len(full.Items))
	}

	req.Budget.MaxBytes = full.Bytes - 1
	res := run(t, &fakeRetriever{set: set}, req)

	if res.Bytes > req.Budget.MaxBytes {
		t.Errorf("response is %d bytes, want at most %d", res.Bytes, req.Budget.MaxBytes)
	}
	if res.Bytes != len(res.Response) {
		t.Errorf("Bytes = %d, want the last render's count %d", res.Bytes, len(res.Response))
	}
	if !eq(itemIDs(res.Items), []string{"A1", "A2"}) {
		t.Errorf("items = %v, want the two highest-ranked rows", itemIDs(res.Items))
	}
	fit := stageTrace(t, res, "response_fit")
	if !eq(fit.DroppedIDs, []string{"A3"}) {
		t.Errorf("response_fit dropped %v, want the lowest-ranked row", fit.DroppedIDs)
	}
	if fit.In != 3 || fit.Out != 2 {
		t.Errorf("response_fit in/out = %d/%d, want 3/2", fit.In, fit.Out)
	}
	if !hasDecision(res, "A3", "response_fit", "response_budget") {
		t.Errorf("no per-row decision for the row the fit pass removed: %+v", res.Trace.Decisions)
	}
}

// TestTheResponseFitPassRecomputesTheOutcome: a row the pass removed may have
// been the one clearing the floor, so a result that was answerable can become
// weak after a trim. Reporting the seed's verdict would claim a floor the answer
// no longer has.
func TestTheResponseFitPassRecomputesTheOutcome(t *testing.T) {
	// A1 clears arm A; A2 is a row the keyword leg never retrieved, so it clears
	// nothing. The two halves below differ only in WHICH of them clears the
	// floor, because that is the whole property: a carried-over verdict passes
	// the first case and fails the second.
	long := strings.Repeat("top ranked row ", 40)

	t.Run("the surviving row still clears the floor", func(t *testing.T) {
		set := hybridSet(ranked("A1", 0, long), ranked("A2", -1, "second row"))
		req := baseRequest()
		req.Budget.MaxItems = 5
		full := run(t, &fakeRetriever{set: set}, req)
		if full.Outcome != OutcomeAnswerable || full.Reason != "floor_met" {
			t.Fatalf("precondition: outcome/reason = %q/%q, want answerable/floor_met", full.Outcome, full.Reason)
		}
		req.Budget.MaxBytes = full.Bytes - 1
		res := run(t, &fakeRetriever{set: set}, req)

		if !eq(itemIDs(res.Items), []string{"A1"}) {
			t.Fatalf("items = %v, want the row the pass kept", itemIDs(res.Items))
		}
		if res.Outcome != OutcomeAnswerable || res.Reason != "floor_met" {
			t.Errorf("outcome/reason = %q/%q, want the recomputed answerable/floor_met", res.Outcome, res.Reason)
		}
	})

	t.Run("the dropped row was the only floor carrier", func(t *testing.T) {
		// A2 clears arm A and is the LAST admitted row, so it is the one the pass
		// drops; what is left never cleared anything. "Last admitted" is the
		// pass's order, not a keyword rank — the pipeline's order is authoritative
		// and stage 9 preserves it — which is why A1 (the sentinel rank -1) is the
		// survivor. A2 is also the LARGE one: a weak answer carries an abstention
		// sentence, and a tiny dropped row would cost fewer bytes than that
		// sentence adds, so with the sizes reversed no cap separates the one-row
		// render from the two-row one.
		set := hybridSet(ranked("A1", 4, "short row"), ranked("A2", 0, long))
		req := baseRequest()
		req.Budget.MaxItems = 5
		seeded := run(t, &fakeRetriever{set: set}, req)
		if seeded.Outcome != OutcomeAnswerable {
			t.Fatalf("precondition: outcome = %q, want answerable", seeded.Outcome)
		}
		// The cap is the size the same verdict takes with the surviving row ALONE,
		// measured rather than taken off the seed: a weak answer carries an
		// abstention sentence the seed does not, so trimming a row's worth off the
		// seed lands past the one-row size and empties the answer instead.
		alone := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 4, "short row"))}, req)
		req.Budget.MaxBytes = alone.Bytes
		res := run(t, &fakeRetriever{set: set}, req)

		if !eq(itemIDs(res.Items), []string{"A1"}) {
			t.Fatalf("items = %v, want the row the pass kept", itemIDs(res.Items))
		}
		if res.Outcome != OutcomeWeak || res.Reason != reasonBelowFloor {
			t.Errorf("outcome/reason = %q/%q, want weak/below_floor: the pass removed the only row that "+
				"cleared a floor, so a carried-over verdict would claim one", res.Outcome, res.Reason)
		}
		if !strings.Contains(res.Response, "do not rely") {
			t.Errorf("the recomputed weak verdict carries no abstention sentence:\n%s", res.Response)
		}
	})
}

// TestTheResponseFitPassReportsAllOverBudget: a response that fits no row at
// all is not an absence claim, it is a budget that could not hold the answer.
func TestTheResponseFitPassReportsAllOverBudget(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 12)),
		ranked("A2", 1, strings.Repeat("second row content ", 12)),
	)

	full := run(t, &fakeRetriever{set: set}, req)
	if full.Outcome != OutcomeAnswerable || len(full.Items) != 2 {
		t.Fatalf("precondition: the uncapped result was %q with %d items, want answerable with 2",
			full.Outcome, len(full.Items))
	}

	capAt, boundary := roomiestEmptyCap(t, set, req, full)
	if len(boundary.Items) != 0 {
		t.Fatalf("no cap in (0, %d) emptied the block: the pass cannot report all_over_budget, "+
			"so a response that fits no row would be an error instead", full.Bytes)
	}
	if boundary.Outcome != OutcomeEmpty || boundary.Reason != reasonAllOverBudget {
		t.Errorf("outcome/reason = %q/%q, want empty/all_over_budget", boundary.Outcome, boundary.Reason)
	}
	if boundary.Bytes > capAt {
		t.Errorf("the empty envelope is %d bytes, want at most the cap it was fitted to (%d)", boundary.Bytes, capAt)
	}
	if !strings.Contains(boundary.Response, "response budget") {
		t.Errorf("response = %q, want the sentence that says the budget is what removed the rows", boundary.Response)
	}
}

// TestTheBudgetCopyNamesTheCapRatherThanAnUnreachableLimit: the byte cap is
// server-side — no tool argument reaches it — so advice to "raise the limit"
// sends a caller after a knob it does not have. The number, and the action the
// caller does have, are what the sentence has to carry.
func TestTheBudgetCopyNamesTheCapRatherThanAnUnreachableLimit(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 12)),
		ranked("A2", 1, strings.Repeat("second row content ", 12)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	capAt, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if strings.Contains(res.Abstention, "Raise the limit") {
		t.Errorf("the sentence tells the caller to raise a limit no tool argument reaches: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, fmt.Sprintf("%d", capAt)) {
		t.Errorf("the sentence does not name the byte cap the caller cannot change: %q", res.Abstention)
	}
}

// TestTheResponseFitSentenceNamesTheCapAndTheOtherStage: the fit pass has its
// own `all_over_budget` branch, and it was the one empty sentence attributing
// every removed row to the byte cap. A set that validity emptied and that the
// fit pass then emptied renders it above a breakdown naming both stages, which
// is the sentence/note contradiction the other four branches do not have — and
// unlike them it needs no large cap to reach, only a caller that sets
// `Budget.MaxBytes` itself.
func TestTheResponseFitSentenceNamesTheCapAndTheOtherStage(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	rows := []memory.Candidate{
		ranked("A1", 0, strings.Repeat("first row content ", 120)),
		ranked("A2", 1, strings.Repeat("second row content ", 120)),
	}
	gone := ranked("A3", 0, "an expired row")
	gone.ValidUntil = &expired
	set := ftsOnlySet(append(rows, gone)...)

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)
	_, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if !strings.Contains(res.Abstention, "response budget") {
		t.Fatalf("precondition: the sentence is not the response-fit one, so this fixture cannot test "+
			"its claim: %q", res.Abstention)
	}
	if !hasNote(res.Notes, "validity 1") {
		t.Fatalf("precondition: the breakdown does not name the other stage, so the sentence has only "+
			"one cause to account for and the test would pass for the wrong reason: %v", res.Notes)
	}
	if !strings.Contains(res.Abstention, perStageNote) {
		t.Errorf("the sentence names one stage while the note names two, and does not say so: %q",
			res.Abstention)
	}
}

// TestTheStagePointerDisappearsWithTheNoteItPointsAt: perStageNote is a pointer
// to a note the response-fit pass is allowed to cut, and it is cut from the end
// so the breakdown — which leads the list — is the last one to go. Under a cap
// tight enough to reach it, an answer would read "The note below breaks the
// removals down per stage." with nothing below, which is the checkability the
// sentence exists to promise, gone. The pointer has to leave with its note.
func TestTheStagePointerDisappearsWithTheNoteItPointsAt(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	rows := []memory.Candidate{
		ranked("A1", 0, strings.Repeat("first row content ", 120)),
		ranked("A2", 1, strings.Repeat("second row content ", 120)),
	}
	gone := ranked("A3", 0, "an expired row")
	gone.ValidUntil = &expired
	set := ftsOnlySet(append(rows, gone)...)
	// A SECOND note, and that is the point of the fixture. The tail cut takes from
	// the end, so with only the breakdown in the list one cut removes the note the
	// pointer names and the two states are indistinguishable. The retrieval-failure
	// note sorts after it, so a one-note cut here leaves the breakdown in place —
	// the case a pointer tested by cut COUNT gets wrong, dropping the promise off
	// an answer that still carries it. It does not change the reason: rows were
	// found and removed, so emptyReason names the stage that removed them.
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)

	// The tightest empty cap and the note-bearing envelope move together — the
	// sentence quotes the cap, so a cap one byte smaller renders a byte shorter —
	// which makes any single cap a boundary to be guessed at. The invariant is
	// swept instead: across the band the pointer is present exactly when the
	// breakdown is, and the sweep has to reach all three states (both notes, only
	// the breakdown, no notes) or it proves nothing.
	_, roomiest := roomiestEmptyCap(t, set, req, full)
	if !hasNote(roomiest.Notes, "validity 1") {
		t.Fatalf("precondition: the roomiest empty cap does not keep the breakdown, so no cap in this "+
			"fixture can test losing it: %v", roomiest.Notes)
	}
	if len(roomiest.Notes) < 2 {
		t.Fatalf("precondition: the roomiest empty cap carries %d note(s), so a one-note cut cannot "+
			"leave the breakdown behind and the mid-band state is unreachable: %v",
			len(roomiest.Notes), roomiest.Notes)
	}
	both, breakdownOnly, none := 0, 0, 0
	for cap := roomiest.Bytes; cap > roomiest.Bytes/2; cap-- {
		req.Budget.MaxBytes = cap
		got, err := Run(context.Background(), &fakeRetriever{set: set}, req)
		if err != nil || len(got.Items) != 0 {
			continue
		}
		switch breakdown := hasNote(got.Notes, "validity 1"); {
		case breakdown && len(got.Notes) > 1:
			both++
		case breakdown:
			breakdownOnly++
		default:
			none++
		}
		// The exact pairing, stated on the note the pointer names rather than on
		// the note COUNT: a second note surviving is not a reason to drop it.
		if strings.Contains(got.Abstention, perStageNote) != hasNote(got.Notes, "validity 1") {
			t.Errorf("cap %d: the sentence's pointer (%t) and the breakdown it names (%t) disagree: %q",
				cap, strings.Contains(got.Abstention, perStageNote), hasNote(got.Notes, "validity 1"),
				got.Abstention)
		}
		if !strings.Contains(got.Abstention, "response budget") {
			t.Errorf("cap %d: the sentence lost its cause along with the pointer: %q", cap, got.Abstention)
		}
	}
	if both == 0 || breakdownOnly == 0 || none == 0 {
		t.Fatalf("the sweep never reached all three states (both=%d breakdownOnly=%d none=%d), so it "+
			"did not test the transition the pointer has to follow", both, breakdownOnly, none)
	}
}

// roomiestEmptyCap finds the largest byte cap whose answer still holds no row,
// with the result it produced.
//
// It scans downward rather than bisecting, because the property is not monotone
// in the cap and a bisection over a non-monotone predicate converges to the
// wrong answer without ever failing. There are three bands: above the full render
// everything is admitted, in a band between the empty envelope's size and a
// one-row render's size no row is admitted, and below the empty envelope Run
// refuses the block outright. Monotonicity holds only within the first band, and
// the band a caller cares about is the third.
//
// The ROOMIEST end is returned, and only that end, for two reasons. The cap is
// measured rather than written down because it moves with every sentence the
// envelope carries — including the cap number the budget copy quotes — so a
// hard-coded figure would go stale the first time a word changed. And a test
// asserting that the diagnostic notes SURVIVED needs room for them: the
// breakdown note is itself part of the envelope it explains, so at the tight end
// the pass drops the very note such a test is checking for. A caller that needs
// the tightest cap does not need this helper — it gets whichever end fits, and
// the verdict is the same either way.
func roomiestEmptyCap(t *testing.T, set *memory.CandidateSet, req Request, full Result) (int, Result) {
	t.Helper()
	for cap := full.Bytes; cap > 0; cap-- {
		req.Budget.MaxBytes = cap
		got, err := Run(context.Background(), &fakeRetriever{set: set}, req)
		if err == nil && len(got.Items) == 0 {
			return cap, got
		}
	}
	t.Fatalf("no cap in (0, %d) emptied the block, so the pass cannot report all_over_budget", full.Bytes)
	return 0, Result{}
}

// TestAResponseThatCannotFitIsAnError: the empty envelope still carries the
// verdict, the reason and the machine line. A cap below that is not an empty
// answer — it is a caller asking for less than an answer costs, and reporting
// `empty` would claim the store had nothing to say.
func TestAResponseThatCannotFitIsAnError(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxBytes = 20

	_, err := Run(context.Background(), &fakeRetriever{set: ftsOnlySet()}, req)

	if !errors.Is(err, ErrResponseBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrResponseBudgetExceeded", err)
	}
}

// TestAFitPassEmptyStillExplainsWhatRemovedTheRows: the per-stage breakdown is
// what makes an empty answer's leading sentence checkable, and the response-fit
// pass is the one thing that empties a set without going through stage 9. If it
// shrinks the admitted items but not the surviving rows, the breakdown's own gate
// stays false and the note silently disappears — on exactly the case this
// post-pass introduces.
func TestAFitPassEmptyStillExplainsWhatRemovedTheRows(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	// Rows big enough that a one-row render is far larger than an empty one, so
	// the zero-item band is wide and the breakdown note fits inside it. With small
	// rows the band is a byte wide and the pass has no room for the note at any
	// cap in it, which would make this test pass or fail on the fixture's size
	// rather than on the derivation.
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 40)),
		ranked("A2", 1, strings.Repeat("second row content ", 40)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	// The roomiest cap that still admits nothing, which is what roomiestEmptyCap
	// returns: the breakdown note is part of the envelope it explains, so at the
	// tight end the pass drops the very note this asserts on. That is the pass
	// working as designed; the defect pinned here is the note never being
	// derived at all.
	capAt, res := roomiestEmptyCap(t, set, req, full)

	if len(res.Items) != 0 {
		t.Fatalf("precondition: %d items, want none", len(res.Items))
	}
	if !hasNote(res.Notes, "none reached the answer") {
		t.Errorf("notes %v do not explain that rows were removed, so the sentence above them is "+
			"not checkable:\n%s", res.Notes, res.Response)
	}
	if !hasNote(res.Notes, "response_fit 2") {
		t.Errorf("the breakdown does not count the pass that removed them (cap %d): %v", capAt, res.Notes)
	}
}

// TestTheLegSummarySeparatesAbsentFromNotRun: the tool always asks for a hybrid
// search, so the vector leg is always applicable — and on a machine with no
// embedder, or one whose embed call just failed, it is applicable and never
// attempted. Reporting that with the same word a keyword-only request earns tells
// a harness reading the line that the leg did not apply, when in fact the request
// asked for it and it did not answer.
func TestTheLegSummarySeparatesAbsentFromNotRun(t *testing.T) {
	notRun := ftsOnlySet()
	notRun.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: false}
	ran := run(t, &fakeRetriever{set: notRun}, baseRequest())

	if !strings.Contains(ran.Machine, "vector:not_run") {
		t.Errorf("machine line = %q, want an applicable-but-never-run leg reported as not_run", ran.Machine)
	}

	absent := run(t, &fakeRetriever{set: ftsOnlySet()}, baseRequest())
	if !strings.Contains(absent.Machine, "vector:absent") {
		t.Errorf("machine line = %q, want a non-applicable leg reported as absent", absent.Machine)
	}
}

// TestTheResponseFitPassStopsAtTheLimit: the whole envelope is measured, so the
// three cases the design names — at the limit, one byte over, and well under it
// after the pass — each have a defined answer.
func TestTheResponseFitPassStopsAtTheLimit(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	body := strings.Repeat("row content ", 12)
	set := ftsOnlySet(
		ranked("A1", 0, body),
		ranked("A2", 1, body),
		ranked("A3", 2, body),
		ranked("A4", 3, body),
	)

	full := run(t, &fakeRetriever{set: set}, req)
	for _, tc := range []struct {
		name      string
		cap       int
		wantItems int
	}{
		{name: "at the limit", cap: full.Bytes, wantItems: 4},
		{name: "one byte over", cap: full.Bytes - 1, wantItems: 3},
		{name: "well under", cap: full.Bytes / 2, wantItems: 1},
	} {
		req.Budget.MaxBytes = tc.cap
		res, err := Run(context.Background(), &fakeRetriever{set: set}, req)
		if err != nil {
			t.Errorf("%s: Run: %v", tc.name, err)
			continue
		}
		if len(res.Items) != tc.wantItems {
			t.Errorf("%s: items = %d, want %d", tc.name, len(res.Items), tc.wantItems)
		}
		if res.Bytes > tc.cap {
			t.Errorf("%s: response is %d bytes, want at most %d", tc.name, res.Bytes, tc.cap)
		}
	}
}

// TestDiagnosticNotesDropBeforeTheMachineLine: a cap so small that the answer
// and its diagnostics cannot both fit must lose the diagnostics. The verdict and
// its reason are the answer; a leg-failure disclosure is not, and dropping the
// verdict instead would return a caveat with no result to qualify.
func TestDiagnosticNotesDropBeforeTheMachineLine(t *testing.T) {
	short := run(t, &fakeRetriever{set: ftsOnlySet()}, baseRequest())
	note := strings.Repeat("a leg error nobody can read. ", 8)
	set := ftsOnlySet()
	set.Legs["fts"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: note}
	long := run(t, &fakeRetriever{set: set}, baseRequest())
	if len(long.Notes) == 0 {
		t.Fatalf("precondition: the failing leg produced no note: %+v", long)
	}
	if long.Outcome != OutcomeEmpty || long.Reason != reasonRetrievalFailed {
		t.Fatalf("precondition: outcome/reason = %q/%q, want empty/retrieval_failed", long.Outcome, long.Reason)
	}

	req := baseRequest()
	req.Budget.MaxBytes = short.Bytes + (len(long.Response)-short.Bytes)/2
	res := run(t, &fakeRetriever{set: set}, req)

	if res.Outcome != OutcomeEmpty || res.Reason != reasonRetrievalFailed {
		t.Errorf("outcome/reason = %q/%q, want the verdict unchanged by a note drop", res.Outcome, res.Reason)
	}
	if res.Bytes > req.Budget.MaxBytes {
		t.Errorf("response is %d bytes, want at most %d", res.Bytes, req.Budget.MaxBytes)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want them dropped before the machine line", res.Notes)
	}
	fit := stageTrace(t, res, "response_fit")
	if len(fit.Notes) == 0 {
		t.Errorf("response_fit recorded no dropped note: %+v", fit)
	}
	if !strings.Contains(res.Response, "[ghost:outcome=") {
		t.Errorf("response = %q, want the machine line kept", res.Response)
	}
}

// TestTheResponseFitEmptyNamesTheByteCap: the response-fit producer of
// `all_over_budget`. This one really was the response cap, so the sentence quotes
// it and says the one thing the caller can actually do.
func TestTheResponseFitEmptyNamesTheByteCap(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 40)),
		ranked("A2", 1, strings.Repeat("second row content ", 40)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	capAt, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if strings.Contains(res.Abstention, "Raise the limit") {
		t.Errorf("the sentence tells the caller to raise a limit no tool argument reaches: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, fmt.Sprintf("%d", capAt)) {
		t.Errorf("the sentence does not name the byte cap the caller cannot change: %q", res.Abstention)
	}
}

// TestTheBudgetCopyNamesTheCapThatEmptiedTheSet: stage 9 is the other producer
// of `all_over_budget`, and it reaches that reason with MaxBytes at 0. A sentence
// quoting "the response budget of 0 bytes" names a budget the caller never set
// and tells it to fix a filter it never passed.
//
// The fixture is a per-bucket CONTENT byte cap on purpose, because that is the
// bound whose remedy is NOT a bigger row limit: raising `limit` admits rows this
// cap cuts again, so advice to do it sends the caller round the same loop with a
// bigger number. The sentence therefore has to name the byte cap and say why the
// limit is not the answer.
func TestTheBudgetCopyNamesTheCapThatEmptiedTheSet(t *testing.T) {
	set := ftsOnlySet(
		ranked("A1", 0, "a row of text"),
		ranked("A2", 1, "another row"),
	)
	req := baseRequest()
	req.Budget = Budget{MaxItems: 10, Slices: []Slice{{Bucket: "proj", MaxBytes: 1}}}

	res := run(t, &fakeRetriever{set: set}, req)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if strings.Contains(res.Abstention, "response budget") {
		t.Errorf("the sentence blames the response cap, which this request never set (MaxBytes=%d): %q",
			req.Budget.MaxBytes, res.Abstention)
	}
	if !strings.Contains(res.Abstention, "byte cap") {
		t.Errorf("the sentence does not name the bound that emptied the set, a per-bucket content byte "+
			"cap: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, "Raising the row limit would not help") {
		t.Errorf("the sentence tells the caller to raise a limit that cannot admit another row past a "+
			"byte cap: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, "byte cap") || strings.Contains(res.Abstention, "Raise the limit to see them") {
		t.Errorf("the sentence offers the row-count remedy for a byte-capped slice: %q", res.Abstention)
	}

	// There is no mirror case, and that is the fact worth pinning: no row-COUNT
	// cap can empty a block. A slice's item cap admits the first row of its
	// bucket, since the cap can only be exceeded at zero, and zero means
	// unbounded; the total cap keeps min(MaxItems, admitted) and MaxItems 0 means
	// unbounded too. So an `all_over_budget` sentence is only ever about a
	// content byte cap or the response-fit pass, and a future change that lets a
	// row count empty a block has to extend the sentence. This says so where the
	// next change will meet it.
	for _, tc := range []struct {
		name string
		b    Budget
	}{
		{"total item cap", Budget{MaxItems: 1}},
		{"slice item cap", Budget{MaxItems: 10, Slices: []Slice{{Bucket: "proj", MaxItems: 1}}}},
	} {
		got := run(t, &fakeRetriever{set: set}, func() Request {
			r := baseRequest()
			r.Budget = tc.b
			return r
		}())
		if got.Outcome == OutcomeEmpty {
			t.Errorf("%s: this row-count cap emptied the block, so all_over_budget can now be about a row "+
				"count and the sentence has to name that instead of a byte cap: %q", tc.name, got.Abstention)
		}
	}
}

// TestAnAbsenceClaimNeedsCompleteCoverage: `no_candidates` is the only reason
// that may say nothing matched, and it may only say so over coverage every
// applicable leg vouched for — available, error-free, untruncated, and
// CoverageComplete. The vector leg reports CoverageComplete=false on purpose
// (reconciling it costs two COUNT(*) scans, and a row with no embedding is
// invisible to its own scan), so a hybrid search over a corpus with unembedded
// rows cannot claim absence. The window note is the answer instead, because
// retrieval is windowed even when coverage is complete.
func TestAnAbsenceClaimNeedsCompleteCoverage(t *testing.T) {
	// A request with no item bound, no scope and no category: nothing the note
	// could advise is set, so the note must not advise anything.
	bare := hybridRequest()
	bare.Budget = Budget{MaxBytes: 40_000}
	hybrid := run(t, &fakeRetriever{set: hybridSet()}, bare)
	if strings.Contains(hybrid.Response, "No matching memories found") {
		t.Errorf("a hybrid search whose vector leg declines to vouch for its coverage still claims "+
			"the store has nothing:\n%s", hybrid.Response)
	}
	if !strings.Contains(hybrid.Response, "no match within the searched window") {
		t.Errorf("the answer does not carry the window note that replaces the absence claim:\n%s", hybrid.Response)
	}
	// The note's advice has to name a knob the request actually set. The shipped
	// tool path is the sharp case: ghost_memory_search always sends an item limit
	// (10 by default) and no scope filter, so the note every unfiltered caller
	// gets names the limit and must say NOTHING about a scope filter it never
	// passed. A budget with no MaxItems — the shape the first half of this test
	// used to build — produced a note with no advice at all, which passed while
	// covering a request the tool cannot send.
	shipped := hybridRequest()
	shipped.Budget = Budget{MaxItems: 10, MaxBytes: 40_000}
	tool := run(t, &fakeRetriever{set: hybridSet()}, shipped)
	if !strings.Contains(tool.Response, "widen the limit") {
		t.Errorf("the note drops the one advice a shipped search can act on:\n%s", tool.Response)
	}
	for _, unpassed := range []string{"drop the scope filter", "drop the category filter"} {
		if strings.Contains(tool.Response, unpassed) {
			t.Errorf("the note advises changing a filter the request never set (%q):\n%s",
				unpassed, tool.Response)
		}
	}
	// The separator is the sharp end of it: with one clause the note is two
	// sentences and a single dash between them, so a branch that also appends a
	// filter clause leaves "or" after the limit — and a substring check for the
	// filter's words would not see a dangling conjunction.
	if strings.Contains(tool.Response, "widen the limit or") {
		t.Errorf("the note offers a choice the request cannot act on:\n%s", tool.Response)
	}

	// An empty scope map is a filter the request did not set — and it is the
	// shape a JSON `{}` decodes to, so it is a shape a caller sends. The filter
	// caveat and the stage-3 rule both test len(scope) > 0; the note has to
	// agree with them or it advises dropping a filter nobody passed.
	blank := hybridRequest()
	blank.Scope = map[string]string{}
	blank.Budget = Budget{MaxItems: 10, MaxBytes: 40_000}
	unfiltered := run(t, &fakeRetriever{set: hybridSet()}, blank)
	if strings.Contains(unfiltered.Response, "drop the scope filter") {
		t.Errorf("the note advises dropping a scope filter the request set to nothing:\n%s", unfiltered.Response)
	}
	if !strings.Contains(unfiltered.Response, "widen the limit") {
		t.Errorf("precondition: this fixture did not produce a window note at all:\n%s", unfiltered.Response)
	}

	// A request that DID set them keeps the advice, because then it is the one
	// thing that can help.
	filtered := run(t, &fakeRetriever{set: hybridSet()}, func() Request {
		req := hybridRequest()
		req.Scope = map[string]string{"environment": "production"}
		return req
	}())
	if !strings.Contains(filtered.Response, "drop the scope filter") {
		t.Errorf("the window note drops the filter advice on a request that set a filter:\n%s", filtered.Response)
	}
	if hybrid.Reason != reasonNoCandidates {
		t.Errorf("reason = %q, want no_candidates: the fact that no candidate came back is still true", hybrid.Reason)
	}

	// A keyword-only request whose leg answered and was not truncated did search
	// everything the leg could see, so the absence sentence is earned.
	keyword := run(t, &fakeRetriever{set: ftsOnlySet()}, baseRequest())
	if !strings.Contains(keyword.Response, "No matching memories found") {
		t.Errorf("a keyword-only search over a complete, untruncated leg does not claim absence:\n%s", keyword.Response)
	}

	// The same leg, but cut off at the window. It is given CoverageComplete true
	// on purpose: the two are independent conditions, and a retriever that claims
	// completeness while stopping at the window makes a claim the assembler must
	// not take at face value. memory.Candidates happens to tie them together, so
	// nothing in the tree does this today — which is exactly why the rule is
	// pinned here rather than left to a reader to notice.
	truncated := ftsOnlySet()
	truncated.Legs["fts"] = memory.LegStatus{
		Applicable: true, Attempted: true, Available: true, Truncated: true, CoverageComplete: true,
	}
	cut := run(t, &fakeRetriever{set: truncated}, baseRequest())
	if strings.Contains(cut.Response, "No matching memories found") {
		t.Errorf("a truncated keyword leg still claims the store has nothing:\n%s", cut.Response)
	}
}

// TestAWeakFilteredAnswerKeepsTheWindowCaveat: the caveat says further matches
// may exist past the window, which is the one hint a caller needs when the
// result is short AND untrustworthy. It reached every non-empty filtered answer
// before the verdict landed, so a weak one losing it is a regression on exactly
// the outcome where the hint matters most.
func TestAWeakFilteredAnswerKeepsTheWindowCaveat(t *testing.T) {
	req := hybridRequest()
	req.Scope = map[string]string{"environment": "production"}

	res := run(t, &fakeRetriever{set: weakSet(weakScoredRow("A1"))}, req)

	if res.Outcome != OutcomeWeak {
		t.Fatalf("precondition: outcome = %q, want weak", res.Outcome)
	}
	if !strings.Contains(res.Response, "scope filter") {
		t.Errorf("a weak filtered answer lost the window caveat:\n%s", res.Response)
	}
}

// weakScoredRow is a row the keyword leg retrieved well outside Arm A's window,
// in a set whose vector leg answered.
func weakScoredRow(id string) memory.Candidate {
	c := ranked(id, 6, "the reconciliation ledger closes nightly")
	c.VectorRank = 0
	c.VectorScore = 0.44
	return c
}

// TestTheEmptySentenceNeverClaimsAUniformCause: the reason set is closed, so a
// set emptied by two stages carries one reason — and the sentence that leads the
// answer must not claim every row failed the same way while the note beneath it
// says otherwise. The copy and the note are asserted together, because their
// agreement is the property: neither is trustworthy alone.
func TestTheEmptySentenceNeverClaimsAUniformCause(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	rows := make([]memory.Candidate, 0, 4)
	for range 3 {
		row := ranked("GONE", 0, "the old policy")
		row.ValidUntil = &expired
		rows = append(rows, row)
	}
	rows = append(rows, ranked("KEPT", 0, strings.Repeat("live row ", 20)))
	req := baseRequest()
	// A one-byte slice cap cuts the live row without touching the expired ones,
	// so the breakdown has to name two stages while the reason names one.
	req.Budget = Budget{MaxItems: 10, Slices: []Slice{{Bucket: "proj", MaxBytes: 1}}}

	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)

	if res.Reason != reasonAllInvalid {
		t.Fatalf("reason = %q, want all_invalid: three of the four rows went to validity", res.Reason)
	}
	for _, uniform := range []string{"Every candidate", "All candidates", "every candidate"} {
		if strings.Contains(res.Abstention, uniform) {
			t.Errorf("the answer claims a uniform cause (%q) while the note names two stages: %q",
				uniform, res.Abstention)
		}
	}
	if !strings.Contains(res.Abstention, "out of date") {
		t.Errorf("the answer does not say what happened to the rows: %q", res.Abstention)
	}
	if !hasNote(res.Notes, "validity 3, budget 1") {
		t.Errorf("the answer omits the per-stage breakdown that makes the sentence checkable: %v", res.Notes)
	}
	if !strings.Contains(res.Response, "(Note: ") {
		t.Errorf("the rendered answer does not carry the notes at all:\n%s", res.Response)
	}
}

// TestARowlessAnswerNeverClaimsMemoriesWereWithheld: two reasons are reachable
// only when retrieval returned NO row at all — the leg that failed, and the leg
// that never ran. "No sufficiently trustworthy memory found" is the EXCLUSION
// wording: it says rows were found and then withheld, which is false of both, and
// on the ordinary no-embedder machine it is the sentence a caller meets most often
// after a miss. The incompleteness framing is the honest one, and the two row-less
// reasons read alike so a caller does not have to learn a third shape.
func TestARowlessAnswerNeverClaimsMemoriesWereWithheld(t *testing.T) {
	const exclusion = "No sufficiently trustworthy memory found"
	for _, tc := range []struct {
		name string
		leg  memory.LegStatus
	}{
		{"the leg that never ran", memory.LegStatus{Applicable: true, Attempted: false}},
		{"the leg that ran and broke", memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}},
	} {
		set := ftsOnlySet()
		set.Legs["vector"] = tc.leg

		res := run(t, &fakeRetriever{set: set}, hybridRequest())

		if len(res.Items) != 0 {
			t.Fatalf("%s: precondition: the fixture admitted %d rows, so it is not a row-less answer",
				tc.name, len(res.Items))
		}
		if strings.Contains(res.Abstention, exclusion) {
			t.Errorf("%s: a search that found nothing answers with the exclusion wording, which claims "+
				"memories were withheld: %q", tc.name, res.Abstention)
		}
		if !strings.Contains(res.Abstention, "incomplete") {
			t.Errorf("%s: the answer does not say the search was incomplete, so the reader cannot tell "+
				"a degraded retrieval from an empty store: %q", tc.name, res.Abstention)
		}
	}
}

// TestTheSeamItselfRefusesAnUnusableCosine: config refuses NaN, Inf, a negative
// and anything above 1 on both of ITS paths, and then the seam accepted all four
// — which is the wrong place for the only guard. Run is the package's exported
// entry point, and the two silent failures are the ones config's refusal exists
// to prevent: a negative reads as OFF and the line tells a user who set a floor
// that they have none, and anything above 1 arms a threshold no row's cosine can
// reach, so every result outside the keyword arm comes back weak with nothing on
// the line to tell it from a measured verdict.
func TestTheSeamItselfRefusesAnUnusableCosine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float32
	}{
		{"negative", -0.5},
		{"above one", 1.5},
		{"infinite", float32(math.Inf(1))},
		{"negative infinite", float32(math.Inf(-1))},
		// NaN is the one a YAML file can produce and no comparison catches by
		// accident: it compares false against 0, so it arms nothing and reads
		// as OFF — a floor the user set that silently does not exist.
		{"NaN", float32(math.NaN())},
	} {
		set := hybridSet(vectorScored("A1", 0, 0.9, "database configuration pooling"))
		_, err := Run(context.Background(), &fakeRetriever{set: set}, func() Request {
			r := hybridRequest()
			r.AbstainCosine = tc.value
			return r
		}())
		if err == nil {
			t.Errorf("%s cosine: Run accepted a value config.cosineValue refuses", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "AbstainCosine") {
			t.Errorf("%s cosine: the error does not name the field to fix: %v", tc.name, err)
		}
	}
	// Zero and one are not refusals: 0 is the shipped OFF and 1 is the largest
	// cosine there is, which a confident row clears.
	for _, ok := range []float32{0, 1} {
		set := hybridSet(vectorScored("A1", 0, 0.9, "database configuration pooling"))
		_, err := Run(context.Background(), &fakeRetriever{set: set}, func() Request {
			r := hybridRequest()
			r.AbstainCosine = ok
			return r
		}())
		if err != nil {
			t.Errorf("cosine %v: Run refused a value in range: %v", ok, err)
		}
	}
}

// TestNoEmptySentenceClaimsEveryRowFailedTheSameWay: the rule is about the
// WORDS, and it binds every reason, not just the one the other test happens to
// build. `dominantRemoval` picks the stage that removed the MOST rows, so a set
// two stages emptied gets one reason and a note naming both — and a sentence
// saying "every candidate" is then contradicted by the note directly beneath it.
// The caller reads the sentence and skips the note, so the sentence has to be
// the one that cannot be wrong.
func TestNoEmptySentenceClaimsEveryRowFailedTheSameWay(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	for _, tc := range []struct {
		name   string
		set    *memory.CandidateSet
		req    Request
		reason string
		note   string
	}{
		{
			name: "category with an expired row",
			set: func() *memory.CandidateSet {
				// Both surviving-category rows are wrong-category on purpose: the
				// point is that TWO stages share the removal, so one more wrong
				// row than the expired one is what tips dominance to predicates.
				rows := make([]memory.Candidate, 0, 3)
				for _, id := range []string{"A1", "A2"} {
					row := ranked(id, 0, "the wrong category")
					row.Category = "gotcha"
					rows = append(rows, row)
				}
				gone := ranked("A3", 0, "an expired row")
				gone.ValidUntil = &expired
				return ftsOnlySet(append(rows, gone)...)
			}(),
			req: func() Request {
				r := baseRequest()
				r.Category = "fact"
				return r
			}(),
			reason: reasonAllOutOfCategory,
			note:   "validity 1",
		},
		{
			name: "scope with an expired row",
			set: func() *memory.CandidateSet {
				rows := []memory.Candidate{
					scopedCandidate("A1", map[string]string{"environment": "development"}, 0.9),
					scopedCandidate("A2", map[string]string{"environment": "staging"}, 0.8),
				}
				gone := ranked("A3", 0, "an expired row")
				gone.ValidUntil = &expired
				return ftsOnlySet(append(rows, gone)...)
			}(),
			req: func() Request {
				r := baseRequest()
				r.Scope = map[string]string{"environment": "production"}
				return r
			}(),
			reason: reasonAllOutOfScope,
			note:   "validity 1",
		},
		{
			name: "item budget with an expired row",
			set: func() *memory.CandidateSet {
				rows := []memory.Candidate{
					ranked("A1", 0, strings.Repeat("live row ", 20)),
					ranked("A2", 0, strings.Repeat("live row ", 20)),
				}
				gone := ranked("A3", 0, "an expired row")
				gone.ValidUntil = &expired
				return ftsOnlySet(append(rows, gone)...)
			}(),
			req: func() Request {
				r := baseRequest()
				r.Budget = Budget{MaxItems: 10, Slices: []Slice{{Bucket: "proj", MaxBytes: 30}}}
				return r
			}(),
			reason: reasonAllOverBudget,
			note:   "validity 1",
		},
		{
			name: "validity with a wrong-category row",
			set: func() *memory.CandidateSet {
				// The mirror image of the first case: here validity removes the
				// most, so the reason names IT, and the category row proves a
				// second stage shares the removal. A sentence that pointed at the
				// breakdown only for the three reasons whose stage ran third
				// would leave this one the sole unchecked claim.
				rows := make([]memory.Candidate, 0, 3)
				for _, id := range []string{"A1", "A2"} {
					row := ranked(id, 0, "an expired row")
					row.ValidUntil = &expired
					rows = append(rows, row)
				}
				wrong := ranked("A3", 0, "the wrong category")
				wrong.Category = "gotcha"
				return ftsOnlySet(append(rows, wrong)...)
			}(),
			req: func() Request {
				r := baseRequest()
				r.Category = "fact"
				return r
			}(),
			reason: reasonAllInvalid,
			note:   "predicates 1",
		},
	} {
		res := run(t, &fakeRetriever{set: tc.set}, tc.req)
		if res.Reason != tc.reason {
			t.Errorf("%s: reason = %q, want %q (precondition: two stages must share the work)",
				tc.name, res.Reason, tc.reason)
			continue
		}
		if !hasNote(res.Notes, tc.note) {
			t.Errorf("%s: the breakdown does not name the second stage: %v", tc.name, res.Notes)
			continue
		}
		if !strings.Contains(res.Abstention, perStageNote) {
			t.Errorf("%s: the sentence names one stage while the note names two, and does not say so: %q",
				tc.name, res.Abstention)
		}
		for _, uniform := range []string{"every candidate", "Every candidate", "all candidates", "All candidates"} {
			if strings.Contains(res.Abstention, uniform) {
				t.Errorf("%s: the sentence claims a uniform cause (%q) while the note names two stages: %q",
					tc.name, uniform, res.Abstention)
			}
		}
	}
}

// TestAFilterReasonAndTheFilterCaveatDoNotContradictEachOther: a category or
// scope exclusion gets a sentence naming what excluded the rows AND the caveat
// naming the filter with a next step. The two cover different things — why the
// rows are gone and what to do about it — so neither may be missing, and
// neither may claim the store had no such memory.
func TestAFilterReasonAndTheFilterCaveatDoNotContradictEachOther(t *testing.T) {
	other := scopedCandidate("A1", map[string]string{"environment": "development"}, 0.9)
	req := baseRequest()
	req.Scope = map[string]string{"environment": "production"}

	res := run(t, &fakeRetriever{set: ftsOnlySet(other)}, req)

	if res.Reason != reasonAllOutOfScope {
		t.Fatalf("reason = %q, want all_out_of_scope", res.Reason)
	}
	if !strings.Contains(strings.ToLower(res.Abstention), "no sufficiently trustworthy memory found") {
		t.Errorf("the sentence does not say the rows were found and withheld: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, "scope") {
		t.Errorf("the sentence does not name the filter that emptied the set: %q", res.Abstention)
	}
	if !strings.Contains(res.Response, "scope filter") {
		t.Errorf("the rendered answer carries no caveat naming the filter and a next step:\n%s", res.Response)
	}
	if strings.Contains(res.Response, "No matching memories found") {
		t.Errorf("an excluded set is reported as an absent one:\n%s", res.Response)
	}
}

// TestTheTraceCarriesTheBoundedNotes: the explain projection reads the notes
// from the trace, so the fit pass's re-derivation has to land there too or an
// explanation would show notes the answer dropped.
func TestTheTraceCarriesTheBoundedNotes(t *testing.T) {
	set := ftsOnlySet()
	set.Legs["fts"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "no such table"}

	res := run(t, &fakeRetriever{set: set}, baseRequest())

	if !hasNote(res.Trace.Notes, "no such table") {
		t.Errorf("trace notes %v do not carry what Result.Notes carries: %+v", res.Trace.Notes, res.Notes)
	}
}

// TestARequestWithNoArmAvailableIsNotWeak: a vector-only request has no keyword
// leg, so every row keeps the -1 "the keyword leg never retrieved this" sentinel
// and arm A can never fire on any of them. With arm B at its shipped default
// (off) there is then no arm able to judge the result at all — and reporting
// `weak` would be a claim against a threshold nobody configured, which is the
// same mistake as blaming an embedder outage for a weak keyword hit.
func TestARequestWithNoArmAvailableIsNotWeak(t *testing.T) {
	set := hybridSet(ranked("A1", -1, "the reconciliation ledger closes nightly"))
	delayed := set
	delayed.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: false},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}
	req := hybridRequest()
	req.Condition = CondVectorOnly
	req.Scope = nil

	res := run(t, &fakeRetriever{set: delayed}, req)

	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: neither arm could judge this request", res.Outcome)
	}
	if res.Reason != reasonNoFloorArm {
		t.Errorf("reason = %q, want no_floor_arm: the reason has to say the floor was never applied", res.Reason)
	}
	if strings.Contains(res.Abstention, "do not rely") {
		t.Errorf("a result no arm could judge carries the weak warning: %q", res.Abstention)
	}

	// With arm B configured there IS an arm, and it decides.
	req.AbstainCosine = 0.5
	judged := run(t, &fakeRetriever{set: delayed}, req)
	if judged.Outcome != OutcomeWeak || judged.Reason != reasonBelowFloor {
		t.Errorf("outcome/reason = %q/%q, want weak/below_floor: the configured cosine is now the "+
			"only arm and this row is under it", judged.Outcome, judged.Reason)
	}
}

// TestTheWindowCaveatIsNotBlamedForAByteCapTrim: the caveat says further
// matches may exist past the window and tells the caller to raise the limit. A
// response-fit trim is not a window — it removes rows the window admitted, and
// raising the limit admits MORE of them, so the answer would advise the one
// action that cannot help, contradicting the tool's own budget error.
func TestTheWindowCaveatIsNotBlamedForAByteCapTrim(t *testing.T) {
	body := strings.Repeat("deployment runbook step ", 40)
	set := ftsOnlySet(
		ranked("A1", 0, body),
		ranked("A2", 1, body),
		ranked("A3", 2, body),
	)
	req := baseRequest()
	req.Budget.MaxItems = 5
	req.Category = "fact"

	// The cap fits one row and not two, measured from the one-row render so the
	// fixture's size does not decide the boundary.
	one := run(t, &fakeRetriever{set: ftsOnlySet(ranked("A1", 0, body))}, req)
	req.Budget.MaxBytes = one.Bytes
	res := run(t, &fakeRetriever{set: set}, req)

	if len(res.Items) != 1 {
		t.Fatalf("precondition: %d items, want 1 (the cap must trim)", len(res.Items))
	}
	if strings.Contains(res.Response, "finite search window") {
		t.Errorf("a byte-cap trim renders the window caveat, which blames the window and advises "+
			"raising a limit that makes the response larger:\n%s", res.Response)
	}
	// And the line still says how many rows the caller got, so the shortfall is
	// visible with no caveat at all.
	if !strings.Contains(res.Machine, "admitted=1") {
		t.Errorf("machine line = %q, want the admitted count", res.Machine)
	}
}

// TestTheWindowCaveatStillNamesAWideShortFilteredAnswer: the other half. A
// windowed search that came back short on its own has no other explanation, and
// the caveat is the only thing that says so.
func TestTheWindowCaveatStillNamesAWideShortFilteredAnswer(t *testing.T) {
	set := ftsOnlySet(ranked("A1", 0, "a row of text"))
	req := baseRequest()
	req.Budget.MaxItems = 5
	req.Category = "fact"

	res := run(t, &fakeRetriever{set: set}, req)

	if !strings.Contains(res.Response, "finite search window") {
		t.Errorf("a short filtered answer with no trim lost the window caveat:\n%s", res.Response)
	}
	if !strings.Contains(res.Response, "raise the limit") {
		t.Errorf("the caveat does not name the action that can help here:\n%s", res.Response)
	}
}

// TestTheMachineLineDoesNotPrintAThresholdItNeverApplied: `abstain_cosine` on the
// verdict line is a statement about the thresholds that PRODUCED the verdict. A
// user who sets context.abstain_cosine on a machine with no working embedder gets
// a line reading `abstain_cosine=0.620` beside a verdict that never compared it and
// `legs=vector:not_run`, which reads as though the cosine cleared a row it was
// never compared against.
func TestTheMachineLineDoesNotPrintAThresholdItNeverApplied(t *testing.T) {
	notRun := ftsOnlySet(ranked("A1", 0, "database configuration pooling"))
	notRun.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: false}
	req := baseRequest()
	req.AbstainCosine = 0.62

	res := run(t, &fakeRetriever{set: notRun}, req)

	// A rank-0 keyword hit clears arm A whatever the vector leg did, so this is
	// floor_met now — the point of the assertions below is the LINE, not the
	// reason.
	if res.Outcome != OutcomeAnswerable || res.Reason != reasonFloorMet {
		t.Fatalf("precondition: outcome/reason = %q/%q, want answerable/floor_met", res.Outcome, res.Reason)
	}
	if strings.Contains(res.Machine, "0.620") {
		t.Errorf("machine line = %q, prints a cosine the run never compared a row against", res.Machine)
	}
	if !strings.Contains(res.Machine, "abstain_cosine=not_applied") {
		t.Errorf("machine line = %q, want a configured-but-unused arm distinguishable from an "+
			"unconfigured one", res.Machine)
	}
	if !res.Trace.Floors.VectorArmOn {
		t.Error("VectorArmOn is false, so the trace would report the request as unconfigured when it was not")
	}
	if res.Trace.Floors.VectorApplied {
		t.Error("VectorApplied is true for a run whose vector leg never executed")
	}

	// The three states stay distinct: unconfigured, configured-and-unused, and
	// applied. Collapsing the first two tells a reader who set the key that it did
	// nothing; collapsing the last into the first tells a reader who did not that
	// a threshold exists.
	if line := run(t, &fakeRetriever{set: ftsOnlySet(ranked("A1", 0, "x"))}, baseRequest()); !strings.Contains(line.Machine, "abstain_cosine=off") {
		t.Errorf("an unconfigured arm renders %q, want abstain_cosine=off", line.Machine)
	}
	appliedReq := hybridRequest()
	appliedReq.AbstainCosine = 0.62
	applied := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 0, "x"))}, appliedReq)
	if !strings.Contains(applied.Machine, "abstain_cosine=0.620") {
		t.Errorf("an applied arm renders %q, want the threshold", applied.Machine)
	}
	if !applied.Trace.Floors.VectorApplied {
		t.Error("VectorApplied is false for a run whose vector leg answered")
	}
}

// TestAFitPassThatEmptiesTheBlockClearsTheArmsState: the pass re-renders after
// every drop, and `verdict()` returns for the empty case before it recomputes the
// arms. So `FTSApplied` kept whatever the last non-empty iteration left in it, and
// a block that ended empty printed `floor_fts_rank=3` beside a verdict that
// compared no row — the exact hole the field was added to close, reopened by the
// pass that makes the empty case reachable.
func TestAFitPassThatEmptiesTheBlockClearsTheArmsState(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	gone := ranked("A2", 0, "an expired row")
	gone.ValidUntil = &expired
	set := hybridSet(ranked("A1", 0, strings.Repeat("first row content ", 120)), gone)

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)
	_, res := roomiestEmptyCap(t, set, req, full)

	if len(res.Items) != 0 {
		t.Fatalf("precondition: the pass admitted %d rows, so this is not the empty case", len(res.Items))
	}
	if strings.Contains(res.Machine, "floor_fts_rank=3") {
		t.Errorf("machine line = %q, which carries the keyword threshold of an earlier iteration next "+
			"to a verdict that compared no row", res.Machine)
	}
	if !strings.Contains(res.Machine, "floor_fts_rank=not_applied") {
		t.Errorf("machine line = %q, want the unapplied state on an answer with no rows", res.Machine)
	}
}

// TestTheKeywordArmRendersNotAppliedWhenItHadNoValue: the cosine got a third
// state for exactly this reason — a threshold printed next to a verdict it did
// not produce invites a reader to believe the judgement came from it — and the
// keyword arm had the same hole until `FTSApplied` gave it the same treatment. A
// retriever that ranked nothing leaves the -1 sentinel on every row, so no
// keyword comparison happened at all.
func TestTheKeywordArmRendersNotAppliedWhenItHadNoValue(t *testing.T) {
	// A hybrid search whose keyword leg answered and retrieved nothing: the
	// ordinary shape of a semantic query sharing no words with the corpus.
	set := hybridSet(ranked("A1", -1, "a paraphrase with no shared vocabulary"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeAnswerable || res.Reason != reasonNoFloorArm {
		t.Fatalf("outcome/reason = %q/%q, want answerable/no_floor_arm: no arm held a value, so "+
			"below_floor would be a claim against a threshold nobody applied", res.Outcome, res.Reason)
	}
	if strings.Contains(res.Machine, "floor_fts_rank=3") {
		t.Errorf("machine line = %q, which asserts a keyword floor of 3 next to a verdict no keyword "+
			"comparison produced", res.Machine)
	}
	if !strings.Contains(res.Machine, "floor_fts_rank=not_applied") {
		t.Errorf("machine line = %q, want the keyword arm's unapplied state, the counterpart of "+
			"abstain_cosine=not_applied", res.Machine)
	}
	if res.Trace.Floors.FTSApplied {
		t.Error("FTSApplied is true for a result whose rows carry no keyword rank")
	}
	// And where a rank DID reach a row the threshold prints, because there the
	// judgement is the arm's own.
	judged := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 4, "a paraphrase"))}, hybridRequest())
	if !strings.Contains(judged.Machine, "floor_fts_rank=3") {
		t.Errorf("machine line = %q, want the keyword threshold where the arm judged", judged.Machine)
	}
	if !judged.Trace.Floors.FTSApplied {
		t.Error("FTSApplied is false for a result whose row carries a keyword rank")
	}
}

// TestTheCosineArmIgnoresTheSentinelToo: the mirror of the keyword arm's rule. A
// row the vector leg did not retrieve keeps VectorScore at the -1 sentinel, and
// `0 >= 0.6` is false for a reason that has nothing to do with similarity — so a
// configured arm would report `below_floor` for a row it never measured, with the
// configured number printed beside it as though it had been compared.
func TestTheCosineArmIgnoresTheSentinelToo(t *testing.T) {
	set := hybridSet(vectorScored("A1", -1, -1, "a row the vector leg never retrieved"))
	req := hybridRequest()
	req.AbstainCosine = 0.6

	res := run(t, &fakeRetriever{set: set}, req)

	if res.Outcome != OutcomeAnswerable || res.Reason != reasonNoFloorArm {
		t.Errorf("outcome/reason = %q/%q, want answerable/no_floor_arm: the -1 score is a sentinel, "+
			"not a measurement that failed a threshold", res.Outcome, res.Reason)
	}
	// A real score below the floor is the opposite case and stays weak: the
	// sentinel rule must not become "arm B never judges anything".
	scored := run(t, &fakeRetriever{set: hybridSet(vectorScored("A1", -1, 0.2, "a weak match"))}, req)
	if scored.Outcome != OutcomeWeak || scored.Reason != reasonBelowFloor {
		t.Errorf("outcome/reason = %q/%q, want weak/below_floor: a measured 0.2 below a 0.6 floor is "+
			"exactly what the arm is for", scored.Outcome, scored.Reason)
	}
	// The mixed set: one measured row, one the leg never retrieved. The arm holds a
	// value, so the result is judged, and the verdict rests on the measurement.
	mixed := run(t, &fakeRetriever{set: hybridSet(
		vectorScored("A1", -1, 0.2, "a weak match"),
		vectorScored("A2", -1, -1, "a row the vector leg never retrieved"),
	)}, req)
	if mixed.Outcome != OutcomeWeak || mixed.Reason != reasonBelowFloor {
		t.Errorf("outcome/reason = %q/%q, want weak/below_floor: one measured row is enough for the arm "+
			"to judge, and the sentinel row is not what decided it", mixed.Outcome, mixed.Reason)
	}
}

// TestANegativeVectorScoreIsAValueNotAnAbsence: presence in the vector arm is
// decided by the exact -1 mark the retriever leaves on a row the leg did not
// retrieve, and never by the sign of the score. A cosine is in [-1, 1], so one
// that came back negative was MEASURED and is a very weak match; counting it as
// "no vector value" made a block whose every row scored below zero report
// `no_floor_arm` — the reason that says no arm held a value to compare — when
// the arm was holding one and judging it. That is the difference between "the
// corpus was judged and found wanting" and "nothing here was measured at all",
// and a caller told the second would go looking for an embedder.
func TestANegativeVectorScoreIsAValueNotAnAbsence(t *testing.T) {
	req := hybridRequest()
	req.AbstainCosine = 0.6

	res := run(t, &fakeRetriever{set: hybridSet(
		vectorScored("A1", -1, -0.85, "a match the vector leg measured as its opposite"),
		vectorScored("A2", -1, -0.10, "a match the vector leg measured as near-orthogonal"),
	)}, req)

	if res.Outcome != OutcomeWeak || res.Reason != reasonBelowFloor {
		t.Fatalf("outcome/reason = %q/%q, want weak/below_floor: the vector leg measured both rows "+
			"and neither cleared the 0.6 floor, which is a verdict about the corpus rather than "+
			"an arm that held nothing", res.Outcome, res.Reason)
	}
	// The arm is the one that judged, so its threshold is on the line: a vector
	// arm read as absent renders `not_applied` and would send the reader after
	// an embedder.
	if !strings.Contains(res.Machine, "abstain_cosine=0.600") {
		t.Errorf("machine line = %q, want the configured cosine printed: the arm ran and held a "+
			"value to compare", res.Machine)
	}
	if !res.Trace.Floors.VectorApplied {
		t.Error("VectorApplied is false for a result whose rows all carry a measured cosine")
	}
	// The keyword arm had no value on this set, so the verdict is the vector
	// arm's alone and the line has to say the keyword one did not apply.
	if !strings.Contains(res.Machine, "floor_fts_rank=not_applied") {
		t.Errorf("machine line = %q, want the keyword arm's unapplied state beside it", res.Machine)
	}
	// And the -1 mark is still what absence looks like, so the boundary is
	// local: only the exact sentinel, never a low score, means "not retrieved".
	absent := run(t, &fakeRetriever{set: hybridSet(
		vectorScored("A1", -1, -0.85, "a match the vector leg measured as its opposite"),
		vectorScored("A2", -1, -1, "a row the vector leg never retrieved"),
	)}, req)
	if absent.Outcome != OutcomeWeak || absent.Reason != reasonBelowFloor {
		t.Errorf("outcome/reason = %q/%q, want weak/below_floor: one measured row is enough for the "+
			"arm to hold a value", absent.Outcome, absent.Reason)
	}
	none := run(t, &fakeRetriever{set: hybridSet(
		vectorScored("A1", -1, -1, "a row the vector leg never retrieved"),
		vectorScored("A2", -1, -1, "another row the vector leg never retrieved"),
	)}, req)
	if none.Outcome != OutcomeAnswerable || none.Reason != reasonNoFloorArm {
		t.Errorf("outcome/reason = %q/%q, want answerable/no_floor_arm: the -1 sentinel is still "+
			"the mark for a leg that did not retrieve the row", none.Outcome, none.Reason)
	}
}

// TestAFailedVectorLegIsNotAConfiguration: the two halves of one rule must not
// disagree on the same shape. "No floor verdict was possible" is either a
// configuration state (no arm had a value) or an incident (a retrieval broke),
// and a vector leg that RAN and FAILED is the second. Reporting it as the first
// would tell an operator their machine has no embedder when one answered and the
// search broke.
func TestAFailedVectorLegIsNotAConfiguration(t *testing.T) {
	set := hybridSet(ranked("A1", 0, "database configuration pooling"))
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Reason != reasonRetrievalPartial {
		t.Errorf("reason = %q, want retrieval_partial: a leg that ran and failed is an incident, "+
			"not a machine with no embedder", res.Reason)
	}
	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: the keyword row is still an answer", res.Outcome)
	}
	if !strings.Contains(res.Response, "one retrieval leg failed") {
		t.Errorf("the answer does not name the failure:\n%s", res.Response)
	}
}

// stageTrace returns the named stage's record, failing the test when the stage
// was never recorded.
func stageTrace(t *testing.T, res Result, name string) StageTrace {
	t.Helper()
	for _, st := range res.Trace.Stages {
		if st.Stage == name {
			return st
		}
	}
	t.Fatalf("the trace records no %q stage: %+v", name, res.Trace.Stages)
	return StageTrace{}
}

// hasDecision reports whether the trace explains one row's fate at one stage.
func hasDecision(res Result, id, stage, reason string) bool {
	for _, d := range res.Trace.Decisions {
		if d.ID == id && d.Stage == stage && d.Reason == reason && !d.Kept {
			return true
		}
	}
	return false
}

func stringPtr(s string) *string { return &s }
