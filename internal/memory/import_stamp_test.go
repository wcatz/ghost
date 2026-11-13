package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// importStampFixture imports one memory into a fresh store holding project p1
// and returns the error ImportMemory gave and the row as the store then holds
// it (nil when no row was written).
func importStampFixture(t *testing.T, m PortableMemory, apply bool) (*PortableMemory, error) {
	t.Helper()
	ctx := context.Background()
	s := portableTestStore(t)
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	m.ProjectID = "p1"
	if m.Category == "" {
		m.Category = "fact"
	}
	if m.Source == "" {
		m.Source = "mcp"
	}
	if m.Content == "" {
		m.Content = "the staging endpoint is retired at the end of the year"
	}
	_, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: apply, TrustProvenance: true})
	rows, rerr := s.PortableMemories(ctx, []string{"p1"})
	if rerr != nil {
		t.Fatalf("PortableMemories: %v", rerr)
	}
	for i := range rows {
		if rows[i].ID == m.ID {
			return &rows[i], err
		}
	}
	return nil, err
}

func derefStamp(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// importedStamps imports m and fails unless the stored valid_from, valid_until
// and verified_at read back as want (nil for NULL).
func importedStamps(t *testing.T, m PortableMemory, want [3]string) *PortableMemory {
	t.Helper()
	row, err := importStampFixture(t, m, true)
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	for i, got := range []*string{row.ValidFrom, row.ValidUntil, row.VerifiedAt} {
		if g := derefStamp(got); (want[i] == "" && got != nil) || (want[i] != "" && g != want[i]) {
			t.Errorf("%s stored as %q, want %q", []string{"valid_from", "valid_until", "verified_at"}[i], g, want[i])
		}
	}
	return row
}

// An imported stamp is read with the forms the save tool documents: RFC 3339
// (before this rule it was stored verbatim and read as no claim, so an expired
// fact came back live) is normalized to the stored layout, in UTC.
func TestImportMemoryNormalizesAnRFC3339Stamp(t *testing.T) {
	row := importedStamps(t, PortableMemory{
		ID:         "m-rfc",
		ValidFrom:  stampPtr("2024-06-01T09:00:00+02:00"),
		ValidUntil: stampPtr("2025-01-01T00:00:00Z"),
		VerifiedAt: stampPtr("2024-09-20T12:30:45Z"),
	}, [3]string{"2024-06-01 07:00:00", "2025-01-01 00:00:00", "2024-09-20 12:30:45"})
	if state, _ := ValidityState(row.ValidFrom, row.ValidUntil, row.VerifiedAt, mustParseStamp(t, "2026-10-17 00:00:00")); state != ValidityExpired {
		t.Errorf("state at 2026-10-17 = %q, want %q", state, ValidityExpired)
	}
}

// A bare date as valid_until means the end of that day (the save tool's rule); as
// valid_from or verified_at it is midnight. A same-day window is a real window.
func TestImportMemoryReadsABareDateValidUntilAsTheEndOfThatDay(t *testing.T) {
	row := importedStamps(t, PortableMemory{
		ID:         "m-day",
		ValidFrom:  stampPtr("2026-12-31"),
		ValidUntil: stampPtr("2026-12-31"),
		VerifiedAt: stampPtr("2026-09-20"),
	}, [3]string{"2026-12-31 00:00:00", "2026-12-31 23:59:59", "2026-09-20 00:00:00"})
	if state, _ := ValidityState(row.ValidFrom, row.ValidUntil, row.VerifiedAt, mustParseStamp(t, "2026-12-31 12:00:00")); state == ValidityExpired {
		t.Errorf("state at noon on 2026-12-31 = %q; the claim was stated through that day", state)
	}
}

// A value already in the stored layout (what `ghost export` writes) lands byte
// for byte: the end-of-day rule is the bare date's alone.
func TestImportMemoryKeepsAStoredLayoutStampUnchanged(t *testing.T) {
	importedStamps(t, PortableMemory{
		ID:         "m-stored",
		ValidFrom:  stampPtr("2026-01-01 00:00:00"),
		ValidUntil: stampPtr("2026-12-31 00:00:00"),
		VerifiedAt: stampPtr("2026-09-20 08:15:30"),
	}, [3]string{"2026-01-01 00:00:00", "2026-12-31 00:00:00", "2026-09-20 08:15:30"})
}

// An empty stamp is no claim, as it is to the save tool: stored as NULL.
func TestImportMemoryStoresAnEmptyStampAsNoClaim(t *testing.T) {
	importedStamps(t, PortableMemory{ID: "m-empty", ValidUntil: stampPtr(""), VerifiedAt: stampPtr("   ")}, [3]string{})
}

// A record carrying a value the save tool refuses is refused by name, in the
// dry run and in the apply alike, and no row is written: an unreadable stamp,
// a window that ends before it starts (judged after normalization, so an
// RFC 3339 pair is judged too), and a confidence outside 0 to 1.
func TestImportMemoryRefusesWhatTheSaveToolRefuses(t *testing.T) {
	conf := func(f float64) *float64 { return &f }
	for _, tc := range []struct {
		name  string
		m     PortableMemory
		field string
	}{
		{"junk valid_until", PortableMemory{ValidUntil: stampPtr("next tuesday")}, "valid_until must be RFC 3339"},
		{"junk valid_from", PortableMemory{ValidFrom: stampPtr("sometime last spring")}, "valid_from must be RFC 3339"},
		{"junk verified_at", PortableMemory{VerifiedAt: stampPtr("last Tuesday, give or take")}, "verified_at must be RFC 3339"},
		{"inverted window", PortableMemory{
			ValidFrom: stampPtr("2026-12-01 00:00:00"), ValidUntil: stampPtr("2026-01-01 00:00:00"),
		}, "is not after valid_from"},
		{"inverted RFC 3339 window", PortableMemory{
			ValidFrom: stampPtr("2026-12-01T00:00:00Z"), ValidUntil: stampPtr("2026-01-01T00:00:00Z"),
		}, "is not after valid_from"},
		{"confidence 7", PortableMemory{Confidence: conf(7)}, "confidence must be between 0 and 1"},
		{"confidence below zero", PortableMemory{Confidence: conf(-0.1)}, "confidence must be between 0 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.ID = "m-bad"
			record := tc.m
			record.ProjectID, record.Category, record.Source, record.Content = "p1", "fact", "mcp", "a body"
			if err := CheckImportedMemory(record); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("CheckImportedMemory = %v, want a refusal naming %q", err, tc.field)
			}
			for _, apply := range []bool{false, true} {
				row, err := importStampFixture(t, tc.m, apply)
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Errorf("ImportMemory(apply=%v) = %v, want a refusal naming %q", apply, err, tc.field)
				}
				if row != nil {
					t.Errorf("ImportMemory(apply=%v) wrote the refused record", apply)
				}
			}
		})
	}
	// The bounds themselves are ratings, as they are to the save tool.
	for _, c := range []float64{0, 1} {
		if _, err := importStampFixture(t, PortableMemory{ID: "m-edge", Confidence: conf(c)}, true); err != nil {
			t.Errorf("confidence %v refused: %v", c, err)
		}
	}
}

func mustParseStamp(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, ok := ParseStamp(s)
	if !ok {
		t.Fatalf("ParseStamp(%q) failed", s)
	}
	return parsed
}

// A stored-layout value is kept as its raw text, fractional second included:
// a re-format would truncate both ends to the same second and refuse a valid
// window every reader orders.
func TestImportMemoryKeepsAFractionalStoredStampRaw(t *testing.T) {
	importedStamps(t, PortableMemory{
		ID:         "m-frac",
		ValidFrom:  stampPtr("2026-01-01 00:00:00.2"),
		ValidUntil: stampPtr("2026-01-01 00:00:00.8"),
	}, [3]string{"2026-01-01 00:00:00.2", "2026-01-01 00:00:00.8", ""})
}
