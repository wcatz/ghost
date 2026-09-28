package reflection

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

// opMem is a stored input memory with the id the harness is shown. Ids are
// ULID-shaped because the prompt renders the real ones and a test that used
// "A"/"B" would never catch a formatting change in what the model is asked to
// copy.
func opMem(id, cat, content string, importance float32, tags ...string) memory.Memory {
	return memory.Memory{ID: id, Category: cat, Content: content, Importance: importance, Tags: tags, Source: "mcp"}
}

const (
	opID1 = "D20E133860CC4AFE38B485AD5371BA59"
	opID2 = "4FB5503C3BD92A6A3CACF854EE097A47"
	opID3 = "A639E7A7654D2FF555008CE53674BE19"
	opID4 = "03AB1E23FED1D2318894E4BF95BC54A8"
)

// opInput is a two-memory corpus: one that must survive untouched and one that
// is a candidate for merging or dropping.
func opInput() ReflectionInput {
	return ReflectionInput{
		ProjectName: "ghost",
		ExistingMemories: []memory.Memory{
			opMem(opID1, "gotcha", "SSH to the Hetzner bastion goes through port 2222, not 22", 0.9, "ssh", "port"),
			opMem(opID2, "fact", "Production runs in region fsn1 behind Cloudflare", 0.5, "prod"),
		},
	}
}

// opRun parses and executes an ops response in one step, the way the LLM tier
// does, so a test states only what the harness returned.
func opRun(t *testing.T, input ReflectionInput, reply string) ReflectionResult {
	t.Helper()
	resp, err := parseOpResponse(reply)
	if err != nil {
		t.Fatalf("parseOpResponse: %v", err)
	}
	result, err := executeOps(resp, input, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("executeOps: %v", err)
	}
	return result
}

func opErr(t *testing.T, input ReflectionInput, reply string) error {
	t.Helper()
	resp, err := parseOpResponse(reply)
	if err != nil {
		return err
	}
	_, err = executeOps(resp, input, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	return err
}

// TestKeepOpPassesTheMemoryThroughUntouched is the whole point of the
// per-id contract (#639). Re-emitting a memory as free text resets its id, and
// with it its embedding, its links and its age, because memory_embeddings and
// memory_links are ON DELETE CASCADE. A keep must carry the stored row's own
// content byte for byte, so ReplaceNonManual's exact-content reuse recognises
// it and updates the row in place instead of deleting and re-inserting it.
func TestKeepOpPassesTheMemoryThroughUntouched(t *testing.T) {
	in := opInput()
	result := opRun(t, in, `{"learned_context":"ctx","ops":["keep `+opID1+`"]}`)

	got, ok := findMemory(result, in.ExistingMemories[0].Content)
	if !ok {
		t.Fatalf("the kept row is not in the result: %+v", result.Memories)
	}
	want := in.ExistingMemories[0]
	if got.Content != want.Content {
		t.Errorf("content retyped:\n got %q\nwant %q", got.Content, want.Content)
	}
	if got.Category != want.Category {
		t.Errorf("category = %q, want %q", got.Category, want.Category)
	}
	if got.Importance != want.Importance {
		t.Errorf("importance = %v, want %v — a keep must not reweight the row", got.Importance, want.Importance)
	}
	if strings.Join(got.Tags, ",") != "ssh,port" {
		t.Errorf("tags = %v, want [ssh port] — a keep must not untag the row", got.Tags)
	}
}

// findMemory picks an emitted memory by content, which is the only key a test
// can state without also pinning the order operations happen to be emitted in.
func findMemory(result ReflectionResult, content string) (ReflectMemory, bool) {
	for _, m := range result.Memories {
		if m.Content == content {
			return m, true
		}
	}
	return ReflectMemory{}, false
}

// TestNothingInAResultIsARetypedMemory pins the property the pass-through and
// the keep share: every memory the result contains is either some input's own
// text or the text of a merge or rewrite that named its ids. Under the retired
// free-text contract the model restated every memory it saw, so a memory it had
// nothing to say about came back as a paraphrase with a fresh id — a duplicate
// beside the row the drop guard then re-added verbatim.
func TestNothingInAResultIsARetypedMemory(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "the ledger is reached from the office subnet"
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", "the bastion answers ping on 443", 0.5))

	// The harness only merged the two memories it had something to say about.
	// Every other row must come through unchanged.
	result := opRun(t, in, fmt.Sprintf(
		`{"ops":["merge %s,%s -> the bastion in region fsn1 answers ping on 443"]}`, opID3, opID1))

	stored := make(map[string]bool)
	for _, m := range in.ExistingMemories {
		stored[m.Content] = true
	}
	for _, m := range result.Memories {
		if !stored[m.Content] {
			t.Errorf("a memory in the result matches no input: %q", m.Content)
		}
	}
	if _, ok := findMemory(result, in.ExistingMemories[0].Content); !ok {
		t.Errorf("the id left out of the merge was not carried through: %+v", result.Memories)
	}
}

// TestMergeOpCarriesBothSourcesIntoOneSurvivor: the merge's metadata comes
// from the sources it merges, never from the model's free choice — the
// surviving category is the first id's, importance is the strongest, and tags
// are the union, so folding a memory in never silently recategorizes it or
// drops its labels.
func TestMergeOpCarriesBothSourcesIntoOneSurvivor(t *testing.T) {
	result := opRun(t, opInput(),
		`{"learned_context":"ctx","ops":["merge `+opID1+`,`+opID2+` -> the bastion SSH port 2222 fronts production in region fsn1"]}`)

	if len(result.Memories) != 1 {
		t.Fatalf("got %d memories, want 1 merged survivor: %+v", len(result.Memories), result.Memories)
	}
	got := result.Memories[0]
	if !strings.Contains(got.Content, "2222") || !strings.Contains(got.Content, "fsn1") {
		t.Errorf("merged content lost an input's specifics: %q", got.Content)
	}
	if got.Category != "gotcha" {
		t.Errorf("category = %q, want the first id's gotcha", got.Category)
	}
	if got.Importance != 0.9 {
		t.Errorf("importance = %v, want the strongest source (0.9)", got.Importance)
	}
	if strings.Join(got.Tags, ",") != "port,prod,ssh" {
		t.Errorf("tags = %v, want the sorted union [port prod ssh]", got.Tags)
	}
}

// TestRewriteOpKeepsTheSourceMetadata: a correction is the source's memory with
// corrected text, not a new memory, so it inherits the same row's category,
// weight and labels.
func TestRewriteOpKeepsTheSourceMetadata(t *testing.T) {
	in := opInput()
	in.ExistingMemories[1].Content = "the metrics endpoint is served by the ingest service"
	result := opRun(t, in, `{"learned_context":"ctx","ops":["rewrite `+opID2+` -> the metrics endpoint is served by the billing service, not ingest"]}`)

	got, ok := findMemory(result, "the metrics endpoint is served by the billing service, not ingest")
	if !ok {
		t.Fatalf("the correction was not applied: %+v", result.Memories)
	}
	if got.Category != "fact" || got.Importance != 0.5 || strings.Join(got.Tags, ",") != "prod" {
		t.Errorf("rewrite did not inherit the source row: %+v", got)
	}
}

// TestRewriteThatWouldChangeAnIdentifierIsRejected: the one correction Ghost
// must refuse is the one that swaps a specific. It cannot know which of two
// ports the operator meant, and a wrong number is stored as fact forever — so
// the rewrite is discarded and the source kept, which is also why the prompt
// tells the harness to "keep" a memory whose specifics it cannot reproduce
// exactly.
func TestRewriteThatWouldChangeAnIdentifierIsRejected(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "SSH to the Hetzner bastion uses port 2222"
	result := opRun(t, in, `{"learned_context":"ctx","ops":["rewrite `+opID1+` -> SSH to the Hetzner bastion uses port 22"]}`)

	if _, ok := findMemory(result, "SSH to the Hetzner bastion uses port 22"); ok {
		t.Errorf("an identifier change was written: %+v", result.Memories)
	}
	if _, ok := findMemory(result, in.ExistingMemories[0].Content); !ok {
		t.Errorf("the rejected rewrite lost its source: %+v", result.Memories)
	}
}

// TestDropOpEmitsNothingAndStaysUnderTheDropGuard: an obsolete drop removes the
// harness-claimed redundancy, but the token audit still governs whether the row
// is really deleted — Ghost cannot check the claim, so an obsolete drop that no
// survivor explains leaves the input to be re-added verbatim. The id it names is
// not passed through either, so the drop is the harness's decision rather than
// Ghost's.
func TestDropOpEmitsNothingAndStaysUnderTheDropGuard(t *testing.T) {
	in := opInput()
	result := opRun(t, in, `{"learned_context":"ctx","ops":["drop `+opID1+` reason: obsolete"]}`)

	if _, ok := findMemory(result, in.ExistingMemories[0].Content); ok {
		t.Errorf("a dropped memory was emitted: %+v", result.Memories)
	}
	if len(result.Replacements) != 0 {
		t.Errorf("an obsolete drop claimed a replacement: %+v", result.Replacements)
	}
	if _, ok := findMemory(result, in.ExistingMemories[1].Content); !ok {
		t.Errorf("the id the response never mentioned was not carried through: %+v", result.Memories)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 1 || drops[0].Content != in.ExistingMemories[0].Content {
		t.Fatalf("want the obsolete-dropped input audited for re-add, got %+v", drops)
	}
}

// TestSupersededDropKeepsTheStaleRowWhenTheSuccessorDoesNotExplainIt pins the
// KEEP bias on the real ops path. The model said explicitly that this row is now
// stated better elsewhere and named the id that states it, and the guard does not
// take its word for it: an unattended reflect never deletes a memory on the
// model's say-so alone, because a kept stale row is demotable by resolve and
// supersede and a deleted one is not (#549).
//
// The fixture's successor shares nothing with the row it supersedes (0.000), so
// the corpus cannot show the row is gone and it is retained. The successor is
// still recorded, and still survives.
func TestSupersededDropKeepsTheStaleRowWhenTheSuccessorDoesNotExplainIt(t *testing.T) {
	in := opInput()
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","keep `+opID2+`"]}`)

	if len(result.Memories) != 1 || result.Memories[0].Content != in.ExistingMemories[1].Content {
		t.Fatalf("want only the surviving row emitted, got %+v", result.Memories)
	}
	// The claim is still recorded: a reader of the result should be able to see
	// what the response said it was replacing, even though nothing acts on it.
	if len(result.Replacements) != 1 {
		t.Fatalf("replacements = %+v, want the one stated claim", result.Replacements)
	}
	if r := result.Replacements[0]; r.ID != opID1 || r.Text != in.ExistingMemories[1].Content {
		t.Errorf("replacement = %+v, want %q replaced by the text of %q", r, opID1, opID2)
	}
	drops := AuditGuardedDrops(in, result)
	if !auditContains(drops, in.ExistingMemories[0].Content) {
		t.Fatalf("the superseded row was deleted on the response's say-so alone: %+v", drops)
	}
	if auditContains(drops, in.ExistingMemories[1].Content) {
		t.Errorf("the successor was itself flagged: %+v", drops)
	}
}

// TestExecuteOpsRecordsWhichIdsWereMergedOrRewritten pins the two id lists the
// result carries. A merge's sources are scored against the text of that merge,
// and a rewrite or a drop is in neither list — the guard audits it like any other
// row. Nothing else in the package records this, so a result arriving with empty
// lists would score every merge source against a single output and re-add rows a
// merge had legitimately absorbed.
func TestExecuteOpsRecordsWhichIdsWereMergedOrRewritten(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "the ledger is reached from the office subnet"
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", "the bastion answers ping on 443", 0.5),
		opMem(opID4, "fact", "region fsn1 fronts every ingest worker", 0.4))

	const mergedText = "the bastion in region fsn1 answers ping on 443"
	result := opRun(t, in, `{"learned_context":"ctx","ops":["keep `+opID1+`","drop `+opID2+` reason: obsolete","merge `+opID3+`,`+opID4+` -> `+mergedText+`"]}`)

	if len(result.Merges) != 1 {
		t.Fatalf("merges = %+v, want the one merge", result.Merges)
	}
	if result.Merges[0].Text != mergedText {
		t.Errorf("merge text = %q, want %q — it is the text the sources are scored against", result.Merges[0].Text, mergedText)
	}
	merged := map[string]bool{}
	for _, id := range result.Merges[0].IDs {
		merged[memIDKey(id)] = true
	}
	for _, want := range []string{opID3, opID4} {
		if !merged[memIDKey(want)] {
			t.Errorf("%s was merged but not recorded: %+v", want, result.Merges)
		}
	}
	for _, unwanted := range []string{opID1, opID2} {
		if merged[memIDKey(unwanted)] {
			t.Errorf("%s was kept or dropped but recorded as merged: %+v", unwanted, result.Merges)
		}
	}
	if len(result.Replacements) != 0 {
		t.Errorf("nothing was rewritten, but a replacement was recorded: %v", result.Replacements)
	}
}

// TestRejectedRewriteClaimsNothing pins the other half: when the grounding check
// refuses a rewrite its source is re-emitted verbatim, so the operation folded
// nothing and replaced nothing. Recording it as rewritten anyway would misreport
// the response's own account of what it did.
func TestRejectedRewriteClaimsNothing(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "SSH to the Hetzner bastion uses port 2222"
	result := opRun(t, in, `{"learned_context":"ctx","ops":["rewrite `+opID1+` -> SSH to the Hetzner bastion uses port 22"]}`)

	if len(result.Memories) != 2 {
		t.Fatalf("got %d memories, want the source verbatim plus the other input: %+v",
			len(result.Memories), result.Memories)
	}
	if len(result.Replacements) != 0 {
		t.Errorf("a rejected rewrite claimed the row: %v", result.Replacements)
	}
}

// TestRewrittenRowIsNotReAddedBesideItsReplacement covers the rewrite that DOES
// carry the memory's substance, which is the case the audit passes on its own: a
// 45% overlap test is not applied to a rewrite's CLAIM at all any more (#549) —
// the replacement is simply an output, and an output that explains the old row
// stops the row being re-added beside it. Where the replacement says nothing of
// the old row, which is the more common rewrite since changing the wording is the
// point, the old row is KEPT; see TestRewrittenRowIsKeptWhenTheRewriteSaysNothingOfIt.
func TestRewrittenRowIsNotReAddedBesideItsReplacement(t *testing.T) {
	old := "bastion SSH uses port 2222 with a hardware key"
	next := "bastion SSH on port 2222 is now opened with Cloudflare Access"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: opID1, Category: "gotcha", Content: old},
		{ID: opID2, Category: "fact", Content: "the ledger ingests batches over gRPC"},
	}}
	written, drops := passThroughResult(t, in, fmt.Sprintf(
		`{"ops":["rewrite %s -> %s","keep %s"]}`, opID1, next, opID2))
	if written[old] {
		t.Errorf("the rewritten row was re-added beside its replacement: %v", written)
	}
	if len(drops) != 0 {
		t.Errorf("a rewrite carrying the substance was flagged for re-add: %+v", drops)
	}
}

// TestSupersessionRecordFollowsAMerge records the text the successor actually
// carries, not its stored content: a successor folded into a merge is replaced by
// the merge's text, and a record pointing at something the result does not hold
// misreports what the response said. The record is not an exemption, so the
// stale row's fate is decided by the audit either way — here the merge carries
// the row's substance outright, at containment 1.000, so it is not re-added.
func TestSupersessionRecordFollowsAMerge(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "the ledger is reached from the office subnet"
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", "the bastion answers ping on 443", 0.5))
	const merged = "the ledger is reached from the office subnet, the bastion answers ping on 443, and production in region fsn1 is fronted by Cloudflare"
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","merge `+opID2+`,`+opID3+` -> `+merged+`"]}`)

	if len(result.Replacements) != 1 {
		t.Fatalf("replacements = %+v, want one", result.Replacements)
	}
	if got := result.Replacements[0].Text; got != merged {
		t.Errorf("recorded text = %q, want the merged text %q", got, merged)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Fatalf("a successor the merge explained was still flagged: %+v", drops)
	}
}

// TestObsoleteDropIsStillAudited is the other side: only a supersession the
// harness can point at is honoured, so an unverifiable "obsolete" claim leaves
// the input to the ordinary audit rather than deleting it on the model's word.
func TestObsoleteDropIsStillAudited(t *testing.T) {
	in := opInput()
	result := opRun(t, in, `{"learned_context":"ctx","ops":["drop `+opID1+` reason: obsolete","keep `+opID2+`"]}`)
	drops := AuditGuardedDrops(in, result)
	if len(drops) != 1 || drops[0].Content != in.ExistingMemories[0].Content {
		t.Fatalf("want the obsolete-dropped input audited for re-add, got %+v", drops)
	}
}

// TestParseOpResponseRejectsUnparseableLine: strictness is the point. A line
// Ghost cannot read is a line whose meaning Ghost does not know, and applying
// the rest of the response would be a partial consolidation — the model said
// one thing, the corpus is rewritten as if it had said another. The tier must
// fail so the tiered consolidator falls through to the deterministic tier.
func TestParseOpResponseRejectsUnparseableLine(t *testing.T) {
	for name, line := range map[string]string{
		"unknown verb":          `consolidate ` + opID1,
		"keep with no id":       `keep`,
		"drop without a reason": `drop ` + opID1,
		"drop with free reason": `drop ` + opID1 + ` because it feels wrong`,
		"merge with no arrow":   `merge ` + opID1 + `,` + opID2 + ` the two of them`,
		"merge with no text":    `merge ` + opID1 + `,` + opID2 + ` -> `,
		"rewrite with no id":    `rewrite -> corrected`,
		"single-id merge":       `merge ` + opID1 + ` -> corrected`,
	} {
		t.Run(name, func(t *testing.T) {
			reply := `{"learned_context":"ctx","ops":["keep ` + opID2 + `","` + line + `"]}`
			resp, err := parseOpResponse(reply)
			if err != nil {
				return // rejected at parse time
			}
			if _, err := executeOps(resp, opInput(), nil); err == nil {
				t.Fatalf("line %q was accepted", line)
			}
		})
	}
}

// TestParseOpResponseRejectsUnknownID: an id Ghost did not feed the model is a
// reference to nothing. Guessing at it (prefix matching, nearest match) would
// let a hallucinated line delete or rewrite a real memory, so it fails the run.
func TestParseOpResponseRejectsUnknownID(t *testing.T) {
	for name, reply := range map[string]string{
		"keep":             `{"ops":["keep 00000000000000000000000000000000"]}`,
		"merge source":     `{"ops":["merge 00000000000000000000000000000000,` + opID2 + ` -> x"]}`,
		"rewrite target":   `{"ops":["rewrite ` + opID1 + ` -> x","drop 00000000000000000000000000000000 reason: obsolete"]}`,
		"drop target":      `{"ops":["drop ` + opID1 + ` reason: superseded by 00000000000000000000000000000000","keep ` + opID2 + `"]}`,
		"truncated prefix": `{"ops":["keep ` + opID1[:8] + `"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := opErr(t, opInput(), reply); err == nil {
				t.Fatalf("reply %q was accepted", reply)
			}
		})
	}
}

// TestParseOpResponseAcceptsCaseInsensitiveID: a model that lower-cases an id
// still means the memory it was shown. This is a spelling of the same id, not a
// different one, so it must not cost the whole pass.
func TestParseOpResponseAcceptsCaseInsensitiveID(t *testing.T) {
	in := opInput()
	result := opRun(t, in, `{"learned_context":"ctx","ops":["keep `+strings.ToLower(opID1)+`"]}`)
	if _, ok := findMemory(result, in.ExistingMemories[0].Content); !ok {
		t.Fatalf("lower-cased id did not resolve: %+v", result.Memories)
	}
}

// TestParseOpResponseRejectsIDInTwoOperations: one id in two operations is a
// contradiction ("keep A" and "merge A,B"), and picking either one silently
// loses the other's intent. Both readings are defensible, so neither is applied.
func TestParseOpResponseRejectsIDInTwoOperations(t *testing.T) {
	reply := `{"ops":["keep ` + opID1 + `","merge ` + opID1 + `,` + opID2 + ` -> both facts"]}`
	if err := opErr(t, opInput(), reply); err == nil {
		t.Fatal("a doubly-claimed id was accepted")
	}
}

// TestSupersededDropTargetMustSurvive: a supersession pointing at an id the
// same response drops is not a supersession — the knowledge would leave the
// corpus with nothing. It fails rather than deleting the source.
func TestSupersededDropTargetMustSurvive(t *testing.T) {
	reply := `{"ops":["drop ` + opID1 + ` reason: superseded by ` + opID2 + `","drop ` + opID2 + ` reason: obsolete"]}`
	if err := opErr(t, opInput(), reply); err == nil {
		t.Fatal("a supersession whose target is also dropped was accepted")
	}
}

// TestReaderComplaintsAreBounded: a complaint is quoted back into a prompt,
// written to an append-only log, and printed as the run's failure, so every
// model-supplied fragment in it is bounded where it is interpolated — clipping
// the operation LINE is not enough on its own. `drop <id> reason: <a paragraph>`
// is the case that makes this load-bearing: the reason tail is model prose, and
// unbounded it would put a whole paragraph of it (and of whatever stored memory
// the model was echoing) into all three of those sinks.
func TestReaderComplaintsAreBounded(t *testing.T) {
	// Long enough that no clip of it survives, and shaped like prose over a stored
	// memory rather than like an id, which is what a hallucinated id looks like
	// too.
	// The sentinel sits past BOTH clips — the 80-rune line quote and the 60-rune
	// fragment clip — so it can only appear in the message if one of them stopped
	// working, and it stands in for whatever prose the model was echoing out of
	// stored memory.
	const sentinel = "SENTINEL-PAST-BOTH-CLIPS"
	noise := strings.Repeat("x", 150) + sentinel + strings.Repeat("y", 2000)

	t.Run("unreadable drop reason", func(t *testing.T) {
		err := opErr(t, opInput(), `{"ops":["drop `+opID1+` reason: `+noise+`"]}`)
		if err == nil {
			t.Fatal("a free-form drop reason was accepted")
		}
		// The line quote (80 runes) plus the bounded fragment clip, plus the fixed
		// prose around them. Generous: the claim is that the message cannot grow
		// with the response, not that it is short.
		if n := len(err.Error()); n > 400 {
			t.Errorf("complaint is %d bytes, want a bound that does not grow with the response:\n%s", n, err.Error())
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("the complaint carries model text from past the fragment clip:\n%s", err.Error())
		}
	})

	t.Run("hallucinated id", func(t *testing.T) {
		err := opErr(t, opInput(), `{"ops":["keep `+noise+opID2+`"]}`)
		if err == nil {
			t.Fatal("a hallucinated id was accepted")
		}
		if n := len(err.Error()); n > 400 {
			t.Errorf("complaint is %d bytes, want a bound that does not grow with the response:\n%s", n, err.Error())
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("the complaint carries model text from past the fragment clip:\n%s", err.Error())
		}
	})
}

// TestReaderComplaintForLogWithholdsTheLine: the log rendering is the one a sink
// that outlives the run may carry, so the quoted operation line is withheld
// while the reason and the id — the diagnostic, and neither of them content —
// survive. It also has to fail CLOSED on an error this package did not build as a
// complaint, because an unrecognised renderer is exactly the case where a log
// line must not guess.
func TestReaderComplaintForLogWithholdsTheLine(t *testing.T) {
	err := opErr(t, opInput(), `{"ops":["keep 00000000000000000000000000000000"]}`)
	if err == nil {
		t.Fatal("an unknown id was accepted")
	}
	safe := readerComplaintForLog(err)
	if !strings.Contains(safe, "00000000000000000000000000000000") {
		t.Errorf("safe rendering dropped the id that was refused:\n%s", safe)
	}
	if !strings.Contains(safe, "is not one of the memories this run was given") {
		t.Errorf("safe rendering dropped the reason:\n%s", safe)
	}
	if !strings.Contains(safe, "withheld") {
		t.Errorf("safe rendering does not say the operation line was withheld:\n%s", safe)
	}
	if strings.Contains(safe, `"keep 00000000000000000000000000000000"`) {
		t.Errorf("safe rendering quotes the operation line back:\n%s", safe)
	}

	fellback := readerComplaintForLog(fmt.Errorf("something else went wrong"))
	if !strings.Contains(fellback, "unclassified") {
		t.Errorf("an unrecognised error = %q, want it withheld whole and named by type", fellback)
	}
	if strings.Contains(fellback, "went wrong") {
		t.Errorf("an unrecognised error was printed verbatim: %q", fellback)
	}
}

// TestParseOpResponseRejectsTheOldMemoriesShape: the pre-#639 contract had the
// model return a "memories" array. Accepting it would silently keep the free-text
// path alive for any harness that still answers that way, which is the exact
// behaviour this change removes, so the response fails and the next tier runs.
func TestParseOpResponseRejectsTheOldMemoriesShape(t *testing.T) {
	reply := `{"learned_context":"ctx","memories":[{"category":"fact","content":"free text","importance":0.5,"tags":[]}]}`
	if err := opErr(t, opInput(), reply); err == nil {
		t.Fatal("the retired memories[] shape was accepted")
	}
}

// TestParseOpResponseRejectsMissingAndEmptyOps: an empty operation list is not
// a consolidation, it is a deletion, and the quality gate is the only thing
// between it and the store's empty-set refusal. Failing here names the cause.
func TestParseOpResponseRejectsMissingAndEmptyOps(t *testing.T) {
	for name, reply := range map[string]string{
		"absent": `{"learned_context":"ctx"}`,
		"empty":  `{"learned_context":"ctx","ops":[]}`,
		"null":   `{"learned_context":"ctx","ops":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := opErr(t, opInput(), reply); err == nil {
				t.Fatalf("reply %q was accepted", reply)
			}
		})
	}
}

// TestParseOpResponseRejectsMalformedJSONAndStripsFences: the envelope is still
// JSON behind an optional code fence, and truncated output stays an error — a
// partial operation list is exactly the partial apply this contract forbids.
func TestParseOpResponseRejectsMalformedJSONAndStripsFences(t *testing.T) {
	if _, err := parseOpResponse(`{"ops":["keep ` + opID1 + `"`); err == nil {
		t.Error("truncated JSON was accepted")
	}
	if _, err := parseOpResponse("I could not produce JSON, here is a summary instead."); err == nil {
		t.Error("prose was accepted")
	}
	fenced := "```json\n" + `{"learned_context":"ctx","ops":["keep ` + opID1 + `"]}` + "\n```"
	resp, err := parseOpResponse(fenced)
	if err != nil {
		t.Fatalf("fenced JSON rejected: %v", err)
	}
	if resp.Ops == nil || len(*resp.Ops) != 1 {
		t.Fatalf("fenced ops = %v, want 1", resp.Ops)
	}
}

// TestParseOpResponseErrorSnippetIsRuneSafe keeps malformed model output from
// exposing a split UTF-8 rune in a diagnostic preview: the snippet is cut to a
// byte budget, and a whole rune has to survive the cut.
func TestParseOpResponseErrorSnippetIsRuneSafe(t *testing.T) {
	_, err := parseOpResponse("x" + strings.Repeat("🙂", 40) + "not-json")
	if err == nil {
		t.Fatal("parseOpResponse unexpectedly accepted malformed JSON")
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("error contains a split UTF-8 rune: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "🙂") {
		t.Errorf("error dropped every whole rune: %q", err.Error())
	}
	if strings.Contains(err.Error(), `\x`) || strings.Contains(err.Error(), `\u`) {
		t.Errorf("error exposes a split rune as an escape: %q", err.Error())
	}
}

// TestOpTierFallsBackWhenOpsAreUnreadable is the end-to-end shape of
// the fallback the strict parser exists to feed: a harness that answers in the
// retired shape loses the pass, and the deterministic tier consolidates instead.
// The harness is a fake that returns a canned string — no real CLI is spawned.
func TestOpTierFallsBackWhenOpsAreUnreadable(t *testing.T) {
	input := opInput()
	input.ExistingMemories = append(input.ExistingMemories,
		opMem(opID3, "fact", "Region fsn1 also fronts the metrics endpoint", 0.4))

	llm := NewLlmConsolidator(&fakeReflector{reply: `{"learned_context":"ctx","memories":[{"content":"x"}]}`})
	tiered := NewTieredConsolidator([]Consolidator{llm, NewSQLiteConsolidator()},
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	result, err := tiered.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) == 0 {
		t.Fatal("the fallback tier produced nothing")
	}
	if !strings.Contains(tiered.Name(), "sqlite") {
		t.Errorf("active tier = %q, want the sqlite fallback", tiered.Name())
	}
}

// TestOpTierKeepsSourcesWhenAMergeInventsAnIdentifier is the grounding
// check at the tier boundary: the merge the model asked for is discarded and
// its sources are carried through untouched, so the run still applies and the
// invented identifier never reaches memory.
func TestOpTierKeepsSourcesWhenAMergeInventsAnIdentifier(t *testing.T) {
	in := opInput()
	in.ExistingMemories[1].Content = "The mesh is served by 2.BeXIAhbj.js"
	llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":["merge ` + opID1 + `,` + opID2 + ` -> the bastion on port 2222 fronts the mesh bundle 2.BeXIAhbq.js"]}`})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) != 2 {
		t.Fatalf("want both sources kept verbatim, got %d: %+v", len(result.Memories), result.Memories)
	}
	for _, m := range result.Memories {
		if m.Content != in.ExistingMemories[0].Content && m.Content != in.ExistingMemories[1].Content {
			t.Errorf("an ungrounded merge was written: %q", m.Content)
		}
	}
}

// TestOpTierFallsBackWhenEverythingIsDropped: a run that keeps nothing
// on a corpus big enough to expect a consolidation is the truncation case the
// quality gate exists for, and it must reach the deterministic tier rather than
// hand an empty set to the store.
func TestOpTierFallsBackWhenEverythingIsDropped(t *testing.T) {
	var input ReflectionInput
	for i := 0; i < 8; i++ {
		input.ExistingMemories = append(input.ExistingMemories,
			opMem(strings.ToUpper(strings.Repeat("a", 31-len(string(rune('0'+i)))))+string(rune('0'+i)), "fact",
				"distinct fact number "+string(rune('a'+i))+" about the ingest pipeline", 0.5))
	}
	input.ProjectName = "ghost"
	var drops []string
	for _, m := range input.ExistingMemories {
		drops = append(drops, `drop `+m.ID+` reason: obsolete`)
	}
	llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":[` + strings.Join(drops, ",") + `]}`})

	tiered := NewTieredConsolidator([]Consolidator{llm, NewSQLiteConsolidator()},
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	result, err := tiered.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) == 0 {
		t.Fatal("an empty LLM result was accepted instead of falling back")
	}
	if !strings.Contains(tiered.Name(), "sqlite") {
		t.Errorf("active tier = %q, want the sqlite fallback", tiered.Name())
	}
}

// fakeReflector is a harness stand-in: it returns a canned reply and counts
// calls. No real CLI harness is ever spawned by these tests.
type fakeReflector struct {
	reply string
	err   error
	calls int
}

func (f *fakeReflector) Reflect(_ context.Context, _ string) (string, ai.TokenUsage, error) {
	f.calls++
	if f.err != nil {
		return "", ai.TokenUsage{}, f.err
	}
	return f.reply, ai.TokenUsage{}, nil
}

// TestReaderComplaintWithholdsAValueFromTheReasonToo: withholding the operation
// LINE is not enough, because six reasons quote a model-supplied fragment (the
// source enumerates all six) and a refusal is often triggered BY that fragment
// being free-form. A
// `drop <id> reason: <a credential>` is refused precisely because the tail is
// neither "obsolete" nor "superseded by <id>", so the refused text is the
// model's own, and a short-format token fits inside clipOpText's clip whole. Without the value-shape gate this string is what
// readerComplaintForLog and safeTierError write to the append-only lifecycle.log,
// and it would be the one in-tier log sink without the gate its three siblings
// get from previewContent.
func TestReaderComplaintWithholdsAValueFromTheReasonToo(t *testing.T) {
	// A credential-shaped value: the shape, not a real one, and long enough that
	// clipOpText's 60-rune clip would keep most of it.
	const secretValue = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	for name, in := range map[string]ReflectionInput{
		// The free-form drop tail, quoted by the unreadable-reason refusal.
		"free-form drop reason": opInput(),
	} {
		t.Run(name, func(t *testing.T) {
			err := opErr(t, in, `{"ops":["drop `+opID1+` reason: `+secretValue+`"]}`)
			if err == nil {
				t.Fatal("a free-form drop reason was accepted")
			}
			safe := readerComplaintForLog(err)
			if !strings.Contains(safe, "unreadable drop reason") {
				t.Errorf("safe rendering dropped the reason a reader needs:\n%s", safe)
			}
			if strings.Contains(safe, secretValue) {
				t.Errorf("safe rendering carries a value the refused reason quoted:\n%s", safe)
			}
			if !strings.Contains(safe, "withheld") {
				t.Errorf("safe rendering does not say the value was withheld:\n%s", safe)
			}
		})
	}

	// The mirror direction, and the one a lower-fold-only gate misses. The
	// free-form drop tail is probed in the spelling the MODEL WROTE, so a
	// lower-case AKIA key — which secret.Detect only recognises upper-cased,
	// since its rule is `(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}` — reaches the gate
	// with neither the original nor the lower-folded spelling matching. The
	// superseded-by case below is the same token in the other position: the parser
	// upper-cases that one, so it IS caught there. Both directions are reachable,
	// which is what makes the gate's third probe load-bearing.
	t.Run("free-form drop reason holding a lower-case upper-only token", func(t *testing.T) {
		const akia = "akiaiosfodnn7example"
		err := opErr(t, opInput(), `{"ops":["drop `+opID1+` reason: the deploy token is `+akia+`"]}`)
		if err == nil {
			t.Fatal("a free-form drop reason was accepted")
		}
		safe := readerComplaintForLog(err)
		if strings.Contains(strings.ToUpper(safe), strings.ToUpper(akia)) {
			t.Errorf("safe rendering carries an upper-only-shaped token the lower fold missed:\n%s", safe)
		}
	})

	// A third site, and the one a reader is most likely to assume is covered by
	// the other two: a `superseded by` TARGET. It is a different executeOps
	// branch from the unknown id above, quoting a different fragment, and
	// clipOpText is the only thing between it and the log — so if a future
	// refactor routes this one differently, only a test on THIS branch notices.
	t.Run("superseded-by target that is a value", func(t *testing.T) {
		err := opErr(t, opInput(), `{"ops":["drop `+opID1+` reason: superseded by `+secretValue+`"]}`)
		if err == nil {
			t.Fatal("a supersession to an id this run was not given was accepted")
		}
		safe := readerComplaintForLog(err)
		if !strings.Contains(safe, "superseded-by target") {
			t.Errorf("safe rendering dropped the reason a reader needs:\n%s", safe)
		}
		// Case-insensitively, and deliberately: the parser UPPER-CASES a
		// supersession's target (it matches the stored spelling, hex(randomblob)
		// being upper-case), so the value reaching this complaint is not the
		// string this test wrote. A case-sensitive Contains here passes whether
		// or not the gate ran — which is exactly what the mutation below caught
		// doing on the first version of this case.
		if strings.Contains(strings.ToUpper(safe), strings.ToUpper(secretValue)) {
			t.Errorf("safe rendering carries the hallucinated supersession target verbatim:\n%s", safe)
		}
	})

	// The same gate on a hallucinated id, which executeOps quotes by name: the
	// model invents an id that IS the value, and the reader refuses it for not
	// being one of the input.
	t.Run("hallucinated id that is a value", func(t *testing.T) {
		err := opErr(t, opInput(), `{"ops":["keep `+secretValue+`"]}`)
		if err == nil {
			t.Fatal("a hallucinated id was accepted")
		}
		safe := readerComplaintForLog(err)
		if !strings.Contains(safe, "is not one of the memories this run was given") {
			t.Errorf("safe rendering dropped the reason:\n%s", safe)
		}
		if strings.Contains(safe, secretValue) {
			t.Errorf("safe rendering carries the hallucinated id verbatim:\n%s", safe)
		}
	})
}

// TestClipOpTextStillGatesARealId: the case-insensitive probe exists to catch a
// folded token, and its cost is a second Detect call on every fragment in every
// complaint. What must not regress is the other direction — a real stored id
// reaching a diagnostic, which happens on every unknown-id refusal that is
// merely a typo rather than an attack. A stored id must pass through untouched,
// for two separate reasons the detector's own constants give: it is 32 hex
// characters, which is under the two bare-hex floors (cardanoKeyMinRun 68,
// longHexFloor 132) though ABOVE assignedSecretFloor (20) — so "it is short"
// is not the general answer either — and it carries no provider prefix and no
// `key: value` assignment, which is what every other rule needs. If a future
// rule widened enough to catch one, this fails and the fix belongs in the rule
// rather than in the gate.
func TestClipOpTextStillGatesARealId(t *testing.T) {
	for _, id := range []string{opID1, opID2, opID3, strings.ToLower(opID1), "A1B2C3D4E5F6A7B8C9D0E1F2A3B4C5D6"} {
		if got := clipOpText(id); got != id {
			t.Errorf("clipOpText(%q) = %q, want it unchanged: a real id is a diagnostic, not a value", id, got)
		}
	}
}
