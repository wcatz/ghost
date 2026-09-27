package reflection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// fmtRewrite is an ops response that rewrites opID1 to the given text and keeps
// opID2, so a test states only the wording under test.
func fmtRewrite(text string) string {
	return fmt.Sprintf(`{"ops":["rewrite %s -> %s","keep %s"]}`, opID1, text, opID2)
}

// These pin the two ways a consolidation used to delete a memory an unattended
// apply never reported (#549). Both are silent — no warning, no --allow-drops,
// exit 0 — and both are now closed by the same rule: the drop guard keeps a row
// the corpus cannot show is gone, because resolve and supersede run right after
// reflect and can demote a stale row, while nothing can bring back a deleted one.

// TestSupersessionFromTheOpsPathKeepsTheStaleRow drives the real executeOps
// path, not a hand-built fixture, so the verdict is the one a `ghost reflect
// --apply --require-llm` would reach. The two notes are ordinary and unrelated —
// a bastion SSH port and a production region — and the response pairs them,
// which the parser allows because the target survives the response. The stale row
// is kept.
func TestSupersessionFromTheOpsPathKeepsTheStaleRow(t *testing.T) {
	in := opInput()
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","keep `+opID2+`"]}`)

	// The claim is still recorded, because a reader of the result should be able
	// to see what the response said it was replacing.
	if len(result.Replacements) != 1 || result.Replacements[0].ID != opID1 {
		t.Fatalf("replacements = %+v, want the one stated claim", result.Replacements)
	}
	if drops := AuditGuardedDrops(in, result); !auditContains(drops, in.ExistingMemories[0].Content) {
		t.Fatalf("a supersession deleted a memory the corpus could not show was gone: %+v", drops)
	}
}

// TestRewrittenRowIsKeptWhenTheRewriteSaysNothingOfIt is the same rule on the
// rewrite path, and it is a real behaviour change rather than a refinement: a
// rewrite whose new text shares nothing with the old one used to be an automatic
// exemption, so the old row went. The whole point of a rewrite is to change the
// wording, so that exemption covered most rewrites, and it was the same hole as
// the supersession one — the model saying so, and the guard agreeing. The old row
// is now kept beside its replacement.
func TestRewrittenRowIsKeptWhenTheRewriteSaysNothingOfIt(t *testing.T) {
	const old = "bastion SSH uses port 2222 with a hardware key"
	const next = "operator access is fronted by Cloudflare Access"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: opID1, Category: "gotcha", Content: old},
		{ID: opID2, Category: "fact", Content: "the ledger ingests batches over gRPC"},
	}}
	written, drops := passThroughResult(t, in, fmtRewrite(next))
	if !written[old] {
		t.Errorf("the rewritten row was deleted on the rewrite's say-so alone, not kept: %v", written)
	}
	if !auditContains(drops, old) {
		t.Errorf("the rewritten row was not flagged for re-add: %+v", drops)
	}
}

// TestRewrittenRowIsNotReAddedWhenTheRewriteKeepsTheSubstance is the accept
// side. A rewrite that carries the memory's substance — the ports, the hosts,
// the versions — scores past the bar, so the ordinary audit finds the
// replacement on its own and the old row is not re-added beside it. This is the
// paraphrase-duplicate class #639 measured, and it is why the rule is a token
// audit rather than a blanket refusal to dispose of anything.
func TestRewrittenRowIsNotReAddedWhenTheRewriteKeepsTheSubstance(t *testing.T) {
	const old = "bastion SSH uses port 2222 with a hardware key"
	const next = "bastion SSH on port 2222 is now opened with Cloudflare Access"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: opID1, Category: "gotcha", Content: old},
		{ID: opID2, Category: "fact", Content: "the ledger ingests batches over gRPC"},
	}}
	written, drops := passThroughResult(t, in, fmtRewrite(next))
	if written[old] {
		t.Errorf("a rewrite carrying the substance re-added the old row beside it: %v", written)
	}
	if len(drops) != 0 {
		t.Errorf("a rewrite carrying the substance was still flagged: %+v", drops)
	}
}

// bulletErr is bullet without the t.Fatalf, so a test can assert the refusal
// instead of merely provoking it. A Fatalf inside a helper fails the test that
// called it, which leaves "the guard fired" unpinnable — removing the guard would
// leave every other assertion green, verified by mutation.
func bulletErr(prompt, marker string) (string, error) {
	if n := strings.Count(prompt, marker); n != 1 {
		return "", fmt.Errorf("marker %s appears %d times in the prompt, want exactly 1; a test using it would inspect the wrong line", marker, n)
	}
	_, after, ok := strings.Cut(prompt, marker)
	if !ok {
		return "", fmt.Errorf("the prompt has no %s bullet at all", marker)
	}
	line, _, ok := strings.Cut(after, "\n")
	if !ok {
		return "", fmt.Errorf("the %s bullet is the last line, so it cannot be delimited", marker)
	}
	return line, nil
}

// bullet returns the single prompt line beginning at marker, so a contract can be
// asserted on the bullet that carries it rather than on the prompt as a whole. A
// shared literal across two bullets would pin neither: a change that put the
// wrong wording on one while the other stayed right would pass.
//
// The marker must be UNIQUE, and that is checked rather than assumed.
// BuildReflectionPrompt renders untrusted stored memory above the ops contract,
// so a memory whose content quotes the ops vocabulary would put a second copy of
// the marker in the prompt — and strings.Cut returns the FIRST match, which
// would be that memory's line. Every "must not contain" assertion below would
// then pass while inspecting nothing about the ops bullet, which is the exact
// masking these tests exist to prevent.
func bullet(t *testing.T, prompt, marker string) string {
	t.Helper()
	line, err := bulletErr(prompt, marker)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// TestPromptTellsTheModelARewriteMayComeBack is the prompt half of the KEEP rule,
// and it is a test because the drift is silent: the guard changed and the prompt
// did not, so on the unattended path the model was told a rewrite or a
// supersession is final when neither is. Every such operation the corpus cannot
// account for then becomes a paraphrase duplicate that every later pass reads,
// which is the cost the rule accepts — accepted, but not intended.
//
// The prompt already carried the equivalent warning for a merge (mergeTail) and
// an obsolete drop (staleTail), so this asserts the two REPLACING operations got
// the same signal rather than asserting a new idea.
func TestPromptTellsTheModelARewriteMayComeBack(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "the bastion is reached on port 2222"},
	}})

	for _, want := range []string{
		`"rewrite <id> -> <text>"`,         // the bullet exists at all
		"CARRY the old memory's substance", // the replacement must explain the row
		"puts that row back verbatim",      // and what happens when it does not
		"until a later pass demotes",       // so the model knows it is not permanent either
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not tell the model %q about a rewrite that does not account for its row", want)
		}
	}

	// The supersession bullet is the one the hole was reached through, so it must
	// carry the signal too and not only inherit it from the rewrite bullet.
	sup := bullet(t, prompt, `"drop <id> reason: superseded by <id>"`)
	if !strings.Contains(sup, "puts that row back verbatim") {
		t.Errorf("the supersession bullet still reads as final; it needs the same warning as a rewrite")
	}
	// The two spliced clauses must be separated by a real sentence boundary.
	// This one is here because the comment on staleTail claims the full stop
	// matters and nothing else checked it: without it the harness is handed
	// "…is undone by the verbatim re-add State an obsolete drop only when…".
	obsolete := bullet(t, prompt, `"drop <id> reason: obsolete"`)
	if !strings.Contains(obsolete, "verbatim re-add. State an obsolete drop") {
		t.Errorf("the obsolete bullet runs its two spliced clauses together: %q", obsolete)
	}
}

// TestPromptDoesNotPromiseAReAddUnderAllowDrops is the mode half, and it exists
// because the first version of that sentence was emitted in both modes. Under
// --allow-drops the verbatim re-add is skipped entirely, so a prompt that says
// "the apply puts that row back verbatim" there promises a save the run never
// makes — on the path eval/cycle measures, which passes --apply --allow-drops
// for every reflect. The grader would be told a rewrite is free when the row is
// in fact deleted.
//
// Each case is checked on the BULLET that carries it rather than on the prompt
// as a whole, so a re-add leaking into the obsolete bullet cannot be masked by
// replaceTail's wording and vice versa. A shared literal across the two would
// pin neither: a change that put the default tail back on one bullet while the
// other still carried the right one would pass.
func TestPromptDoesNotPromiseAReAddUnderAllowDrops(t *testing.T) {
	// A stored memory quoting the ops vocabulary puts a SECOND copy of the marker
	// in the prompt, ahead of the real contract, and strings.Cut returns the
	// first — so every "must not contain" assertion below would inspect that
	// memory's line and pass while checking nothing. bulletErr refuses instead,
	// and this asserts the refusal rather than provoking it.
	collide := []memory.Memory{{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha",
		Content: `the ledger README says to write "rewrite <id> -> <text>" somewhere`}}
	colliding := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: collide, AllowDrops: true})
	if n := strings.Count(colliding, `"rewrite <id> -> <text>"`); n < 2 {
		t.Fatalf("collision fixture: marker appears %d times, want 2 so the first match is the memory's, not the bullet's", n)
	}
	if line, err := bulletErr(colliding, `"rewrite <id> -> <text>"`); err == nil {
		t.Errorf("bulletErr returned a line for a duplicated marker (%q); every assertion using it would read the wrong line", line)
	}

	memories := []memory.Memory{{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "the bastion is reached on port 2222"}}
	dropping := BuildReflectionPrompt(ReflectionInput{ProjectName: "ghost", ExistingMemories: memories, AllowDrops: true})

	rewrite := bullet(t, dropping, `"rewrite <id> -> <text>"`)
	if strings.Contains(rewrite, "back verbatim") {
		t.Errorf("the rewrite bullet promises a re-add the apply skips: %q", rewrite)
	}
	if !strings.Contains(rewrite, "the input is DELETED, and yours is the last version of it") {
		t.Errorf("the rewrite bullet does not say the replaced row is deleted: %q", rewrite)
	}

	supersede := bullet(t, dropping, `"drop <id> reason: superseded by <id>"`)
	if strings.Contains(supersede, "back verbatim") {
		t.Errorf("the supersession bullet promises a re-add the apply skips: %q", supersede)
	}

	obsolete := bullet(t, dropping, `"drop <id> reason: obsolete"`)
	if strings.Contains(obsolete, "back verbatim") {
		t.Errorf("the obsolete bullet promises a re-add the apply skips: %q", obsolete)
	}
	if !strings.Contains(obsolete, "since a drop nothing explains is a real deletion.") {
		t.Errorf("the obsolete bullet does not carry the mode's own staleTail: %q", obsolete)
	}

	// The OTHER splice site for staleTail, in the "drop stale situational
	// memories" rule. It is a different bullet reached through a different
	// template line, and a default tail leaking in here would ship an
	// --allow-drops prompt promising a re-add the apply never performs with every
	// other assertion still green.
	stale := bullet(t, dropping, "- Drop stale situational memories")
	if strings.Contains(stale, "undone by the verbatim re-add") {
		t.Errorf("the stale-memories rule promises a re-add the apply skips: %q", stale)
	}
	if !strings.Contains(stale, "since a drop nothing explains is a real deletion.") {
		t.Errorf("the stale-memories rule does not carry the mode's own staleTail: %q", stale)
	}
}

// TestMergeSourceIsScoredAgainstItsOwnMerge: a merged source was scored against
// the UNION of every output memory. That was sound before the pass-through
// existed, when the union was a handful of survivors; now every id the response
// never named is emitted verbatim, so the union of a real result is the whole
// project's vocabulary and a source whose substance its own merge discarded
// passes containment on the strength of whatever unrelated memory happens to
// share its words. That is the same class of loss the pass-through was written to
// remove — an unrelated survivor sharing 45% of the absorbed memory's tokens —
// reintroduced through the merge branch.
//
// The fixture is built so the union DOES rescue the source: the kept OIDC note
// covers 0.889 of it. Only the merge's own text fails to (0.200), because the
// merge carried the SSH note and silently dropped the OIDC one.
func TestMergeSourceIsScoredAgainstItsOwnMerge(t *testing.T) {
	const (
		sshID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		oidc  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		kept  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
	)
	sshMem := "the bastion accepts SSH on port 2222"
	oidcMem := "the ledger OIDC discovery document is served by idp.internal on port 8443"
	keptMem := "the OIDC issuer is idp.internal and its discovery document is served on port 8443"
	merged := "the bastion accepts SSH on port 2222"

	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: sshID, Category: "gotcha", Content: sshMem},
		{ID: oidc, Category: "architecture", Content: oidcMem},
		{ID: kept, Category: "architecture", Content: keptMem},
	}}
	result := ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "gotcha", Content: merged},
			{Category: "architecture", Content: keptMem},
		},
		Merges: []Merge{{IDs: []string{sshID, oidc}, Text: merged}},
	}

	drops := AuditGuardedDrops(in, result)
	if !auditContains(drops, oidcMem) {
		t.Fatalf("a merge that dropped this source deleted it unreported, on the strength of an unrelated memory: %+v", drops)
	}
	if auditContains(drops, sshMem) {
		t.Errorf("the source the merge did carry was flagged: %+v", drops)
	}
	if auditContains(drops, keptMem) {
		t.Errorf("an untouched kept memory was flagged: %+v", drops)
	}
}

// TestMergeSourcesAreAbsorbedWhenTheMergeCarriesThem is the accept side: a merge
// whose text carries every source it folded in still absorbs all of them, or
// scoring against the merge alone would re-add a duplicate beside the merge that
// had just replaced it. Both sources are covered well past the bar (0.800 and
// 0.667).
//
// It does NOT distinguish the merge's own text from the per-output test: the
// fixture has one output and it IS the merge text, so both comparisons see the
// same tokens. TestMergeSourceIsScoredAgainstItsOwnMerge is the one that needs an
// unrelated second memory to tell the two apart, and it is the one that fails if
// the union comes back.
func TestMergeSourcesAreAbsorbedWhenTheMergeCarriesThem(t *testing.T) {
	const (
		sshID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		oidc  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
	)
	sshMem := "the bastion accepts SSH on port 2222"
	oidcMem := "the ledger OIDC discovery document is served by idp.internal on port 8443"
	merged := "the bastion accepts SSH on 2222 and OIDC discovery is served by idp.internal on 8443"

	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: sshID, Category: "gotcha", Content: sshMem},
		{ID: oidc, Category: "architecture", Content: oidcMem},
	}}
	result := ReflectionResult{
		Memories: []ReflectMemory{{Category: "architecture", Content: merged}},
		Merges:   []Merge{{IDs: []string{sshID, oidc}, Text: merged}},
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Fatalf("a merge that carried both sources still lost one: %+v", drops)
	}
}

// TestMergeSourceIsAuditedWhenItsOwnMergeIsGone keeps the lapse behaviour: a
// merge a post-filter removed must not leave its source exempt. With no witness
// the source goes back under the ordinary per-output audit, which is the right
// question once there is no merge to be spread across.
func TestMergeSourceIsAuditedWhenItsOwnMergeIsGone(t *testing.T) {
	const sshID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
	sshMem := "the bastion accepts SSH on port 2222"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: sshID, Category: "gotcha", Content: sshMem},
	}}
	result := ReflectionResult{
		// The merge text is gone from Memories, so the claim cannot be witnessed.
		Memories: []ReflectMemory{{Category: "fact", Content: "Production runs in region fsn1"}},
		Merges:   []Merge{{IDs: []string{sshID}, Text: "the bastion accepts SSH on port 2222"}},
	}
	if drops := AuditGuardedDrops(in, result); !auditContains(drops, sshMem) {
		t.Fatalf("a source whose merge was filtered away went unreported: %+v", drops)
	}
}

// TestSummarizingMergeReAddsTheSourceItCompressed pins the direction the
// narrowing above deliberately accepts, so it is a decision rather than a
// side-effect. A merge that SUMMARISES its sources — the normal shape of a real
// consolidation, where the prompt asks for a corpus of high-quality memories and
// not a short one — will often carry one source's substance past the bar and
// compress another's below it. The carried one is absorbed; the compressed one
// comes back verbatim, beside the merge that absorbed its sibling.
//
// That is a paraphrase duplicate, the class #639 measured on every project. It is
// the price of the narrowing and it is a deliberate one: the alternatives are a
// silent deletion and a silent duplicate, and only one of those is recoverable.
// Containment here is 0.800 and 0.250.
//
// What this fixture pins is the OUTCOME for a summarizing merge, not the choice
// of witness set — with one output the union and the merge's own text are the
// same tokens. TestMergeSourceIsScoredAgainstItsOwnMerge is the one that needs
// the extra unrelated memory to tell them apart, and it fails if the union
// comes back.
func TestSummarizingMergeReAddsTheSourceItCompressed(t *testing.T) {
	const (
		sshID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		docID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
	)
	carried := "the bastion accepts SSH on port 2222"
	compressed := "OIDC discovery is documented in the ledger service README under auth"
	const merged = "the bastion accepts SSH on 2222 and OIDC discovery is served by idp.internal on 8443"

	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: sshID, Category: "gotcha", Content: carried},
		{ID: docID, Category: "fact", Content: compressed},
	}}
	result := ReflectionResult{
		Memories: []ReflectMemory{{Category: "architecture", Content: merged}},
		Merges:   []Merge{{IDs: []string{sshID, docID}, Text: merged}},
	}

	drops := AuditGuardedDrops(in, result)
	if !auditContains(drops, compressed) {
		t.Fatalf("a source the merge compressed below the bar was treated as absorbed: %+v", drops)
	}
	if auditContains(drops, carried) {
		t.Errorf("the source the merge carried past the bar was flagged: %+v", drops)
	}
}
