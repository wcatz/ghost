package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestImportedEvidenceIsGuardedLikeTheMemoryRow(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
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
			default:
				record.SourceRef = credential
			}
			for _, apply := range []bool{false, true} {
				_, _, _, err := s.ImportMemory(ctx, PortableMemory{ID: "evguard00000000000000000000AA", ProjectID: testProject, Category: "fact", Content: "an ordinary imported fact", Source: "onboarding", Evidence: []PortableEvidence{record}}, ImportOptions{Apply: apply})
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("evidence[0].%s", field)) || strings.Contains(err.Error(), credential) {
					t.Fatalf("apply=%v err=%v", apply, err)
				}
			}
		})
	}
	t.Run("a refused record writes nothing", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if _, _, _, err := s.ImportMemory(ctx, PortableMemory{ID: "evguard00000000000000000000AA", ProjectID: testProject, Category: "fact", Content: "an ordinary imported fact", Source: "onboarding", Evidence: []PortableEvidence{{Kind: "observed", SourceRef: credential}}}, ImportOptions{Apply: false}); err == nil {
			t.Fatal("expected refusal")
		}
		rows, err := s.PortableMemories(ctx, nil)
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows=%d err=%v", len(rows), err)
		}
		var evidence int
		if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance`).Scan(&evidence); err != nil || evidence != 0 {
			t.Fatalf("evidence=%d err=%v", evidence, err)
		}
	})
	for _, field := range []string{"agent", "session_id"} {
		t.Run("arrival record "+field, func(t *testing.T) {
			s := testStore(t)
			record := PortableMemory{ID: "evguard00000000000000000000AA", ProjectID: testProject, Category: "fact", Content: "an ordinary imported fact", Source: "onboarding"}
			if field == "agent" {
				record.Agent = credential
			} else {
				record.SessionID = credential
			}
			if _, _, _, err := s.ImportMemory(context.Background(), record, ImportOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	t.Run("ordinary evidence still imports", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const id = "evok000000000000000000000000AA"
		if _, _, _, err := s.ImportMemory(ctx, PortableMemory{ID: id, ProjectID: testProject, Category: "fact", Content: "an ordinary imported fact", Source: "onboarding", Evidence: []PortableEvidence{{Kind: "observed", Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/runbook.md"}}}, ImportOptions{Apply: true}); err != nil {
			t.Fatal(err)
		}
		ev, err := s.MemoryProvenance(ctx, id)
		if err != nil || len(ev) != 2 || ev[0].SourceRef != "docs/runbook.md" || ev[0].Agent != "claude-code" {
			t.Fatalf("ev=%+v err=%v", ev, err)
		}
	})
}
