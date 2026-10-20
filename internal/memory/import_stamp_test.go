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

// An imported stamp is read with the forms the save tool documents. RFC 3339 is
// the form a person hand-editing an artifact is most likely to type, and before
// this rule it was stored verbatim and read by every reader as no claim at all,
// so an expired fact came back as live. It is normalized to the stored layout,
// in UTC, exactly as ghost_memory_save stores it.
func TestImportMemoryNormalizesAnRFC3339Stamp(t *testing.T) {
	row, err := importStampFixture(t, PortableMemory{
		ID:         "m-rfc",
		ValidFrom:  stampPtr("2024-06-01T09:00:00+02:00"),
		ValidUntil: stampPtr("2025-01-01T00:00:00Z"),
		VerifiedAt: stampPtr("2024-09-20T12:30:45Z"),
	}, true)
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	for _, f := range []struct{ name, got, want string }{
		{"valid_from", derefStamp(row.ValidFrom), "2024-06-01 07:00:00"},
		{"valid_until", derefStamp(row.ValidUntil), "2025-01-01 00:00:00"},
		{"verified_at", derefStamp(row.VerifiedAt), "2024-09-20 12:30:45"},
	} {
		if f.got != f.want {
			t.Errorf("%s stored as %q, want the stored layout %q", f.name, f.got, f.want)
		}
	}
	// And the row now reads as what it says: a window that closed in January 2025.
	if state, _ := ValidityState(row.ValidFrom, row.ValidUntil, row.VerifiedAt, mustParseStamp(t, "2026-10-17 00:00:00")); state != ValidityExpired {
		t.Errorf("state at 2026-10-17 = %q, want %q", state, ValidityExpired)
	}
}

// A bare date as valid_until means the end of that day, the save tool's rule:
// "valid until 2026-12-31" is true through the 31st. A bare date as valid_from or
// verified_at is midnight. A same-day window is a real window, not an inverted
// one, because the end is normalized before the order is judged.
func TestImportMemoryReadsABareDateValidUntilAsTheEndOfThatDay(t *testing.T) {
	row, err := importStampFixture(t, PortableMemory{
		ID:         "m-day",
		ValidFrom:  stampPtr("2026-12-31"),
		ValidUntil: stampPtr("2026-12-31"),
		VerifiedAt: stampPtr("2026-09-20"),
	}, true)
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	for _, f := range []struct{ name, got, want string }{
		{"valid_from", derefStamp(row.ValidFrom), "2026-12-31 00:00:00"},
		{"valid_until", derefStamp(row.ValidUntil), "2026-12-31 23:59:59"},
		{"verified_at", derefStamp(row.VerifiedAt), "2026-09-20 00:00:00"},
	} {
		if f.got != f.want {
			t.Errorf("%s stored as %q, want %q", f.name, f.got, f.want)
		}
	}
	if state, _ := ValidityState(row.ValidFrom, row.ValidUntil, row.VerifiedAt, mustParseStamp(t, "2026-12-31 12:00:00")); state == ValidityExpired {
		t.Errorf("state at noon on 2026-12-31 = %q; the claim was stated through that day", state)
	}
}

// A value already in the stored layout is what `ghost export` writes for every
// row the save tool wrote, so it must land byte for byte. Midnight on a
// valid_until is a time the writer chose, and the end-of-day rule is the bare
// date's alone.
func TestImportMemoryKeepsAStoredLayoutStampUnchanged(t *testing.T) {
	row, err := importStampFixture(t, PortableMemory{
		ID:         "m-stored",
		ValidFrom:  stampPtr("2026-01-01 00:00:00"),
		ValidUntil: stampPtr("2026-12-31 00:00:00"),
		VerifiedAt: stampPtr("2026-09-20 08:15:30"),
	}, true)
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	for _, f := range []struct{ name, got, want string }{
		{"valid_from", derefStamp(row.ValidFrom), "2026-01-01 00:00:00"},
		{"valid_until", derefStamp(row.ValidUntil), "2026-12-31 00:00:00"},
		{"verified_at", derefStamp(row.VerifiedAt), "2026-09-20 08:15:30"},
	} {
		if f.got != f.want {
			t.Errorf("%s stored as %q, want it unchanged as %q", f.name, f.got, f.want)
		}
	}
}

// An empty stamp is no claim, as it is to the save tool, and is stored as NULL
// rather than as an empty string a reader has to know to ignore.
func TestImportMemoryStoresAnEmptyStampAsNoClaim(t *testing.T) {
	row, err := importStampFixture(t, PortableMemory{
		ID:         "m-empty",
		ValidUntil: stampPtr(""),
		VerifiedAt: stampPtr("   "),
	}, true)
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	if row.ValidUntil != nil || row.VerifiedAt != nil {
		t.Errorf("stored valid_until=%q verified_at=%q, want both NULL", derefStamp(row.ValidUntil), derefStamp(row.VerifiedAt))
	}
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

// A stored-layout value is kept as its raw text, fractional second included.
// time.Parse accepts a fraction the layout does not name, and every reader
// (ParseStamp, CheckWindowOrder, readableStampSQL) orders this window; a
// re-format would truncate both ends to the same second and refuse a valid
// window over values the row does not hold.
func TestImportMemoryKeepsAFractionalStoredStampRaw(t *testing.T) {
	row, err := importStampFixture(t, PortableMemory{
		ID:         "m-frac",
		ValidFrom:  stampPtr("2026-01-01 00:00:00.2"),
		ValidUntil: stampPtr("2026-01-01 00:00:00.8"),
	}, true)
	if err != nil {
		t.Fatalf("ImportMemory refused a window every reader orders: %v", err)
	}
	if row == nil {
		t.Fatal("no row was written")
	}
	if got := derefStamp(row.ValidFrom); got != "2026-01-01 00:00:00.2" {
		t.Errorf("valid_from stored as %q, want the raw %q", got, "2026-01-01 00:00:00.2")
	}
	if got := derefStamp(row.ValidUntil); got != "2026-01-01 00:00:00.8" {
		t.Errorf("valid_until stored as %q, want the raw %q", got, "2026-01-01 00:00:00.8")
	}
}
