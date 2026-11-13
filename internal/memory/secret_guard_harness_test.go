package memory

import (
	"context"
	"strings"
	"testing"
)

func TestObservedEvidenceCarriesNoUnguardedProvenance(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	for _, tc := range []struct {
		name  string
		write func(*Store, string) error
	}{
		{"Create", func(s *Store, ref string) error {
			_, err := s.Create(context.Background(), testProject, Memory{Category: "fact", Content: "an ordinary fact with a hostile reference", Source: "mcp", SourceRef: ref})
			return err
		}},
		{"UpsertWithOptions", func(s *Store, ref string) error {
			_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "an ordinary fact with a hostile reference", "mcp", 0.6, nil, UpsertOptions{Provenance: Provenance{Agent: "opencode", SourceRef: ref}})
			return err
		}},
		{"UpdateMemoryWithOptions", func(s *Store, ref string) error {
			id, _, _, err := s.Upsert(context.Background(), testProject, "fact", "a fact that will later be edited", "mcp", 0.6, nil)
			if err != nil {
				return err
			}
			return s.UpdateMemoryWithOptions(context.Background(), testProject, id, UpdateOptions{Provenance: Provenance{Agent: "opencode", SourceRef: ref}})
		}},
		{"ImportMemory", func(s *Store, ref string) error {
			_, _, _, err := s.ImportMemory(context.Background(), PortableMemory{ID: "harnessguard000000000000000AA", ProjectID: testProject, Category: "fact", Content: "an imported fact with a hostile reference", Source: "onboarding", SourceRef: ref}, ImportOptions{Apply: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			err := tc.write(s, credential)
			if err == nil || !strings.Contains(err.Error(), "source_ref") || strings.Contains(err.Error(), credential) {
				t.Fatalf("err=%v", err)
			}
			if n := evidenceRowsCarrying(t, s, credential); n != 0 {
				t.Fatalf("evidence rows=%d", n)
			}
		})
	}
	t.Run("an ordinary reference reaches both copies", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a fact read out of a runbook", "mcp", 0.6, nil, UpsertOptions{Provenance: Provenance{Agent: "opencode", SourceRef: "docs/runbook.md"}})
		if err != nil {
			t.Fatal(err)
		}
		records, err := s.MemoryProvenance(ctx, id)
		if err != nil || len(records) != 1 || records[0].SourceRef != "docs/runbook.md" || records[0].Agent != "opencode" {
			t.Fatalf("records=%+v err=%v", records, err)
		}
	})
	t.Run("a verification carries the same guarded reference", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a fact that was checked against a runbook", "mcp", 0.6, nil, UpsertOptions{Provenance: Provenance{Agent: "opencode", SourceRef: "docs/runbook.md"}, Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")}})
		if err != nil {
			t.Fatal(err)
		}
		records, err := s.MemoryProvenance(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var verifications int
		for _, r := range records {
			if r.Kind == evidenceVerified {
				verifications++
				if r.SourceRef != "docs/runbook.md" {
					t.Fatalf("source_ref=%q", r.SourceRef)
				}
			}
		}
		if verifications != 1 {
			t.Fatalf("verifications=%d", verifications)
		}
	})
}

func evidenceRowsCarrying(t *testing.T, s *Store, ref string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE source_ref = ? OR agent = ? OR session_id = ?`, ref, ref, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHarnessRefusalsNameTheFieldAndNotTheValue(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	s := testStore(t)
	_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "a fact with a hostile reference", "mcp", 0.6, nil, UpsertOptions{Provenance: Provenance{SourceRef: credential}})
	if err == nil || !strings.Contains(err.Error(), "source_ref") || strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("err=%v", err)
	}
}
