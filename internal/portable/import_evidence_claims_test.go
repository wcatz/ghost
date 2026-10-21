package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The issue's own reproduction: an artifact whose memory carries one evidence
// record with two stamps no reader can parse and a rating of 99 is rejected by
// name, the rest of the artifact still lands, and nothing about that record
// reaches the store.
//
// Before, the evidence record was stored as it stood, and the support summary
// counted it as a verification — so the context label reported the memory as
// supported by verified observations on the strength of values the save path
// would never have written.
func TestImportRejectsAnEvidenceRecordWhoseStampsAndRatingAreUnreadable(t *testing.T) {
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-evidence", "project_id": "p1", "category": "fact",
			"content": "a body carrying an unreadable claim", "source": "mcp",
			"evidence": []any{map[string]any{
				"kind": "verified", "observed_at": "not a date", "verified_at": "not a date", "confidence": 99,
			}},
		}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-readable", "project_id": "p1", "category": "fact",
			"content": "a body carrying a readable claim", "source": "mcp",
			"evidence": []any{map[string]any{
				"kind": "verified", "observed_at": "2026-09-20 00:00:00",
				"verified_at": "2026-09-20 00:00:00", "confidence": 0.9,
			}},
		}}),
	}
	artifact := strings.Join(lines, "\n") + "\n"

	for _, apply := range []bool{false, true} {
		_, store := newTestStoreWithDB(t)
		got := map[string]RecordResult{}
		report, err := Import(context.Background(), store, strings.NewReader(artifact),
			ImportOptions{Apply: apply, TrustProvenance: true}, func(r RecordResult) {
				if r.Type == TypeMemory {
					got[r.ID] = r
				}
			})
		if err != nil {
			t.Fatalf("Import(apply=%v): %v", apply, err)
		}
		if report.Rejected != 1 {
			t.Errorf("apply=%v: rejected %d, want 1: %v", apply, report.Rejected, report.Errors)
		}
		r, ok := got["m-evidence"]
		if !ok {
			t.Errorf("apply=%v: no report line for m-evidence", apply)
			continue
		}
		if r.Action != ActionReject || r.Error == nil {
			t.Errorf("apply=%v: m-evidence was %s (%v), want rejected", apply, r.Action, r.Error)
			continue
		}
		if !strings.Contains(r.Error.Error(), "evidence[0]") {
			t.Errorf("apply=%v: the refusal does not name the evidence record: %v", apply, r.Error)
		}
		if r := got["m-readable"]; r.Action != ActionCreate {
			t.Errorf("apply=%v: m-readable was %s (%v), want created", apply, r.Action, r.Error)
		}

		// And the refused record left nothing behind: no row, and no evidence
		// row claiming a verification nobody performed.
		rows, err := store.PortableMemories(context.Background(), []string{"p1"})
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		for _, m := range rows {
			if m.ID == "m-evidence" {
				t.Errorf("apply=%v: the refused memory was written", apply)
			}
		}
		if !apply {
			// A dry run writes nothing, so the counts below are the store's own.
			continue
		}
		counts, err := store.MemoryEvidenceCounts(context.Background(), "m-readable")
		if err != nil {
			t.Fatalf("MemoryEvidenceCounts: %v", err)
		}
		if counts.Verified != 1 {
			t.Errorf("apply=%v: the readable memory reports %d verified record(s), want 1", apply, counts.Verified)
		}
		if got := counts.Label(); !strings.Contains(got, "1 verified") {
			t.Errorf("apply=%v: the support summary reads %q, want it to name the one verified record", apply, got)
		}
	}
}

// The other half of the round trip: an evidence record whose stamps and rating
// a reader can interpret crosses an export and an import unchanged, so the
// support summary a reader sees on the far side is the one the origin store
// held.
func TestExportImportKeepsEvidenceStampsAndRatings(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	if err := src.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", "git@github.com:wcatz/one.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	confidence := 0.75
	if _, err := src.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "a claim with evidence", Source: "mcp", Importance: 0.5,
		Confidence: &confidence,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first := exportBytes(t, src, "")
	dst := newTestStore(t)
	report, err := Import(ctx, dst, bytes.NewReader(first), ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("import rejected %d record(s): %v", report.Rejected, report.Errors)
	}
	second := exportBytes(t, dst, "")

	before := evidenceStamps(t, first)
	after := evidenceStamps(t, second)
	if len(before) == 0 {
		t.Fatalf("the artifact carries no evidence record, so the round trip is not exercising anything")
	}
	for id, fields := range before {
		if after[id] != fields {
			t.Errorf("evidence record %s changed across the round trip:\n first  %s\n second %s", id, fields, after[id])
		}
	}
}

// evidenceStamps maps each evidence record's id to the raw bytes of that record
// as the artifact spells it.
func evidenceStamps(t *testing.T, artifact []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range bytes.Split(artifact, []byte("\n")) {
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var rec struct {
			Type   string                     `json:"type"`
			Memory map[string]json.RawMessage `json:"memory"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("decode artifact line: %v", err)
		}
		if rec.Type != TypeMemory {
			continue
		}
		var records []json.RawMessage
		if err := json.Unmarshal(rec.Memory["evidence"], &records); err != nil {
			t.Fatalf("decode the evidence list: %v", err)
		}
		for _, r := range records {
			var id struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(r, &id); err != nil {
				t.Fatalf("decode an evidence record: %v", err)
			}
			out[id.ID] = string(r)
		}
	}
	return out
}
