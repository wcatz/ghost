package reflection

import (
	"context"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRewriteLapsesWhenAFilterRemovesTheReplacement drives the whole tier. The
// rewrite passes the grounding check and the operations are resolved, so the
// source is exempt from the drop guard on the strength of a replacement text —
// and then dropForeignProjectMemories runs and removes that text, because it
// names a project the input corpus never mentioned. Nothing re-checks the claim
// afterwards, so the source row is disposed of with no replacement in the corpus
// and nothing for --allow-drops to act on: a silent deletion, the same shape as
// the supersession one, and for the same reason.
func TestRewriteLapsesWhenAFilterRemovesTheReplacement(t *testing.T) {
	const (
		source = "the metrics endpoint is served by the ingest service"
		reword = "the metrics endpoint is served by the dingo sidecar"
	)
	in := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{ID: seqID(1), Category: "fact", Content: source},
			{ID: seqID(2), Category: "fact", Content: "region fsn1 fronts every ingest worker"},
		},
	}
	llm := NewLlmConsolidator(&fakeReflector{reply: fmt.Sprintf(
		`{"ops":["rewrite %s -> %s","keep %s"]}`, seqID(1), reword, seqID(2))})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if _, ok := findMemory(result, reword); ok {
		t.Fatalf("the contaminating rewrite survived the filter: %+v", result.Memories)
	}
	if len(result.Memories) != 1 || result.Memories[0].Content != in.ExistingMemories[1].Content {
		t.Fatalf("want only the passed-through row left: %+v", result.Memories)
	}

	// The source has to come back, and it has to come back through the guard, since
	// the tier no longer emits anything for an id it resolved.
	drops := AuditGuardedDrops(in, result)
	if len(drops) != 1 || drops[0].Content != source {
		t.Fatalf("a rewrite outlived its replacement and the source row went with it: %+v", drops)
	}
}

// TestRewriteIsExemptOnlyWhileItsReplacementSurvives is the unit half of the same
// rule, stated both ways so a change to either side is caught here.
func TestRewriteIsExemptOnlyWhileItsReplacementSurvives(t *testing.T) {
	const (
		old = "the metrics endpoint is served by the ingest service"
		new = "the metrics endpoint is served by the billing service"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "fact", Content: old},
		{ID: seqID(2), Category: "fact", Content: "region fsn1 fronts every ingest worker"},
	}}
	reply := fmt.Sprintf(`{"ops":["rewrite %s -> %s","keep %s"]}`, seqID(1), new, seqID(2))
	result := opRun(t, in, reply)

	if _, ok := findMemory(result, new); !ok {
		t.Fatalf("the rewrite was not applied: %+v", result.Memories)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Errorf("a rewrite with its replacement present was flagged: %+v", drops)
	}

	// The same result with the replacement taken away — which is what a post-filter
	// does — must put the source back, because nothing in the corpus carries the
	// claim any more. The untouched row stays, so the source is the only thing the
	// guard can be missing.
	filtered := result
	filtered.Memories = []ReflectMemory{{Category: "fact", Content: in.ExistingMemories[1].Content}}
	drops := AuditGuardedDrops(in, filtered)
	if len(drops) != 1 || drops[0].Content != old {
		t.Fatalf("want the source re-added once its replacement is gone, got %+v", drops)
	}
}

// TestSupersessionAndRewriteShareOneExemption pins the unification: a rewrite and
// a supersession are the same claim about the same thing — an input id the
// response disposed of, and the text that stands in its place — so they share one
// list and one witness check. Two code paths for one rule is how the rewrite came
// to be trusted unconditionally while the supersession was not.
func TestSupersessionAndRewriteShareOneExemption(t *testing.T) {
	const (
		old = "bastion SSH uses port 2222 with a hardware key"
		new = "bastion SSH uses port 2222 with the hardware key fob"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: old},
		{ID: seqID(2), Category: "gotcha", Content: new},
	}}
	result := opRun(t, in, fmt.Sprintf(
		`{"ops":["rewrite %s -> %s","drop %s reason: superseded by %s"]}`, seqID(1), new, seqID(2), seqID(1)))

	if len(result.Replacements) != 2 {
		t.Fatalf("replacements = %+v, want the rewrite and the supersession in one list", result.Replacements)
	}
	byID := map[string]string{}
	for _, r := range result.Replacements {
		byID[memIDKey(r.ID)] = r.Text
	}
	if byID[memIDKey(seqID(1))] != new {
		t.Errorf("the rewrite's witness = %q, want the rewritten text %q", byID[memIDKey(seqID(1))], new)
	}
	if byID[memIDKey(seqID(2))] != new {
		t.Errorf("the supersession's witness = %q, want the successor's text %q", byID[memIDKey(seqID(2))], new)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Errorf("both rows are witnessed as replaced, yet %d were flagged: %+v", len(drops), drops)
	}

	// One witness check for both: take the replacing text away and both rows go
	// back under the audit.
	filtered := result
	filtered.Memories = nil
	if drops := AuditGuardedDrops(in, filtered); len(drops) != 2 {
		t.Fatalf("want both rows audited once the witness is gone, got %+v", drops)
	}
}

// TestReplacementWithNoWitnessIsNotAnExemption covers the degenerate case: a
// claim with no text to check. executeOps only records a rewrite it actually
// applied, so this cannot arise from the tier — but a result carrying one must
// not be read as "this row is definitely gone".
func TestReplacementWithNoWitnessIsNotAnExemption(t *testing.T) {
	const old = "the ledger ingests through the bastion on port 2222"
	in := ReflectionInput{ExistingMemories: []memory.Memory{{ID: seqID(1), Category: "fact", Content: old}}}
	result := ReflectionResult{Replacements: []Replacement{{ID: seqID(1)}}}

	drops := AuditGuardedDrops(in, result)
	if len(drops) != 1 || drops[0].Content != old {
		t.Fatalf("a witness-less replacement disposed of the row: %+v", drops)
	}
}

// TestRewriteOfASourceAFilterKeptIsStillExempt guards the fix from over-reaching:
// the common case must not start re-adding rewritten rows, or every rewrite
// becomes a duplicate.
func TestRewriteOfASourceAFilterKeptIsStillExempt(t *testing.T) {
	const (
		old = "the metrics endpoint is served by the ingest service"
		new = "the metrics endpoint is served by the billing service"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "fact", Content: old},
		{ID: seqID(2), Category: "fact", Content: "region fsn1 fronts every ingest worker"},
	}}
	written, drops := passThroughResult(t, in, fmt.Sprintf(
		`{"ops":["rewrite %s -> %s","keep %s"]}`, seqID(1), new, seqID(2)))

	if !written[new] {
		t.Fatalf("the rewritten text is not in the corpus: %v", written)
	}
	if written[old] {
		t.Errorf("the rewritten row was re-added beside its replacement: %v", written)
	}
	if len(drops) != 0 {
		t.Errorf("a rewrite whose replacement survived was flagged: %+v", drops)
	}
}

// TestMergeLapsesWhenAFilterRemovesIt is the same failure as the rewrite's, one
// step further out. A merge's sources take the union branch on the strength of
// the merge, and once the merge is gone the union is the rest of the project —
// the pass-through emits every unclaimed input — so a source whose words recur
// across two unrelated survivors passes 45% containment against a vocabulary that
// has nothing to do with it, and is neither re-added nor reported: two rows
// deleted with nothing replacing them, silently.
//
// The fixture splits the two sources' tokens across two pass-throughs, each
// holding well under the bar on its own: no single output explains either source,
// and only the union does. That is the shape the union branch exists for, which is
// why the mutation it guards against is invisible without it.
func TestMergeLapsesWhenAFilterRemovesIt(t *testing.T) {
	const (
		ingest = "the ledger ingests batches through the bastion relay on port 2222 every night"
		export = "the ledger exports batches through the bastion relay on port 2222"
		ping   = "the bastion relay answers ping on 443 from the office"
		purge  = "batches are purged after they pass through the ledger"
	)
	in := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{ID: seqID(1), Category: "gotcha", Content: ingest},
			{ID: seqID(2), Category: "fact", Content: export},
			{ID: seqID(3), Category: "gotcha", Content: ping},
			{ID: seqID(4), Category: "fact", Content: purge},
		},
	}
	// The merge text names a project the input corpus never mentions, so the
	// contamination filter removes it — after the ids are recorded as merged. The
	// two sources it folded in are then scored against the ping note and the purge
	// note, which together explain them and neither does alone.
	const merged = "the ledger ingests and exports batches through the dingo bastion relay on port 2222"
	llm := NewLlmConsolidator(&fakeReflector{reply: fmt.Sprintf(
		`{"ops":["merge %s,%s -> %s"]}`, seqID(1), seqID(2), merged)})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if _, ok := findMemory(result, merged); ok {
		t.Fatalf("the contaminating merge survived the filter: %+v", result.Memories)
	}
	if len(result.Memories) != 2 {
		t.Fatalf("want the two passed-through rows left, got %+v", result.Memories)
	}

	// No output explains either source, so with the merge gone both have to come
	// back. The merge-text witness is what is being tested here: a filter can
	// remove the merge after the ids are recorded, and a source still claiming a
	// merge that is not there has nothing left to be scored against. The
	// surviving-merge case is pinned separately, by
	// TestMergedSourceIsStillScoredAgainstItsOwnMerge, so this cannot be satisfied
	// by refusing to record merges at all.
	drops := AuditGuardedDrops(in, result)
	for _, want := range []string{ingest, export} {
		if !auditContains(drops, want) {
			t.Errorf("source %q was silently deleted by a merge that is not there: %+v", want, drops)
		}
	}
}

// TestMergedSourceIsStillScoredAgainstItsOwnMerge guards the fix from
// over-reaching: a merge that survived must still absorb its sources, or a
// source whose substance the merge carried comes back beside the merge that
// absorbed it. Scoring is against the merge's own text (#549), so the merge text
// has to actually carry each source — that is the whole witness available, since
// a merge source is consumed by its merge and its own text is never in the
// result.
func TestMergedSourceIsStillScoredAgainstItsOwnMerge(t *testing.T) {
	ssh := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH from the office is firewalled, so use port 2222"}
	region := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "Production runs in region fsn1"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{ssh, region}}
	const merged = "bastion SSH from the office is firewalled, use port 2222, and production runs in region fsn1"

	result := ReflectionResult{
		Memories: []ReflectMemory{{Category: "gotcha", Content: merged}},
		Merges:   []Merge{{IDs: []string{ssh.ID, region.ID}, Text: merged}},
	}
	if drops := AuditGuardedDrops(input, result); len(drops) != 0 {
		t.Fatalf("a surviving merge still lost a source: %+v", drops)
	}

	// The same result with the merge taken away. Nothing carries the sources now,
	// and the only surviving memory is a stranger to both, so both must come back.
	filtered := result
	filtered.Merges = nil
	filtered.Memories = []ReflectMemory{{Category: "fact", Content: "the ledger ingests batches over gRPC"}}
	if drops := AuditGuardedDrops(input, filtered); len(drops) != 2 {
		t.Fatalf("want both sources re-added once their merge is gone, got %d: %+v", len(drops), drops)
	}
}

// TestSupersessionLapsesWhenAFilterRemovesItsSuccessor is the supersession half,
// kept alongside the rewrite half because they are one rule and one list now.
func TestSupersessionLapsesWhenAFilterRemovesItsSuccessor(t *testing.T) {
	const (
		stale = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		fresh = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		other = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
	)
	in := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{ID: stale, Category: "gotcha", Content: "the ledger is reached from the office subnet"},
			{ID: fresh, Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
			{ID: other, Category: "fact", Content: "the bastion answers ping on 443"},
		},
	}
	llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":["drop ` + stale + ` reason: superseded by ` + fresh + `","merge ` +
		fresh + `,` + other + ` -> Production in region fsn1 is fronted by Cloudflare and the dingo bastion answers ping on 443"]}`})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) != 0 {
		t.Fatalf("the contaminating merge survived the filter: %+v", result.Memories)
	}
	drops := AuditGuardedDrops(in, result)
	if !auditContains(drops, in.ExistingMemories[0].Content) {
		t.Fatalf("a supersession outlived the successor the filter removed: %+v", drops)
	}
}

func auditContains(drops []DroppedGuarded, content string) bool {
	for _, d := range drops {
		if d.Content == content {
			return true
		}
	}
	return false
}
