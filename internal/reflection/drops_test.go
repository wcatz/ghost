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

// TestAuditGuardedDrops_MergedInputIsJudgedAgainstItsOwnMerge: a merged source
// is scored against the text of the merge that folded it in. It used to be
// scored against the union of every output, which was sound before the
// pass-through existed — when the union was a handful of survivors — but is not
// now: the pass-through emits every id the response never named, so the union of
// a real result is the whole project's vocabulary and a source whose substance
// its own merge discarded passes on the strength of an unrelated memory sharing
// its words (#549). See TestMergeSourceIsScoredAgainstItsOwnMerge for the
// fixture that separates the two.
//
// The merge text here carries both sources past the bar, so nothing is flagged.
func TestAuditGuardedDrops_MergedInputIsJudgedAgainstItsOwnMerge(t *testing.T) {
	ssh := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH from the office is firewalled, so use port 2222"}
	region := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "Production runs in region fsn1"}
	const merged = "bastion SSH from the office is firewalled, use port 2222, and production runs in region fsn1"
	input := ReflectionInput{ExistingMemories: []memory.Memory{ssh, region}}

	result := ReflectionResult{
		Memories: []ReflectMemory{{Category: "gotcha", Content: merged}},
		Merges:   []Merge{{IDs: []string{ssh.ID, region.ID}, Text: merged}},
	}
	if drops := AuditGuardedDrops(input, result); len(drops) != 0 {
		t.Fatalf("a merge carrying both sources lost one: %+v", drops)
	}

	// Take the merge away. Neither source is carried through and no output
	// explains either, so both go back under the ordinary per-output audit.
	filtered := result
	filtered.Merges = nil
	filtered.Memories = []ReflectMemory{{Category: "fact", Content: "the ledger ingests batches over gRPC"}}
	drops := AuditGuardedDrops(input, filtered)
	if len(drops) != 2 {
		t.Fatalf("want both sources re-audited once the merge is gone, got %d: %+v", len(drops), drops)
	}
}

// TestAuditGuardedDrops_NonMergedInputIsStillJudgedAgainstOneOutput is the other
// half, and the reason the union is not applied to everything. An explicit drop
// names its id and emits nothing, so for that row the guard is the only thing
// standing between the harness's claim and a deletion — and it has to be
// answered by one survivor. A corpus-wide union would answer "absorbed" for any
// memory whose words also occur somewhere else, and honour the claim, which is
// the unattended loss #337/#549 exist to prevent.
func TestAuditGuardedDrops_NonMergedInputIsStillJudgedAgainstOneOutput(t *testing.T) {
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
		t.Fatalf("an explicitly dropped memory was absorbed by the corpus-wide vocabulary: %+v", drops)
	}
}

// TestAuditGuardedDrops_SupersessionIsJudgedLikeAnyOtherDrop is the KEEP bias
// (#549). An unattended reflect never deletes a memory on the model's say-so
// alone, so a `drop X reason: superseded by Y` is audited exactly like an
// `obsolete` drop: the corpus has to be able to SHOW that X is gone.
//
// The fixture is the original one, and it is the interesting one because the
// successor is a real, carried-forward row that genuinely does replace the stale
// one — operator access moved from a hardware key to Cloudflare Access. The two
// share no tokens at all (0.000), so the audit cannot tell that Y supersedes X,
// and X comes back. Demoting it afterwards is resolve and supersede's job, and
// they run right after reflect in the lifecycle. The alternative was X deleted
// from a corpus with nobody watching, and not repairable.
func TestAuditGuardedDrops_SupersessionIsJudgedLikeAnyOtherDrop(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"}
	fixed := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "operator access is now fronted by Cloudflare Access"}
	orphan := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3", Category: "gotcha", Content: "the metrics endpoint is bound to 12798"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fixed, orphan}}

	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: fixed.Content}},
		Replacements: []Replacement{{ID: stale.ID, Text: fixed.Content}},
	}
	drops := AuditGuardedDrops(input, result)

	// The stale row is KEPT: no output explains it, so it comes back verbatim.
	kept := map[string]bool{}
	for _, d := range drops {
		kept[d.Content] = true
	}
	if !kept[stale.Content] {
		t.Fatalf("a supersession deleted a memory the corpus could not show was gone: %+v", drops)
	}
	// The successor is a survivor and must not be re-added, or the round would
	// resurrect the thing that replaced it.
	if kept[fixed.Content] {
		t.Errorf("the successor was re-added: %+v", drops)
	}
	// And it is not a blanket "every named drop is kept": the memory nobody
	// claimed is audited the same way it always was.
	if !kept[orphan.Content] {
		t.Errorf("an unclaimed memory stopped being audited: %+v", drops)
	}
}

// TestAuditGuardedDrops_SupersessionNearTheBarKeepsTheStaleRow is the case that
// decides whether the rule is a rule or a threshold tuned to a fixture: a real
// supersession from this repository's own history, where the successor is a
// reworded description of the same change. It scores 0.429 against the bar, so
// the stale row is kept. Someone reading "reflection calls the Anthropic HTTP API
// client directly" beside "Reflection now classifies through the calling CLI
// harness subprocess; no direct API client" can see that the first is stale; the
// audit cannot, and erring towards keeping it is the whole point.
func TestAuditGuardedDrops_SupersessionNearTheBarKeepsTheStaleRow(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "fact", Content: "reflection calls the Anthropic HTTP API client directly"}
	fresh := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "Reflection now classifies through the calling CLI harness subprocess; no direct API client"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fresh}}

	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: fresh.Content}},
		Replacements: []Replacement{{ID: stale.ID, Text: fresh.Content}},
	}
	if drops := AuditGuardedDrops(input, result); !auditContains(drops, stale.Content) {
		t.Fatalf("a near-the-bar supersession disposed of the stale row: %+v", drops)
	}
}

// TestAuditGuardedDrops_SupersessionTheCorpusDoesExplainStillPasses is the other
// side, and it is what makes the removal of the exemption free rather than a
// blanket refusal. A successor that DOES restate the fact scores past the bar, so
// the ordinary audit finds it on its own and the stale row is not re-added. The
// witness is not consulted at all; the row simply has a survivor. Containment
// here is 0.714.
func TestAuditGuardedDrops_SupersessionTheCorpusDoesExplainStillPasses(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH on port 2222 is opened with a hardware key"}
	fresh := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "bastion SSH on port 2222 is now opened with Cloudflare Access"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fresh}}

	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: fresh.Content}},
		Replacements: []Replacement{{ID: stale.ID, Text: fresh.Content}},
	}
	if drops := AuditGuardedDrops(input, result); auditContains(drops, stale.Content) {
		t.Fatalf("a successor that restates the fact should have kept the stale row out: %+v", drops)
	}
}

// TestAuditGuardedDrops_AKeptStaleRowIsDemotableNotDeleted: the reason the
// supersession exemption is gone, stated as the property it trades for. A
// superseded row the corpus cannot explain is KEPT, and a kept row is repairable
// — the lifecycle runs resolve and supersede straight after reflect, and
// demoting a row that is genuinely stale is their job. Had the exemption stayed,
// the row would have been deleted from a corpus with nobody watching, and
// nothing downstream could bring it back.
func TestAuditGuardedDrops_AKeptStaleRowIsDemotableNotDeleted(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"}
	fixed := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "operator access is now fronted by Cloudflare Access"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fixed}}
	result := ReflectionResult{
		Memories:     []ReflectMemory{{Category: "fact", Content: fixed.Content}},
		Replacements: []Replacement{{ID: stale.ID, Text: fixed.Content}},
	}

	// Retaining the flagged drop is what puts the row back in the result, byte
	// for byte, so ReplaceNonManual's exact-content reuse keeps its id, age,
	// embedding and links (#452) — it is the same row, not a copy.
	drops := AuditGuardedDrops(input, result)
	result.Memories = append(result.Memories, RetainGuardedDrops(drops)...)
	found := false
	for _, m := range result.Memories {
		if m.Content == stale.Content {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stale row did not survive the round at all: %+v", result.Memories)
	}
	if remaining := AuditGuardedDrops(input, result); len(remaining) != 0 {
		t.Fatalf("retaining the stale row left %d rows still uncovered: %+v", len(remaining), remaining)
	}
}

// TestAuditGuardedDrops_AKeptSupersededRowIsDemotableNotDeleted is the same
// property through the real ops path, and it is what a filter that removes the
// successor used to be able to hide behind. dropForeignProjectMemories deletes a
// memory naming a project the input corpus never mentioned, and a merge is
// exactly such a memory; with the exemption gone there is nothing to lapse, and
// the successor the filter removed is audited on its own merits like any other
// row.
func TestAuditGuardedDrops_AKeptSupersededRowIsDemotableNotDeleted(t *testing.T) {
	stale := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"}
	fixed := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "operator access is now fronted by Cloudflare Access"}
	input := ReflectionInput{ExistingMemories: []memory.Memory{stale, fixed}}

	result := ReflectionResult{
		Memories:     nil, // the successor was removed from the result
		Merges:       []Merge{{IDs: []string{fixed.ID}, Text: fixed.Content}},
		Replacements: []Replacement{{ID: stale.ID, Text: fixed.Content}},
	}
	drops := AuditGuardedDrops(input, result)
	retained := map[string]bool{}
	for _, d := range drops {
		retained[d.Content] = true
	}
	if !retained[stale.Content] {
		t.Fatalf("the stale row went when its successor was filtered away: %+v", drops)
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
