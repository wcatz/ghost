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
	if len(result.Supersessions) != 0 {
		t.Errorf("an obsolete drop claimed supersession: %+v", result.Supersessions)
	}
	if _, ok := findMemory(result, in.ExistingMemories[1].Content); !ok {
		t.Errorf("the id the response never mentioned was not carried through: %+v", result.Memories)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 1 || drops[0].Content != in.ExistingMemories[0].Content {
		t.Fatalf("want the obsolete-dropped input audited for re-add, got %+v", drops)
	}
}

// TestSupersededDropIsNotReAdded pins the drop-guard half of the contract. The
// model said explicitly that this row is now stated better elsewhere and named
// the id that states it, so re-adding the stale row beside its own successor is
// the goduckbot defect in #639 ("three open issues" restored next to "have
// been fixed"). The guard must honour the claim instead of re-adding.
func TestSupersededDropIsNotReAdded(t *testing.T) {
	in := opInput()
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","keep `+opID2+`"]}`)

	if len(result.Memories) != 1 || result.Memories[0].Content != in.ExistingMemories[1].Content {
		t.Fatalf("want only the surviving row, got %+v", result.Memories)
	}
	if len(result.Supersessions) != 1 {
		t.Fatalf("supersessions = %+v, want the one stated claim", result.Supersessions)
	}
	if s := result.Supersessions[0]; s.DroppedID != opID1 || s.TargetID != opID2 ||
		s.TargetText != in.ExistingMemories[1].Content {
		t.Errorf("supersession = %+v, want %q superseded by %q carrying %q",
			s, opID1, opID2, in.ExistingMemories[1].Content)
	}
	drops := AuditGuardedDrops(in, result)
	if len(drops) != 0 {
		t.Fatalf("the explicitly superseded memory was re-added: %+v", drops)
	}
}

// TestExecuteOpsRecordsWhichIdsWereMergedOrRewritten pins the two id lists the
// drop guard reads. A merge's sources are scored against the union of the outputs
// (a merge may spread one source's substance), a rewrite's source is exempt (the
// op replaced the row), and a dropped id is in neither (the guard still judges it).
// Nothing else in the package records this, so a result arriving with empty lists
// would silently restore the strict comparison for merges and re-add every
// rewritten row beside its replacement.
func TestExecuteOpsRecordsWhichIdsWereMergedOrRewritten(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "the ledger is reached from the office subnet"
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", "the bastion answers ping on 443", 0.5),
		opMem(opID4, "fact", "region fsn1 fronts every ingest worker", 0.4))

	result := opRun(t, in, `{"learned_context":"ctx","ops":["keep `+opID1+`","drop `+opID2+` reason: obsolete","merge `+opID3+`,`+opID4+` -> the bastion in region fsn1 answers ping on 443"]}`)

	merged := map[string]bool{}
	for _, id := range result.MergedIDs {
		merged[memIDKey(id)] = true
	}
	for _, want := range []string{opID3, opID4} {
		if !merged[memIDKey(want)] {
			t.Errorf("%s was merged but not recorded: merged=%v", want, result.MergedIDs)
		}
	}
	for _, unwanted := range []string{opID1, opID2} {
		if merged[memIDKey(unwanted)] {
			t.Errorf("%s was kept or dropped but recorded as merged: %v", unwanted, result.MergedIDs)
		}
	}
	if len(result.RewrittenIDs) != 0 {
		t.Errorf("nothing was rewritten, but RewrittenIDs = %v", result.RewrittenIDs)
	}
}

// TestRejectedRewriteClaimsNothing pins the other half: when the grounding check
// refuses a rewrite its source is re-emitted verbatim, so the operation folded
// nothing and replaced nothing. Recording it as rewritten anyway would exempt the
// row from the guard on the strength of a change that never happened.
func TestRejectedRewriteClaimsNothing(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "SSH to the Hetzner bastion uses port 2222"
	result := opRun(t, in, `{"learned_context":"ctx","ops":["rewrite `+opID1+` -> SSH to the Hetzner bastion uses port 22"]}`)

	if len(result.Memories) != 2 {
		t.Fatalf("got %d memories, want the source verbatim plus the other input: %+v",
			len(result.Memories), result.Memories)
	}
	if len(result.RewrittenIDs) != 0 {
		t.Errorf("a rejected rewrite claimed the row: %v", result.RewrittenIDs)
	}
}

// TestRewrittenRowIsNotReAddedBesideItsReplacement is why a rewrite is exempt
// from the drop guard. The replacement need not resemble the original — that is
// the point of a rewrite — so a 45% overlap test would put the old text back
// beside the new one, which is a duplicate rather than a save.
func TestRewrittenRowIsNotReAddedBesideItsReplacement(t *testing.T) {
	old := "bastion SSH uses port 2222 with a hardware key"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: opID1, Category: "gotcha", Content: old},
		{ID: opID2, Category: "fact", Content: "operator access is now fronted by Cloudflare Access"},
	}}
	written, drops := passThroughResult(t, in, fmt.Sprintf(
		`{"ops":["rewrite %s -> operator access is fronted by Cloudflare Access","keep %s"]}`, opID1, opID2))
	if written[old] {
		t.Errorf("the rewritten row was re-added beside its replacement: %v", written)
	}
	if len(drops) != 0 {
		t.Errorf("a rewrite was flagged for re-add: %+v", drops)
	}
}

// TestSupersessionWitnessFollowsAMerge records the witness as the text the
// successor actually carries, not its stored content: a successor folded into a
// merge is replaced by the merge's text, and the claim is only checkable if it
// points at what the result really holds.
func TestSupersessionWitnessFollowsAMerge(t *testing.T) {
	in := opInput()
	in.ExistingMemories[0].Content = "the ledger is reached from the office subnet"
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", "the bastion answers ping on 443", 0.5))
	const merged = "Production in region fsn1 is fronted by Cloudflare and the bastion answers ping on 443"
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","merge `+opID2+`,`+opID3+` -> `+merged+`"]}`)

	if len(result.Supersessions) != 1 {
		t.Fatalf("supersessions = %+v, want one", result.Supersessions)
	}
	if got := result.Supersessions[0].TargetText; got != merged {
		t.Errorf("witness text = %q, want the merged text %q", got, merged)
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Fatalf("a supersession into a merge was undone: %+v", drops)
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
