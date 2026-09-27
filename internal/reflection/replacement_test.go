package reflection

import (
	"context"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRewriteKeepsTheSourceWhenAFilterRemovesTheReplacement drives the whole
// tier. The rewrite passes the grounding check and the operations are resolved —
// and then dropForeignProjectMemories runs and removes the rewritten text, because
// it names a project the input corpus never mentioned. The source row is kept:
// there is nothing left in the corpus that explains where its knowledge went, and
// an unattended reflect never deletes a memory on the model's say-so alone
// (#549). Demoting it later is resolve and supersede's job.
func TestRewriteKeepsTheSourceWhenAFilterRemovesTheReplacement(t *testing.T) {
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

// TestRewriteIsNotReAddedOnlyWhileItsReplacementSurvives is the unit half of the
// same rule, stated both ways so a change to either side is caught here. The
// replacement is not an exemption — it is an output, and the row is simply
// explained by it while it lasts.
func TestRewriteIsNotReAddedOnlyWhileItsReplacementSurvives(t *testing.T) {
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

// TestSupersessionAndRewriteAreRecordedTogether pins that a rewrite and a
// supersession land in ONE list: both are an input id the response disposed of,
// together with the text it says took its place, and a reader of the result
// should see both by the same route. The list is a record — the drop guard reads
// neither kind, and audits a disposed row like any other (#549) — but it is the
// only place the response's own account of a disposal is visible, and a dry run
// and a reader of a bug report both want it.
func TestSupersessionAndRewriteAreRecordedTogether(t *testing.T) {
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
		t.Errorf("the rewrite's recorded text = %q, want the rewritten text %q", byID[memIDKey(seqID(1))], new)
	}
	if byID[memIDKey(seqID(2))] != new {
		t.Errorf("the supersession's recorded text = %q, want the successor's text %q", byID[memIDKey(seqID(2))], new)
	}
	// Both rows are explained by the rewritten text, which is an output, so the
	// audit finds a survivor for each and neither is flagged.
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Errorf("both rows are explained by an output, yet %d were flagged: %+v", len(drops), drops)
	}

	// Take that output away and neither row has anything left explaining it.
	filtered := result
	filtered.Memories = nil
	if drops := AuditGuardedDrops(in, filtered); len(drops) != 2 {
		t.Fatalf("want both rows audited once the explaining output is gone, got %+v", drops)
	}
}

// TestReplacementWithNoTextDisposesOfNothing covers the degenerate record: a
// claim carrying no text at all. executeOps only records a rewrite it actually
// applied and a successor that survives the response, so this cannot arise from
// the tier — but a result carrying one must not read as "this row is definitely
// gone". It is trivially true now that nothing reads the list, and is kept as a
// guard on the record's shape.
func TestReplacementWithNoTextDisposesOfNothing(t *testing.T) {
	const old = "the ledger ingests through the bastion on port 2222"
	in := ReflectionInput{ExistingMemories: []memory.Memory{{ID: seqID(1), Category: "fact", Content: old}}}
	result := ReflectionResult{Replacements: []Replacement{{ID: seqID(1)}}}

	drops := AuditGuardedDrops(in, result)
	if len(drops) != 1 || drops[0].Content != old {
		t.Fatalf("a text-less replacement disposed of the row: %+v", drops)
	}
}

// TestRewriteOfASourceAFilterKeptIsStillExplained guards the rule from
// over-reaching: the common case must not start re-adding rewritten rows, or every
// rewrite becomes a duplicate.
func TestRewriteOfASourceAFilterKeptIsStillExplained(t *testing.T) {
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

// TestSupersessionKeepsTheStaleRowWhenAFilterRemovesItsSuccessor is the
// supersession half of the rewrite rule above, kept beside it because the two
// share a list and, now, a verdict: the successor is folded into a merge that
// names a project the input corpus never mentioned, so the filter removes the
// whole merge and nothing in the corpus explains the stale row any more. It is
// kept.
func TestSupersessionKeepsTheStaleRowWhenAFilterRemovesItsSuccessor(t *testing.T) {
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
