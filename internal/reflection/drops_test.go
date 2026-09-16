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

// TestAuditGuardedDrops_IgnoresUnguardedCategories: fact/architecture memories
// are free to be consolidated away — only gotcha/dependency/preference/
// convention are guarded.
func TestAuditGuardedDrops_IgnoresUnguardedCategories(t *testing.T) {
	input := ReflectionInput{ExistingMemories: []memory.Memory{
		mem("fact", "The queue once used Redis lists with manual requeues"),
		mem("preference", "Wayne prefers PRs under 400 changed lines"),
	}}
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "fact", Content: "The job queue uses Redis Streams"},
	}}
	drops := AuditGuardedDrops(input, result)
	if len(drops) != 1 || drops[0].Category != "preference" {
		t.Fatalf("want only the preference flagged, got %+v", drops)
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
