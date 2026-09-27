package reflection

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func mem(cat, content string) memory.Memory {
	return memory.Memory{Category: cat, Content: content}
}

// TestAuditGuardedDrops_FlagsDeletionWithoutMerge: a guarded-category input
// with no close survivor in the output must be flagged (issue #337, finding
// F3: the consolidator deleted bastion-port facts outright).
func TestAuditGuardedDrops_FlagsDeletionWithoutMerge(t *testing.T) {
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		mem("gotcha", "SSH to the Hetzner bastion goes through port 2222, not 22"),
		mem("fact", "Production runs in region fsn1 behind Cloudflare"),
	}}
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
		{Category: "architecture", Content: "Orchestrator splits into ingest and billing services"},
	}}
	drops := AuditGuardedDrops(input, result)
	if len(drops) != 1 {
		t.Fatalf("want 1 dropped guarded memory, got %d: %+v", len(drops), drops)
	}
	if !strings.Contains(drops[0].Content, "bastion") {
		t.Fatalf("wrong drop flagged: %+v", drops[0])
	}
}

// TestAuditGuardedDrops_MergedRewriteIsNotADrop: consolidation may rewrite a
// guarded memory into a survivor; token overlap must recognize it.
func TestAuditGuardedDrops_MergedRewriteIsNotADrop(t *testing.T) {
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		mem("gotcha", "Redis maxmemory must stay at 512mb on prod or the OOM killer reaps it during batch windows"),
	}}
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "gotcha", Content: "Redis is capped at maxmemory 512mb in production; raising it invites OOM kills during batch jobs"},
	}}
	if drops := AuditGuardedDrops(input, result); len(drops) != 0 {
		t.Fatalf("merged rewrite flagged as drop: %+v", drops)
	}
}

// TestAuditGuardedDrops_RetainsEveryCategory: the guard is not a category
// allowlist (#549). Under the old map only gotcha/dependency/preference/
// convention were audited, so an architecture or decision memory the
// consolidator simply omitted was deleted from the corpus unreported — the one
// loss an unattended --apply pass cannot be watched for. Every category is
// audited now, and a merged rewrite that preserves the input's substance still
// counts as a survivor, so retention does not duplicate it.
func TestAuditGuardedDrops_RetainsEveryCategory(t *testing.T) {
	const pattern = "Redis Streams consumer groups with explicit XACK"
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		mem("architecture", "Orchestrator splits into ingest and billing services"),
		mem("decision", "Chose Postgres over DynamoDB because the ledger is append-only"),
		mem("pattern", pattern),
	}}
	// The consolidator kept one memory, folding the pattern into it, and
	// dropped the other two outright.
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "pattern", Content: "Job consumption uses Redis Streams consumer groups with explicit XACK after handling"},
	}}

	drops := AuditGuardedDrops(input, result)
	if len(drops) != 2 {
		t.Fatalf("want 2 drops (architecture, decision), got %d: %+v", len(drops), drops)
	}
	flagged := map[string]bool{}
	for _, d := range drops {
		flagged[d.Category] = true
	}
	if !flagged["architecture"] || !flagged["decision"] {
		t.Fatalf("want the architecture and decision flagged, got %+v", drops)
	}
	if flagged["pattern"] {
		t.Fatalf("merged rewrite flagged as a drop: %+v", drops)
	}

	result.Memories = append(result.Memories, RetainGuardedDrops(drops)...)
	if remaining := AuditGuardedDrops(input, result); len(remaining) != 0 {
		t.Fatalf("retention left %d memories uncovered: %+v", len(remaining), remaining)
	}
	for _, m := range result.Memories {
		if m.Category == "pattern" && m.Content == pattern {
			t.Fatalf("surviving pattern re-added verbatim, so it is duplicated: %+v", m)
		}
	}
}

// TestAuditGuardedDrops_AddressedInputIsJudgedAgainstTheOutputUnion: a merge
// may carry an input's substance across a survivor plus a sibling, and scoring
// it against any one of them re-adds it verbatim beside the merge that had just
// absorbed it — the paraphrase-duplicate class #639 measured on every project.
// An input the response named (kept, merged, rewritten) is therefore measured
// against the union of the outputs.
//
// The fixture straddles the threshold on purpose: no single output covers 45% of
// the SSH note and the two together cover 86% of it, so the only thing the
// assertion turns on is which set the input is scored against.
func TestAuditGuardedDrops_AddressedInputIsJudgedAgainstTheOutputUnion(t *testing.T) {
	ssh := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH from the office is firewalled, so use port 2222"}
	region := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "Production runs in region fsn1"}
	kept := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3", Category: "fact", Content: "ssh from the office is firewalled for the whole subnet"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{ssh, region, kept}}

	// The merge folded the SSH and region notes together and dropped the words
	// "from the office"; the memory it did not touch carries them.
	result := ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "gotcha", Content: "the bastion is reached on port 2222 from the region fsn1"},
			{Category: "fact", Content: kept.Content},
		},
		AddressedIDs: []string{ssh.ID, region.ID},
	}

	strict := result
	strict.AddressedIDs = nil
	if drops := AuditGuardedDrops(input, strict); len(drops) != 1 || drops[0].Content != ssh.Content {
		t.Fatalf("a single-output comparison should have flagged the SSH note, got %+v", drops)
	}
	if drops := AuditGuardedDrops(input, result); len(drops) != 0 {
		t.Fatalf("an addressed input spread across the output union was flagged: %+v", drops)
	}
}

// TestAuditGuardedDrops_OmittedInputIsStillJudgedAgainstOneOutput is the other
// half, and the reason the union is not applied to everything. Under the per-id
// contract an unnamed id is an expected, explicitly cheap outcome, so this guard
// is now mostly asking "did the model absorb this memory or forget it" — and a
// corpus-wide union answers "absorbed" for any memory whose words also occur
// somewhere else, deleting it with no --allow-drops and no warning. That is the
// unattended loss #337/#549 exist to prevent, so an input nothing in the
// response accounted for must be explained by ONE survivor.
//
// The fixture straddles the threshold deliberately, and the test states both
// sides of the rule: no single output covers 45% of the Redis note and the three
// together cover 50% of it, so the same result flags the note as omitted and
// would absorb it if the response had addressed it. The region note is covered
// outright by one survivor, so the only thing under test is the Redis one.
func TestAuditGuardedDrops_OmittedInputIsStillJudgedAgainstOneOutput(t *testing.T) {
	redisNote := "Redis maxmemory must stay at 512mb in production or the OOM killer reaps the pod"
	regionNote := "Cloudflare fronts the production region fsn1"
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: redisNote},
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: regionNote},
	}}
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "fact", Content: "redis is the only stateful service"},
		{Category: "fact", Content: regionNote},
		{Category: "gotcha", Content: "the OOM killer reaps any worker over 512mb during a batch window"},
	}}

	drops := AuditGuardedDrops(input, result)
	if len(drops) != 1 || drops[0].Content != redisNote {
		t.Fatalf("an omitted memory was absorbed by the corpus-wide vocabulary: %+v", drops)
	}

	addressed := result
	addressed.AddressedIDs = []string{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"}
	if drops := AuditGuardedDrops(input, addressed); len(drops) != 0 {
		t.Fatalf("the same fixture must be absorbed once the response addressed it, else it pins nothing: %+v", drops)
	}
}

// TestAuditGuardedDrops_HonoursAnExplicitSupersession: the guard cannot tell
// "forgot" from "deliberately replaced", which is how a stale memory was
// restored beside the memory that replaced it (#639). An input the harness
// dropped as superseded by another input id, whose successor is still in the
// result, is a decision with a witness, so the guard does not undo it — while an
// input with no such claim is still audited, and the exemption is per-id rather
// than global.
func TestAuditGuardedDrops_HonoursAnExplicitSupersession(t *testing.T) {
	// The superseded row and its successor share almost no vocabulary — an old
	// access method replaced by a new one — so nothing but the explicit claim
	// distinguishes "the harness said so" from "the harness forgot".
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"}
	fixed := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "operator access is now fronted by Cloudflare Access"}
	orphan := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3", Category: "gotcha", Content: "the metrics endpoint is bound to 12798"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fixed, orphan}}

	result := ReflectionResult{
		Memories: []ReflectMemory{{Category: "fact", Content: fixed.Content}},
		Supersessions: []Supersession{{
			DroppedID: stale.ID, TargetID: fixed.ID, TargetText: fixed.Content,
		}},
	}
	drops := AuditGuardedDrops(input, result)
	if len(drops) != 1 || drops[0].Content != orphan.Content {
		t.Fatalf("want only the unrelated input audited, got %+v", drops)
	}
}

// TestAuditGuardedDrops_ForgetsASupersessionWhoseSuccessorIsGone: a filter that
// runs over the result after the operations can still remove the successor —
// dropForeignProjectMemories deletes a memory naming a project the input corpus
// never mentioned, and a merge is exactly such a memory. The stated supersession
// then names nothing, and honouring it would delete the dropped row with no
// replacement. The witness is the successor's text, so a claim whose text is no
// longer in the result lapses and both rows go back under the audit: the stale
// one is re-added, and so is the successor the filter removed.
func TestAuditGuardedDrops_ForgetsASupersessionWhoseSuccessorIsGone(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"}
	fixed := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "operator access is now fronted by Cloudflare Access"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fixed}}

	result := ReflectionResult{
		Memories:      nil, // the successor was removed from the result
		AddressedIDs:  []string{fixed.ID},
		Supersessions: []Supersession{{DroppedID: stale.ID, TargetID: fixed.ID, TargetText: fixed.Content}},
	}
	drops := AuditGuardedDrops(input, result)
	retained := map[string]bool{}
	for _, d := range drops {
		retained[d.Content] = true
	}
	if !retained[stale.Content] {
		t.Fatalf("a supersession outlived its successor and let the stale row go: %+v", drops)
	}
	if !retained[fixed.Content] {
		t.Fatalf("the successor the filter removed was not itself re-added: %+v", drops)
	}
}

// TestRetainGuardedDrops_CarriesFieldsVerbatim: retention must not launder the
// memory. Content stays byte-identical so ReplaceNonManual's exact-content
// reuse matches the original row and its embedding/links survive (#452), and
// importance/tags survive so re-insertion does not reweight or untag it.
func TestRetainGuardedDrops_CarriesFieldsVerbatim(t *testing.T) {
	drops := []DroppedGuarded{{
		Category:   "gotcha",
		Content:    "SSH to the Hetzner bastion goes through port 2222, not 22",
		Importance: 0.9,
		Tags:       []string{"ssh", "port"},
	}}
	retained := RetainGuardedDrops(drops)
	if len(retained) != 1 {
		t.Fatalf("want 1 retained memory, got %d", len(retained))
	}
	m := retained[0]
	if m.Category != "gotcha" {
		t.Errorf("category = %q, want gotcha", m.Category)
	}
	if m.Content != drops[0].Content {
		t.Errorf("content not verbatim: %q", m.Content)
	}
	if m.Importance != 0.9 {
		t.Errorf("importance = %v, want 0.9", m.Importance)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "ssh" || m.Tags[1] != "port" {
		t.Errorf("tags = %v, want [ssh port]", m.Tags)
	}
}

// TestRetainGuardedDrops_ZeroLoss is the invariant retention exists for: after
// retaining every flagged drop, no guarded input is uncovered. This is what
// lets reflect APPLY on a rich project instead of refusing (the pre-#458
// behaviour measured 1 success in 13 attempts).
func TestRetainGuardedDrops_ZeroLoss(t *testing.T) {
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		mem("gotcha", "SSH to the Hetzner bastion goes through port 2222, not 22"),
		mem("dependency", "gouroboros v0.204.3 has the zero-Normalize bug; v0.204.7 fixes it"),
		mem("fact", "Production runs in region fsn1"),
	}}
	// A consolidator that drops both guarded memories and keeps only the fact.
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
	}}

	drops := AuditGuardedDrops(input, result)
	if len(drops) != 2 {
		t.Fatalf("want 2 guarded drops before retention, got %d: %+v", len(drops), drops)
	}
	result.Memories = append(result.Memories, RetainGuardedDrops(drops)...)

	if remaining := AuditGuardedDrops(input, result); len(remaining) != 0 {
		t.Fatalf("retention left %d guarded memories uncovered: %+v", len(remaining), remaining)
	}
}
