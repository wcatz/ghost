package reflection

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// These pin the two ways a disposal claim could delete a memory an unattended
// apply never reports (#549): a witness that is PRESENT but has nothing to do
// with the memory it disposes of, and a merge source scored against every output
// at once. Both are silent — no warning, no --allow-drops, exit 0 — and both
// were reachable from an ordinary `ghost reflect --apply --require-llm`.

// TestUnrelatedSupersessionWitnessIsNotAnExemption: `drop X reason: superseded
// by Y` was honoured on one condition — Y's text is in the result — with no
// check that X and Y are about the same thing. A response that names any
// carried-forward id as the successor of an unrelated memory therefore deleted
// that memory outright. It is not a far-fetched response: the only parser rule
// is that the target is carried forward, so naming a neighbour is a valid
// operation, and nothing downstream ever compared the two texts.
//
// The pair is deliberately realistic rather than adversarial: both are ordinary
// deployment notes in the same project, and the model pairs them because they
// were adjacent in the prompt. The scoring is 0.000, so no threshold choice
// rescues this case — the texts share nothing.
func TestUnrelatedSupersessionWitnessIsNotAnExemption(t *testing.T) {
	const (
		tlsID    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		imageID  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		orphanID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
		tls      = "the staging cluster terminates TLS on port 8443"
		image    = "CI publishes a signed image tagged with the git sha"
		orphan   = "the ledger answers gRPC on port 9000"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: tlsID, Category: "architecture", Content: tls},
		{ID: imageID, Category: "convention", Content: image},
		{ID: orphanID, Category: "fact", Content: orphan},
	}}
	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "convention", Content: image}},
		Replacements: []Replacement{{ID: tlsID, Text: image, Supersession: true}},
	}

	drops := AuditGuardedDrops(in, result)
	if !auditContains(drops, tls) {
		t.Fatalf("a supersession naming an unrelated memory disposed of it unreported: %+v", drops)
	}
	if auditContains(drops, image) {
		t.Errorf("the named successor was itself flagged: %+v", drops)
	}
	// And the disposition is per-id: the memory nobody claimed is still audited,
	// so narrowing the witness rule must not have turned the exemption into a
	// blanket "the response disposed of it, never mind how".
	if !auditContains(drops, orphan) {
		t.Errorf("an unclaimed memory stopped being audited: %+v", drops)
	}
}

// TestRelatedSupersessionIsStillExempt is the accept side, and it is #659's own
// motivating example: a claim whose witness really does restate the same fact
// must not put the stale text back beside the text that replaced it, which is a
// duplicate by construction. Containment is 0.750 here, so the rule separates
// this from the unrelated pair above rather than rejecting every reworded
// supersession.
func TestRelatedSupersessionIsStillExempt(t *testing.T) {
	const (
		staleID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		freshID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		stale   = "three issues are still open"
		fresh   = "the three open issues have all been fixed"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: staleID, Category: "fact", Content: stale},
		{ID: freshID, Category: "fact", Content: fresh},
	}}
	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: fresh}},
		Replacements: []Replacement{{ID: staleID, Text: fresh, Supersession: true}},
	}
	if drops := AuditGuardedDrops(in, result); auditContains(drops, stale) {
		t.Fatalf("a genuine supersession re-added the stale text beside its replacement: %+v", drops)
	}
}

// TestRewriteWitnessIsNotReCheckedForRelatedness is the boundary of the
// supersession rule, and it is why the fix marks the claim kind rather than
// re-checking every disposition. A rewrite's witness is the model's own text FOR
// that row, so nothing forces it to echo the old wording — the operation exists
// precisely to change a claim — and the grounding check has already tied it to
// that row by rejecting a rewrite that introduces an identifier absent from its
// sources. Re-checking relatedness here would put every reworded rewrite back
// beside its replacement, which is the duplicate #659 removed. A supersession
// names a different row and gets the check; a rewrite does not.
func TestRewriteWitnessIsNotReCheckedForRelatedness(t *testing.T) {
	const (
		id   = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		old  = "the ledger is reached from the office subnet"
		next = "the ledger is reached over the bastion mesh"
	)
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: id, Category: "fact", Content: old},
	}}
	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: next}},
		Replacements: []Replacement{{ID: id, Text: next}},
	}
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Fatalf("a rewrite was re-added beside its own replacement: %+v", drops)
	}
}

// TestSupersessionFromTheOpsPathStillRequiresRelatedness guards the plumbing.
// The audit only applies the relatedness check to a claim executeOps marked as a
// supersession, so a result built the way the LLM tier actually builds one — not
// a hand-written fixture — has to reach the same verdict. If the drop path ever
// stopped setting the flag, every supersession would be read as a rewrite and
// this hole would reopen with nothing failing.
func TestSupersessionFromTheOpsPathStillRequiresRelatedness(t *testing.T) {
	in := opInput()
	// opID1 is the bastion SSH port and opID2 the production region: two
	// ordinary, unrelated notes, which is all it takes for a supersession to
	// dispose of something it says nothing about.
	result := opRun(t, in,
		`{"learned_context":"ctx","ops":["drop `+opID1+` reason: superseded by `+opID2+`","keep `+opID2+`"]}`)

	if len(result.Replacements) != 1 {
		t.Fatalf("replacements = %+v, want one", result.Replacements)
	}
	if !result.Replacements[0].Supersession {
		t.Fatal("the drop path did not mark the claim a supersession, so the guard reads it as a rewrite and skips the relatedness check")
	}
	if drops := AuditGuardedDrops(in, result); !auditContains(drops, in.ExistingMemories[0].Content) {
		t.Fatalf("an unrelated supersession disposed of a memory through the real ops path: %+v", drops)
	}
}

// TestMergeSourceIsScoredAgainstItsOwnMerge: a merged source was scored against
// the UNION of every output memory. That was sound before the pass-through
// existed, when the union was a handful of survivors; now every id the response
// never named is emitted verbatim, so the union of a real result is the whole
// project's vocabulary and a source whose substance its own merge threw away
// passes containment on the strength of some unrelated memory that happens to
// share its words. That is the same class of loss the pass-through was written
// to remove — an unrelated survivor sharing tokens with the absorbed memory —
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
// had just replaced it — the paraphrase-duplicate class #639 measured on every
// project. Both sources are covered well past the bar (0.800 and 0.667) and the
// fixture carries no other output to rescue them, so a regression that fell back
// to the per-output test would be caught here too.
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
