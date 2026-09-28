package memory

import (
	"context"
	"strings"
	"testing"
)

// The import's counterpart (TestImportedEvidenceIsGuardedLikeTheMemoryRow) covers
// the route where a provenance value is a FILE's content. This covers the other
// one, and it is a different kind of claim.
//
// The import needs a guard inside appendEvidenceTx's caller because the memory
// row's guard cannot see the nested records at all. The harness path needs
// NOTHING added, because every evidence append there writes the same Provenance
// value, in the same transaction, after the same rejectSecret — so the evidence
// rows cannot hold a value the guard did not refuse. That is coverage by
// construction, and construction is exactly the kind of property that decays
// silently: the day a writer appends evidence from a Provenance its guard never
// saw, nothing in the code says so and every test still passes.
//
// So this asserts the property directly, for the one field on the harness path a
// caller controls: `source_ref` became a tool argument in this PR, and it is
// copied onto an evidence row as well as the memory row. The assertion is on the
// TABLE, not on the memory — a guard that refused the memory but let the
// evidence copy through would pass a test that only read memories.
func TestObservedEvidenceCarriesNoUnguardedProvenance(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	// Every writer that appends an evidence row carrying a caller's source_ref on
	// the harness path. A writer added to this list is a writer whose append needs
	// checking, and a writer that appends WITHOUT being listed is the bug this
	// test is here to catch.
	//
	// The assertion is on the VALUE, not on a row count: some of these writers
	// have to write a legitimate row first to have an id to edit, so a count would
	// need a per-case baseline and would then be checking bookkeeping. "No row
	// carries the credential" is the property itself, it holds whatever else the
	// table holds, and it cannot be satisfied by refusing the memory row while
	// letting the evidence copy through.
	for _, tc := range []struct {
		name string
		// write is the call under test; it must refuse.
		write func(*Store, string) error
	}{
		{
			name: "Create",
			write: func(s *Store, ref string) error {
				_, err := s.Create(context.Background(), testProject, Memory{
					Category: "fact", Content: "an ordinary fact with a hostile reference",
					Source: "mcp", SourceRef: ref,
				})
				return err
			},
		},
		{
			name: "UpsertWithOptions",
			write: func(s *Store, ref string) error {
				_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact",
					"an ordinary fact with a hostile reference", "mcp", 0.6, nil,
					UpsertOptions{Provenance: Provenance{Agent: "opencode", SourceRef: ref}})
				return err
			},
		},
		{
			name: "UpdateMemoryWithOptions",
			write: func(s *Store, ref string) error {
				id, _, _, err := s.Upsert(context.Background(), testProject, "fact",
					"a fact that will later be edited", "mcp", 0.6, nil)
				if err != nil {
					return err
				}
				return s.UpdateMemoryWithOptions(context.Background(), testProject, id,
					UpdateOptions{Provenance: Provenance{Agent: "opencode", SourceRef: ref}})
			},
		},
		{
			name: "ImportMemory",
			write: func(s *Store, ref string) error {
				_, _, _, err := s.ImportMemory(context.Background(), PortableMemory{
					ID: "harnessguard000000000000000AA", ProjectID: testProject,
					Category: "fact", Content: "an imported fact with a hostile reference",
					Source: "onboarding", SourceRef: ref,
				}, ImportOptions{Apply: true})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			if err := tc.write(s, credential); err == nil {
				t.Fatal("the write was accepted; the evidence row now holds the value too")
			} else if !strings.Contains(err.Error(), "source_ref") {
				t.Errorf("the refusal does not name the field: %v", err)
			} else if strings.Contains(err.Error(), credential) {
				t.Errorf("the refusal printed the value: %v", err)
			}
			if n := evidenceRowsCarrying(t, s, credential); n != 0 {
				t.Errorf("%d evidence row(s) carry the credential a refused write put there", n)
			}
		})
	}

	// The other half, or the check is a filter: an ordinary reference reaches BOTH
	// copies, so a reader counting evidence sees the reference the tool was given.
	t.Run("an ordinary reference reaches both copies", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a fact read out of a runbook", "mcp", 0.6, nil,
			UpsertOptions{Provenance: Provenance{Agent: "opencode", SourceRef: "docs/runbook.md"}})
		if err != nil {
			t.Fatalf("UpsertWithOptions refused an ordinary reference: %v", err)
		}
		records, err := s.MemoryProvenance(ctx, id)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		if len(records) != 1 {
			t.Fatalf("evidence = %d records, want 1", len(records))
		}
		if records[0].SourceRef != "docs/runbook.md" {
			t.Errorf("evidence source_ref = %q, want the reference the tool was given", records[0].SourceRef)
		}
		if records[0].Agent != "opencode" {
			t.Errorf("evidence agent = %q, want opencode — the same Provenance the row was written with", records[0].Agent)
		}
	})

	// And the verification record is a copy too, so a verified save's reference
	// lands in the table twice. This is the copy the `verified` append added in
	// this PR, and it is why that append is behind the same guard rather than a
	// fresh one.
	t.Run("a verification carries the same guarded reference", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		verified := verifiedStamp("2026-09-20 00:00:00")
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a fact that was checked against a runbook", "mcp", 0.6, nil,
			UpsertOptions{
				Provenance: Provenance{Agent: "opencode", SourceRef: "docs/runbook.md"},
				Validity:   Validity{VerifiedAt: verified},
			})
		if err != nil {
			t.Fatalf("UpsertWithOptions: %v", err)
		}
		records, err := s.MemoryProvenance(ctx, id)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		var verifications int
		for _, r := range records {
			if r.Kind == evidenceVerified {
				verifications++
				if r.SourceRef != "docs/runbook.md" {
					t.Errorf("the verified record's source_ref = %q, want the guarded value", r.SourceRef)
				}
			}
		}
		if verifications != 1 {
			t.Errorf("%d verified record(s), want 1", verifications)
		}
	})
}

// evidenceRowsCarrying counts evidence rows whose source_ref, agent or session_id
// holds ref. A guard that refused the memory row but let the evidence copy through
// leaves one, so this is the assertion that would catch it.
func evidenceRowsCarrying(t *testing.T, s *Store, ref string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_provenance
		 WHERE source_ref = ? OR agent = ? OR session_id = ?`, ref, ref, ref,
	).Scan(&n); err != nil {
		t.Fatalf("count evidence rows carrying a value: %v", err)
	}
	return n
}

// The guard names the field, not the value, on every path — a refusal that
// echoed the credential would put it in a log, which is the one place this store
// cannot redact afterwards. Cheap to assert and the failure is unrecoverable, so
// it is pinned once here for the harness path the table test above walks.
func TestHarnessRefusalsNameTheFieldAndNotTheValue(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	s := testStore(t)

	_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact",
		"a fact with a hostile reference", "mcp", 0.6, nil,
		UpsertOptions{Provenance: Provenance{SourceRef: credential}})
	if err == nil {
		t.Fatal("UpsertWithOptions accepted a credential-shaped source_ref")
	}
	for _, unwanted := range []string{credential, "ghp_"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the refusal leaked part of the value (%q): %v", unwanted, err)
		}
	}
	if !strings.Contains(err.Error(), "source_ref") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}
