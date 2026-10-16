package memory

import (
	"context"
	"strings"
	"testing"
)

// The dry run and the apply must agree about an evidence id: the id is the
// memory_provenance primary key, so a repeat is refused by the apply and the dry
// run has to say so too, rather than report a create.
func TestImportedEvidenceIDIsJudgedOnDryRunAndApply(t *testing.T) {
	evid := func(ids ...string) []PortableEvidence {
		out := make([]PortableEvidence, len(ids))
		for i, id := range ids {
			out[i] = PortableEvidence{ID: id, Kind: "observed"}
		}
		return out
	}
	cases := []struct {
		name string
		ev   []PortableEvidence
		want string
	}{
		{"repeated", evid("d", "d"), "evidence[1].id repeats"},
		{"whitespace only", evid("ok", "  "), "evidence[1].id must not be whitespace only"},
		{"newline", evid("a\nb"), "evidence[0].id must hold no control"},
		{"nul", evid("a\x00b"), "evidence[0].id must hold no control"},
		{"c1 next line", evid("a\u0085b"), "evidence[0].id must hold no control"},
		{"line separator", evid("a b"), "evidence[0].id must hold no control"},
		{"paragraph separator", evid("a", "a b"), "evidence[1].id must hold no control"},
		{"over bound", evid(strings.Repeat("x", MaxImportedIDLen+1)), "evidence[0].id must be at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			m := PortableMemory{
				ID: "evidid0000000000000000000000AA", ProjectID: testProject,
				Category: "fact", Content: "an ordinary imported fact", Source: "onboarding",
				Evidence: tc.ev,
			}
			if err := CheckImportedMemory(m); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckImportedMemory = %v, want it to contain %q", err, tc.want)
			}
			for _, apply := range []bool{false, true} {
				created, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("apply=%v: created=%v err=%v, want a refusal containing %q", apply, created, err, tc.want)
				}
			}
		})
	}
}

// The refusal never prints the offending text.
func TestImportedEvidenceIDRefusalNamesNoValue(t *testing.T) {
	err := CheckImportedMemory(PortableMemory{
		ID: "evidid0000000000000000000000AB", ProjectID: testProject,
		Category: "fact", Content: "c", Source: "onboarding",
		Evidence: []PortableEvidence{{ID: "leak\nsentinel", Kind: "observed"}},
	})
	if err == nil {
		t.Fatal("accepted a control character in an evidence id")
	}
	if strings.Contains(err.Error(), "sentinel") {
		t.Errorf("the refusal printed the id: %v", err)
	}
}

// A well-formed artifact still imports, and distinct ids are kept as given.
func TestImportedEvidenceDistinctIDsStillRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := PortableMemory{
		ID: "evidid0000000000000000000000AC", ProjectID: testProject,
		Category: "fact", Content: "an ordinary imported fact", Source: "onboarding",
		Evidence: []PortableEvidence{
			{ID: "ev-one", Kind: "observed"},
			{ID: "ev-two", Kind: "imported"},
			{ID: strings.Repeat("x", MaxImportedIDLen), Kind: "observed"},
		},
	}
	if err := CheckImportedMemory(m); err != nil {
		t.Fatalf("CheckImportedMemory refused a valid memory: %v", err)
	}
	for _, apply := range []bool{false, true} {
		created, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply})
		if err != nil || !created {
			t.Fatalf("apply=%v: created=%v err=%v", apply, created, err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_provenance WHERE id IN ('ev-one','ev-two')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("carried evidence ids = %d, err %v, want 2", n, err)
	}
}

// An empty evidence id is minted by the importer, so two of them are not a repeat.
func TestImportedEmptyEvidenceIDsAreMintedDistinct(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := PortableMemory{
		ID: "evidid0000000000000000000000AD", ProjectID: testProject,
		Category: "fact", Content: "an ordinary imported fact", Source: "onboarding",
		Evidence: []PortableEvidence{{Kind: "observed"}, {Kind: "observed"}},
	}
	for _, apply := range []bool{false, true} {
		created, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply})
		if err != nil || !created {
			t.Fatalf("apply=%v: created=%v err=%v", apply, created, err)
		}
	}
	var n, d int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT id) FROM memory_provenance WHERE memory_id = ? AND kind = 'observed'`, m.ID).Scan(&n, &d); err != nil || n != 2 || d != 2 {
		t.Fatalf("rows=%d distinct=%d err=%v, want 2 and 2", n, d, err)
	}
}

// An evidence id already stored on ANOTHER memory is the primary key of that
// row, so the importer mints a fresh id for the incoming record instead of
// failing the insert. The dry run reports the create the apply then makes, the
// first memory's row keeps its id, and re-importing the artifact is a skip.
func TestImportedEvidenceIDHeldByAnotherMemoryIsMinted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mk := func(id, content string) PortableMemory {
		return PortableMemory{
			ID: id, ProjectID: testProject, Category: "fact", Content: content, Source: "onboarding",
			Evidence: []PortableEvidence{{ID: "ev-1", Kind: "observed"}},
		}
	}
	a := mk("evidid0000000000000000000000BA", "the first memory")
	b := mk("evidid0000000000000000000000BB", "the second memory")
	if created, _, _, err := s.ImportMemory(ctx, a, ImportOptions{Apply: true}); err != nil || !created {
		t.Fatalf("seed memory A: created=%v err=%v", created, err)
	}
	if err := CheckImportedMemory(b); err != nil {
		t.Fatalf("CheckImportedMemory refused B, a judgement about the record alone: %v", err)
	}
	for _, apply := range []bool{false, true} {
		created, _, _, err := s.ImportMemory(ctx, b, ImportOptions{Apply: apply})
		if err != nil || !created {
			t.Fatalf("apply=%v: created=%v err=%v, want a create on both paths", apply, created, err)
		}
	}
	owner := func(id string) string {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT memory_id FROM memory_provenance WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("owner of %q: %v", id, err)
		}
		return got
	}
	if got := owner("ev-1"); got != a.ID {
		t.Fatalf("ev-1 now belongs to %s, want memory A to keep it", got)
	}
	count := func(memoryID string) int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_provenance WHERE memory_id = ? AND kind = 'observed'`, memoryID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(b.ID); n != 1 {
		t.Fatalf("B carries %d observed evidence rows, want its 1 record under a minted id", n)
	}
	// Re-importing the same artifact is idempotent: both memories exist, so both
	// are skipped, with no failure and no extra evidence.
	for _, m := range []PortableMemory{a, b} {
		for _, apply := range []bool{false, true} {
			created, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply})
			if err != nil || created {
				t.Fatalf("re-import %s apply=%v: created=%v err=%v, want a skip", m.ID, apply, created, err)
			}
		}
	}
	if n := count(a.ID); n != 1 {
		t.Fatalf("A has %d observed rows after the re-import, want 1", n)
	}
	if n := count(b.ID); n != 1 {
		t.Fatalf("B has %d observed rows after the re-import, want 1", n)
	}
}

// Two memories of one artifact that share an evidence id: the dry run writes
// nothing, so only the apply can see the second collide, and it must not fail.
func TestImportedEvidenceIDSharedWithinOneArtifactIsMinted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"evidid0000000000000000000000CA", "evidid0000000000000000000000CB"} {
		m := PortableMemory{
			ID: id, ProjectID: testProject, Category: "fact", Content: "content of " + id, Source: "onboarding",
			Evidence: []PortableEvidence{{ID: "shared-ev", Kind: "observed"}},
		}
		for _, apply := range []bool{false, true} {
			created, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply})
			if err != nil || !created {
				t.Fatalf("%s apply=%v: created=%v err=%v", id, apply, created, err)
			}
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT memory_id) FROM memory_provenance WHERE kind = 'observed' AND memory_id LIKE 'evidid%000C%'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("memories with evidence = %d, err %v, want 2", n, err)
	}
}
