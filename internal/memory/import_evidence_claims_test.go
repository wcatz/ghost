package memory

import (
	"context"
	"strings"
	"testing"
)

// An evidence record's own stamps and its rating are read by the save tool's
// rules, exactly as the memory's own validity triple and confidence are.
//
// The evidence loop judged an evidence record's kind, its token-guarded fields
// and its id, and nothing else — so an artifact could carry
// `{"kind":"verified","observed_at":"not a date","verified_at":"not a date","confidence":99}`
// and the import stored it. The support summary then counted that record as a
// verification nobody could read, and the context label reported the memory as
// supported by verified observations on the strength of values the save path
// would never have written.
func TestImportedEvidenceClaimsAreRefusedByName(t *testing.T) {
	conf := func(f float64) *float64 { return &f }
	for _, tc := range []struct {
		name  string
		ev    PortableEvidence
		field string
	}{
		{"unreadable observed_at", PortableEvidence{Kind: "verified", ObservedAt: stampPtr("not a date")}, "evidence[0].observed_at"},
		{"unreadable verified_at", PortableEvidence{Kind: "verified", VerifiedAt: stampPtr("not a date")}, "evidence[0].verified_at"},
		{"confidence above one", PortableEvidence{Kind: "verified", Confidence: conf(99)}, "evidence[0].confidence"},
		{"confidence below zero", PortableEvidence{Kind: "verified", Confidence: conf(-0.1)}, "evidence[0].confidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := PortableMemory{
				ID: "m-evidence", ProjectID: "p1", Category: "fact", Source: "mcp",
				Content:  "a body carrying an unreadable claim",
				Evidence: []PortableEvidence{tc.ev},
			}
			if err := CheckImportedMemory(m); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("CheckImportedMemory = %v, want a refusal naming %q", err, tc.field)
			}
			for _, apply := range []bool{false, true} {
				row, err := importStampFixture(t, m, apply)
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Errorf("ImportMemory(apply=%v) = %v, want a refusal naming %q", apply, err, tc.field)
				}
				if row != nil {
					t.Errorf("ImportMemory(apply=%v) wrote the refused record", apply)
				}
			}
		})
	}
}

// The claims a reader can interpret are kept, and a stamp in the stored layout
// lands byte for byte — the export writes that layout for every row the writer
// tools wrote, so a round trip has to be byte-stable.
func TestImportedEvidenceKeepsAReadableStampAndRating(t *testing.T) {
	observed := "2026-09-20 08:15:30"
	verified := "2026-09-21 09:00:00"
	conf := 0.8
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	created, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: "m-evidence", ProjectID: "p1", Category: "fact", Source: "mcp",
		Content: "a body carrying a readable claim",
		Evidence: []PortableEvidence{{
			Kind: "verified", Agent: "claude-code",
			ObservedAt: &observed, VerifiedAt: &verified, Confidence: &conf,
		}},
	}, ImportOptions{Apply: true})
	if err != nil || !created {
		t.Fatalf("ImportMemory: created=%v err=%v", created, err)
	}
	records, err := s.MemoryProvenance(ctx, "m-evidence")
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	var carried *Evidence
	for i := range records {
		if records[i].Agent == "claude-code" {
			carried = &records[i]
		}
	}
	if carried == nil {
		t.Fatalf("the carried evidence record is missing: %+v", records)
	}
	if carried.ObservedAt == nil || *carried.ObservedAt != observed {
		t.Errorf("observed_at = %v, want the stored layout %q", carried.ObservedAt, observed)
	}
	if carried.VerifiedAt == nil || *carried.VerifiedAt != verified {
		t.Errorf("verified_at = %v, want the stored layout %q", carried.VerifiedAt, verified)
	}
	if carried.Confidence == nil || *carried.Confidence != conf {
		t.Errorf("confidence = %v, want %v", carried.Confidence, conf)
	}
}

// A stamp a person hand-edited is read the way the save tool reads one: RFC 3339
// is a moment, and the stored layout is what every reader of the column parses.
// So the value is normalized rather than refused, and what lands in the column is
// a moment a reader can interpret.
func TestImportedEvidenceReadsAnRFC3339StampAsTheSaveToolDoes(t *testing.T) {
	observed := "2026-10-01T09:00:00Z"
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	created, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: "m-evidence", ProjectID: "p1", Category: "fact", Source: "mcp",
		Content:  "a body carrying a hand-typed moment",
		Evidence: []PortableEvidence{{Kind: "observed", ObservedAt: &observed}},
	}, ImportOptions{Apply: true})
	if err != nil || !created {
		t.Fatalf("ImportMemory: created=%v err=%v", created, err)
	}
	records, err := s.MemoryProvenance(ctx, "m-evidence")
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	for _, r := range records {
		// The arrival record the import appends carries the store's own clock,
		// so the carried one is the record under test.
		if r.Kind != "observed" {
			continue
		}
		if r.ObservedAt == nil || *r.ObservedAt != "2026-10-01 09:00:00" {
			t.Errorf("observed_at = %v, want the stored layout %q", r.ObservedAt, "2026-10-01 09:00:00")
		}
		return
	}
	t.Errorf("the carried evidence record is missing: %+v", records)
}

// An absent claim is not a claim: a record carrying an empty stamp stores NULL
// rather than a value every reader has to know to ignore, exactly as the
// memory's own triple does.
func TestImportedEvidenceStoresAnEmptyStampAsNoClaim(t *testing.T) {
	empty := "   "
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	created, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: "m-evidence", ProjectID: "p1", Category: "fact", Source: "mcp",
		Content:  "a body carrying no claim at all",
		Evidence: []PortableEvidence{{Kind: "observed", ObservedAt: &empty}},
	}, ImportOptions{Apply: true})
	if err != nil || !created {
		t.Fatalf("ImportMemory: created=%v err=%v", created, err)
	}
	records, err := s.MemoryProvenance(ctx, "m-evidence")
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	for _, r := range records {
		// The arrival record the import appends carries the store's own clock,
		// so the carried one is the record under test.
		if r.Kind != "observed" {
			continue
		}
		if r.ObservedAt != nil {
			t.Errorf("observed_at = %q, want NULL: a blank stamp is no claim", *r.ObservedAt)
		}
		return
	}
	t.Errorf("the carried evidence record is missing: %+v", records)
}
