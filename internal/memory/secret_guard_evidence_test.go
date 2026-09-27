package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestImportedEvidenceIsGuardedLikeTheMemoryRow covers the one writer this change
// added that takes caller-supplied text: an artifact's nested evidence records.
//
// The rule it defends is the one `secret_guard.go` states — source_ref IS checked
// on the portable import, "the one route where it is a file's content rather than
// the harness's own identity" — and the nested records are that route one level
// deeper. The memory row's guard cannot see them, because the memory row does not
// hold them, which is exactly what makes the gap a gap: the evidence table would
// be the one place a credential survives, in an append-only store that the next
// `ghost export` re-emits and that only the purge's explicit DELETE reaches.
func TestImportedEvidenceIsGuardedLikeTheMemoryRow(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	// Each field the record carries, because a guard added for one of three and
	// forgotten for the other two is a guard that reads as coverage.
	for _, field := range []string{"agent", "session_id", "source_ref"} {
		t.Run(field, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			record := PortableEvidence{Kind: "observed"}
			switch field {
			case "agent":
				record.Agent = credential
			case "session_id":
				record.SessionID = credential
			case "source_ref":
				record.SourceRef = credential
			}

			for _, apply := range []bool{false, true} {
				_, _, _, err := s.ImportMemory(ctx, PortableMemory{
					ID:        "evguard00000000000000000000AA",
					ProjectID: testProject,
					Category:  "fact",
					Content:   "an ordinary imported fact",
					Source:    "onboarding",
					Evidence:  []PortableEvidence{record},
				}, ImportOptions{Apply: apply})
				if err == nil {
					t.Fatalf("apply=%v: ImportMemory accepted a credential in evidence[0].%s", apply, field)
				}
				if want := fmt.Sprintf("evidence[0].%s", field); !strings.Contains(err.Error(), want) {
					t.Errorf("apply=%v: the refusal does not name the field (%v): %v", apply, want, err)
				}
				if strings.Contains(err.Error(), credential) {
					t.Errorf("apply=%v: the refusal printed the value: %v", apply, err)
				}
			}
		})
	}

	// A dry run that refuses what the apply run refuses is what makes the preview
	// worth running, and it is the half a guard placed after the apply=false
	// return would silently get wrong. The loop above runs both modes; this
	// asserts nothing was written by the dry-run refusal.
	t.Run("a refused record writes nothing", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
			ID:        "evguard00000000000000000000AA",
			ProjectID: testProject,
			Category:  "fact",
			Content:   "an ordinary imported fact",
			Source:    "onboarding",
			Evidence:  []PortableEvidence{{Kind: "observed", SourceRef: credential}},
		}, ImportOptions{Apply: false}); err == nil {
			t.Fatal("the dry run accepted a credential in an evidence record")
		}
		rows, err := s.PortableMemories(ctx, nil)
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("the refused dry run wrote %d memories", len(rows))
		}
		var evidence int
		if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance`).Scan(&evidence); err != nil {
			t.Fatalf("count evidence rows: %v", err)
		}
		if evidence != 0 {
			t.Errorf("the refused record wrote %d evidence row(s)", evidence)
		}
	})

	// The ARRIVAL record copies the artifact's own agent, session and source_ref
	// onto a memory_provenance row, so those fields are guarded at the memory
	// level too — once, where the import already refuses source_ref, rather than
	// twice with one copy unguarded. An artifact carrying a credential-shaped agent
	// is refused even with no evidence records at all, because the arrival record
	// is written either way.
	for _, field := range []string{"agent", "session_id"} {
		t.Run("arrival record "+field, func(t *testing.T) {
			s := testStore(t)
			record := PortableMemory{
				ID:        "evguard00000000000000000000AA",
				ProjectID: testProject,
				Category:  "fact",
				Content:   "an ordinary imported fact",
				Source:    "onboarding",
			}
			if field == "agent" {
				record.Agent = credential
			} else {
				record.SessionID = credential
			}
			if _, _, _, err := s.ImportMemory(context.Background(), record, ImportOptions{Apply: true}); err == nil {
				t.Fatalf("ImportMemory accepted a credential in the artifact's %s — the arrival record copies it onto a memory_provenance row", field)
			} else if !strings.Contains(err.Error(), field) {
				t.Errorf("the refusal does not name the field: %v", err)
			}
		})
	}

	// The other half of a guard: ordinary evidence records still import, in both
	// modes, or the check is a filter rather than a rule.
	t.Run("ordinary evidence still imports", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const id = "evok000000000000000000000000AA"
		if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
			ID:        id,
			ProjectID: testProject,
			Category:  "fact",
			Content:   "an ordinary imported fact",
			Source:    "onboarding",
			Evidence: []PortableEvidence{{
				Kind: "observed", Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/runbook.md",
			}},
		}, ImportOptions{Apply: true}); err != nil {
			t.Fatalf("ImportMemory refused ordinary evidence: %v", err)
		}
		ev, err := s.MemoryProvenance(ctx, id)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		// The carried record plus the arrival, which is what an import always adds.
		if len(ev) != 2 || ev[0].SourceRef != "docs/runbook.md" || ev[0].Agent != "claude-code" {
			t.Errorf("evidence = %+v, want the carried record and the arrival", ev)
		}
	})
}
