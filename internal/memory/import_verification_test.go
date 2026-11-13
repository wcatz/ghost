package memory

import (
	"context"
	"database/sql"
	"testing"
)

// The artifact already holds a stamped record the moment the origin store's own
// writers put one there, so the import's arrival record must not re-state the
// verification: a corpus exported and imported N times reports exactly 1
// verification of one real check, however many hops it takes (a floor is what
// let this through, so the assertion is exactly 1).
func TestImportDoesNotReinflateAVerificationTheArtifactAlreadyCarries(t *testing.T) {
	origin := testStore(t)
	ctx := context.Background()
	confidence := 0.8
	if err := origin.EnsureProject(ctx, testProject, "/tmp/origin-round-trip", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	originID, err := origin.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the mirror job is idempotent and re-runs from its own cursor", Source: "mcp", Importance: 0.7,
		Agent: "opencode", SourceRef: "docs/mirror.md", Confidence: &confidence,
		VerifiedAt: stampPtr("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hop1 := importOnce(t, origin, testProject, originID)
	if got := verifiedCount(t, hop1, originID); got != 1 {
		t.Fatalf("after one hop Verified = %d, want 1", got)
	}
	hop2 := importOnce(t, hop1, testProject, originID)
	if got := verifiedCount(t, hop2, originID); got != 1 {
		t.Errorf("after two hops Verified = %d, want 1 — a second hop re-inflated a check", got)
	}
}

// The kind-agnostic half: here the first hop's only stamped record is the
// ARRIVAL (kind imported), so a guard keyed on the carried kind would stamp the
// second arrival too.
func TestSecondHopDoesNotReinflateWhenOnlyTheArrivalIsStamped(t *testing.T) {
	verified := "2026-09-20 00:00:00"
	const id = "arrivalonly0000000000000000AA"
	hop1 := testStore(t)
	if _, _, _, err := hop1.ImportMemory(context.Background(), PortableMemory{
		ID: id, ProjectID: testProject, Category: "fact",
		Content: "a fact whose check lives only in the column",
		Source:  "onboarding", VerifiedAt: &verified,
		Evidence: []PortableEvidence{{Kind: "observed", Agent: "claude-code", ObservedAt: &verified}},
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("hop 1 ImportMemory: %v", err)
	}
	records, err := hop1.MemoryProvenance(context.Background(), id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	var stamped []string
	for _, r := range records {
		if r.VerifiedAt != nil {
			stamped = append(stamped, r.Kind)
		}
	}
	if len(stamped) != 1 || stamped[0] != evidenceImported {
		t.Fatalf("stamped record kinds = %v, want exactly %q", stamped, evidenceImported)
	}
	if got := verifiedCount(t, importOnce(t, hop1, testProject, id), id); got != 1 {
		t.Errorf("after hop 2 Verified = %d, want 1", got)
	}
}

// A memory whose artifact holds records but none stamped, or only an empty-string
// stamp (which is no claim), still gets the arrival stamp from its own column.
func TestImportStillStampsTheArrivalWhenNoCarriedRecordIsVerified(t *testing.T) {
	verified := "2026-09-20 00:00:00"
	empty := ""
	for _, tc := range []struct {
		name string
		ev   []PortableEvidence
	}{
		{"unstamped", []PortableEvidence{
			{Kind: "observed", Agent: "claude-code", ObservedAt: &verified},
			{Kind: "observed", Agent: "codex", ObservedAt: &verified},
		}},
		{"empty string", []PortableEvidence{
			{Kind: "observed", Agent: "claude-code", ObservedAt: &verified, VerifiedAt: &empty},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			const id = "arrivalstamp0000000000000000AA"
			if _, _, _, err := s.ImportMemory(context.Background(), PortableMemory{
				ID: id, ProjectID: testProject, Category: "fact", Content: "an artifact with no verified record",
				Source: "onboarding", VerifiedAt: &verified, Evidence: tc.ev,
			}, ImportOptions{Apply: true}); err != nil {
				t.Fatalf("ImportMemory: %v", err)
			}
			counts, err := s.MemoryEvidenceCounts(context.Background(), id)
			if err != nil {
				t.Fatalf("MemoryEvidenceCounts: %v", err)
			}
			if counts.Verified != 1 || counts.Observations != len(tc.ev)+1 {
				t.Errorf("Verified=%d Observations=%d, want 1 and %d (carried plus the stamped arrival)",
					counts.Verified, counts.Observations, len(tc.ev)+1)
			}
		})
	}
}

// ReplaceNonManual composes the successor's validity window from its sources: a
// complete readable ordered pair wins whichever id is named first, a window no
// layout reads (or the zero instant) is not a stated claim and is never written
// onto the successor, and halves of two rows are never composed into a pair
// neither asserted. Every case runs in both id orders.
func TestReplaceNonManualInheritsAWindowOnlyWhenASourceStatedIt(t *testing.T) {
	type win struct{ from, until, verified string }
	const (
		okFrom, okUntil = "2026-01-01 00:00:00", "2027-03-01 00:00:00"
	)
	for _, tc := range []struct {
		name     string
		sources  []win
		emitFrom *string
		check    func(f, u, v sql.NullString) string
	}{
		{"complete pair beats a half", []win{{okFrom, okUntil, ""}, {"2028-05-01 00:00:00", "", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.String != okFrom || u.String != okUntil {
					return "want the complete pair"
				}
				return ""
			}},
		{"unreadable window never outranks a real one", []win{{okFrom, okUntil, ""}, {"sometime last spring", "when the dust settles", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.String != okFrom || u.String != okUntil {
					return "want the readable pair"
				}
				return ""
			}},
		{"zero instant is not a stated window", []win{{okFrom, okUntil, ""}, {"0001-01-01 00:00:00", "0001-01-01 00:00:01", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.String != okFrom || u.String != okUntil {
					return "want the real pair"
				}
				return ""
			}},
		{"consistent single window is inherited whole", []win{{okFrom, "2027-12-31 23:59:59", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.String != okFrom || u.String != "2027-12-31 23:59:59" {
					return "want the source's window"
				}
				return ""
			}},
		{"halves of two rows are not composed", []win{{okFrom, "", ""}, {"", okUntil, ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.Valid && u.Valid {
					return "composed a pair across two rows"
				}
				if !f.Valid && !u.Valid {
					return "dropped a real half"
				}
				return ""
			}},
		{"a pair ending before it starts is never composed", []win{{"2030-01-01 00:00:00", "", ""}, {"", "2020-06-01 00:00:00", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.Valid && u.Valid {
					return "composed an out-of-order window"
				}
				return ""
			}},
		{"an out-of-order source window is not inherited", []win{{"2030-01-01 00:00:00", "2020-06-01 00:00:00", ""}},
			nil, func(f, u, _ sql.NullString) string {
				if f.Valid && u.Valid {
					return "inherited an out-of-order window"
				}
				return ""
			}},
		{"unreadable stamps are not written", []win{{"whenever the deploy lands", "soon after", "last Tuesday, give or take"}},
			nil, func(f, u, v sql.NullString) string {
				if f.Valid || u.Valid || v.Valid {
					return "wrote a stamp no layout reads"
				}
				return ""
			}},
		{"the empty moment is no claim", []win{{"", "", ""}},
			func() *string { e := ""; return &e }(), func(f, _, _ sql.NullString) string {
				if f.Valid {
					return "stored the empty moment instead of NULL"
				}
				return ""
			}},
	} {
		for _, reverse := range []bool{false, true} {
			name := tc.name
			if reverse {
				name += "/reversed"
			}
			t.Run(name, func(t *testing.T) {
				s := testStore(t)
				ctx := context.Background()
				var ids []string
				for i, w := range tc.sources {
					id, err := s.Create(ctx, testProject, Memory{
						Category: "fact", Content: "source row " + string(rune('A'+i)) + " of " + tc.name, Source: "mcp", Importance: 0.6,
						ValidFrom: optStamp(w.from), ValidUntil: optStamp(w.until), VerifiedAt: optStamp(w.verified),
					})
					if err != nil {
						t.Fatalf("Create: %v", err)
					}
					ids = append(ids, id)
				}
				if reverse {
					for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
						ids[i], ids[j] = ids[j], ids[i]
					}
				}
				content := "successor of " + tc.name
				if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
					Category: "fact", Content: content, Importance: 0.6, ValidFrom: tc.emitFrom, ReplacesIDs: ids,
				}}, "", nil); err != nil {
					t.Fatalf("ReplaceNonManual: %v", err)
				}
				var f, u, v sql.NullString
				if err := s.db.QueryRow(
					`SELECT valid_from, valid_until, verified_at FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
				).Scan(&f, &u, &v); err != nil {
					t.Fatalf("read the successor: %v", err)
				}
				if msg := tc.check(f, u, v); msg != "" {
					t.Errorf("%s (from=%v until=%v verified=%v)", msg, f, u, v)
				}
			})
		}
	}
}

// successorRow finds the live row a rewrite produced.
func successorRow(t *testing.T, s *Store, content string) Memory {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
	).Scan(&id); err != nil {
		t.Fatalf("find the successor row %q: %v", content, err)
	}
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(rows))
	}
	return rows[0]
}

// A fresh successor row carries the claims its carried evidence makes (the row
// and its table must not contradict each other); a value the emission states
// wins and silence inherits from the source; a source that recorded no claim
// gets a successor with none; and trust is the source's except the
// manual-to-reflection change consolidation exists to make.
func TestReplaceNonManualFreshRowCarriesTheSourcesClaims(t *testing.T) {
	ctx := context.Background()
	confidence := 0.7
	rewrite := func(t *testing.T, s *Store, src Memory, out Memory) Memory {
		t.Helper()
		id, err := s.Create(ctx, testProject, src)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		out.ReplacesIDs = []string{id}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{out}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		return successorRow(t, s, out.Content)
	}

	t.Run("columns follow the carried evidence", func(t *testing.T) {
		s := testStore(t)
		row := rewrite(t, s, Memory{
			Category: "architecture", Content: "the writer pins MaxOpenConns(1)", Source: "mcp", Importance: 0.8,
			Agent: "claude-code", SourceRef: "internal/memory/store.go", Confidence: &confidence,
			ValidFrom: stampPtr("2026-01-15 00:00:00"), ValidUntil: stampPtr("2027-12-31 23:59:59"),
			VerifiedAt: stampPtr("2026-09-20 00:00:00"),
		}, Memory{Category: "architecture", Content: "the writer pins MaxOpenConns(1) on purpose", Importance: 0.8})
		counts, err := s.MemoryEvidenceCounts(ctx, row.ID)
		if err != nil || counts.Verified != 1 {
			t.Fatalf("carried stamped records = %d (%v), want 1", counts.Verified, err)
		}
		if row.VerifiedAt == nil || row.ValidFrom == nil || *row.ValidFrom != "2026-01-15 00:00:00" ||
			row.ValidUntil == nil || *row.ValidUntil != "2027-12-31 23:59:59" {
			t.Errorf("successor window/verified_at = %v %v %v, want the source's", row.ValidFrom, row.ValidUntil, row.VerifiedAt)
		}
		if row.Confidence == nil || *row.Confidence != confidence || row.SourceRef != "internal/memory/store.go" || row.Agent != "claude-code" {
			t.Errorf("successor confidence/source_ref/agent = %v %q %q, want the source's", row.Confidence, row.SourceRef, row.Agent)
		}
	})
	t.Run("an unclaimed source yields an unclaimed successor", func(t *testing.T) {
		row := rewrite(t, testStore(t), Memory{Category: "fact", Content: "the mirror runs on a timer", Source: "mcp", Importance: 0.6},
			Memory{Category: "fact", Content: "the mirror runs on a timer and skips a failed cycle", Importance: 0.6})
		if row.VerifiedAt != nil || row.ValidFrom != nil || row.ValidUntil != nil || row.Confidence != nil || row.SourceRef != "" || row.Agent != "" {
			t.Errorf("successor of an unclaimed row carries a claim: %+v", row)
		}
	})
	t.Run("a manual source keeps its author and reference", func(t *testing.T) {
		c := 0.3
		row := rewrite(t, testStore(t), Memory{Category: "gotcha", Content: "the FTS leg truncates at ten terms", Source: "manual", Importance: 0.5,
			Agent: "codex", SourceRef: "PR-1", Confidence: &c},
			Memory{Category: "gotcha", Content: "the FTS leg truncates its query terms at ten", Importance: 0.5})
		if row.Source != "reflection" || row.Agent != "codex" || row.SourceRef != "PR-1" {
			t.Errorf("successor provenance = (%q, %q, %q), want (reflection, codex, PR-1)", row.Source, row.Agent, row.SourceRef)
		}
	})
	t.Run("the emission's stated claim beats the source's", func(t *testing.T) {
		restated := "2027-01-01 00:00:00"
		row := rewrite(t, testStore(t), Memory{
			Category: "architecture", Content: "the read replica lags by one commit", Source: "mcp", Importance: 0.7,
			Agent: "claude-code", SourceRef: "docs/old.md", ValidFrom: stampPtr("2026-01-01 00:00:00"), VerifiedAt: stampPtr("2026-09-20 00:00:00"),
		}, Memory{
			Category: "architecture", Content: "the read replica lags the primary by one commit", Importance: 0.7,
			ValidFrom: &restated, VerifiedAt: stampPtr("2026-11-11 00:00:00"), SourceRef: "docs/new.md",
		})
		if row.ValidFrom == nil || *row.ValidFrom != restated || row.VerifiedAt == nil || *row.VerifiedAt != "2026-11-11 00:00:00" || row.SourceRef != "docs/new.md" {
			t.Errorf("successor did not take the emission's claim: %v %v %q", row.ValidFrom, row.VerifiedAt, row.SourceRef)
		}
		if row.Agent != "claude-code" {
			t.Errorf("successor agent = %q, want the source's since the emission stated none", row.Agent)
		}
	})
}

// importOnce exports every memory of one store and imports the named one into a
// fresh store, returning the destination.
func importOnce(t *testing.T, from *Store, projectID, memoryID string) *Store {
	t.Helper()
	ctx := context.Background()
	rows, err := from.PortableMemories(ctx, []string{projectID})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	for _, r := range rows {
		if r.ID != memoryID {
			continue
		}
		if len(r.Evidence) == 0 {
			t.Fatalf("the artifact carries no evidence records")
		}
		dest := testStore(t)
		if _, _, _, err := dest.ImportMemory(ctx, r, ImportOptions{Apply: true}); err != nil {
			t.Fatalf("ImportMemory: %v", err)
		}
		return dest
	}
	t.Fatalf("PortableMemories returned no row for %s", memoryID)
	return nil
}

// verifiedCount counts stamped records straight from the table, since the row's
// own verified_at column is copied through and reads 1 even in a broken build.
func verifiedCount(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_provenance WHERE memory_id = ? AND verified_at IS NOT NULL AND verified_at <> ''`,
		memoryID,
	).Scan(&n); err != nil {
		t.Fatalf("count stamped evidence rows: %v", err)
	}
	return n
}

func optStamp(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
