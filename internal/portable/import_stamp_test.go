package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestImportRejectsAStampTheSaveToolRefusesByName: an artifact record whose
// validity or confidence the save tool would refuse is rejected on its own,
// named by id, in the dry run and in the apply it previews, and the rest of the
// artifact still lands. Before, all three of these imported silently.
func TestImportRejectsAStampTheSaveToolRefusesByName(t *testing.T) {
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-junk", "project_id": "p1", "category": "fact", "content": "a body with an unreadable end",
			"source": "mcp", "valid_until": "next tuesday",
		}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-win", "project_id": "p1", "category": "fact", "content": "a body with an inverted window",
			"source": "mcp", "valid_from": "2026-12-01 00:00:00", "valid_until": "2026-01-01 00:00:00",
		}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-conf", "project_id": "p1", "category": "fact", "content": "a body rated off the scale",
			"source": "mcp", "confidence": 7,
		}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{
			"id": "m-ok", "project_id": "p1", "category": "fact", "content": "a body stated in RFC 3339",
			"source": "mcp", "valid_until": "2027-01-01T00:00:00Z",
		}}),
	}
	artifact := strings.Join(lines, "\n") + "\n"
	want := map[string]string{
		"m-junk": "valid_until must be RFC 3339",
		"m-win":  "is not after valid_from",
		"m-conf": "confidence must be between 0 and 1",
	}

	for _, apply := range []bool{false, true} {
		_, store := newTestStoreWithDB(t)
		got := map[string]RecordResult{}
		report, err := Import(context.Background(), store, strings.NewReader(artifact), ImportOptions{Apply: apply}, func(r RecordResult) {
			if r.Type == TypeMemory {
				got[r.ID] = r
			}
		})
		if err != nil {
			t.Fatalf("Import(apply=%v): %v", apply, err)
		}
		if report.Rejected != len(want) {
			t.Errorf("apply=%v: rejected %d, want %d: %v", apply, report.Rejected, len(want), report.Errors)
		}
		for id, reason := range want {
			r, ok := got[id]
			if !ok {
				t.Errorf("apply=%v: no report line for %s", apply, id)
				continue
			}
			if r.Action != ActionReject || r.Error == nil || !strings.Contains(r.Error.Error(), reason) {
				t.Errorf("apply=%v: %s was %s (%v), want rejected naming %q", apply, id, r.Action, r.Error, reason)
			}
		}
		if r := got["m-ok"]; r.Action != ActionCreate {
			t.Errorf("apply=%v: m-ok was %s (%v), want created", apply, r.Action, r.Error)
		}
	}
}

// TestExportImportKeepsValidityStampsByteIdentical: a store written through the
// save path holds stored-layout stamps, and they cross an export and an import
// and the next export byte for byte. The comparison is on the stamp and rating
// fields of each memory record rather than on the whole artifact, because the
// import appends an arrival evidence record by design.
func TestExportImportKeepsValidityStampsByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	if err := src.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", "git@github.com:wcatz/one.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	from, until, verified, conf := "2026-01-01 00:00:00", "2026-12-31 23:59:59", "2026-09-20 08:15:30", 0.75
	midnight := "2027-03-01 00:00:00"
	if _, err := src.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "a windowed claim", Source: "mcp", Importance: 0.5,
		ValidFrom: &from, ValidUntil: &until, VerifiedAt: &verified, Confidence: &conf,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := src.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "a claim ending at a chosen midnight", Source: "mcp", Importance: 0.5,
		ValidUntil: &midnight,
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

	a, b := stampFields(t, first), stampFields(t, second)
	if len(a) != 2 {
		t.Fatalf("first export carried %d memories, want 2", len(a))
	}
	for id, fields := range a {
		if b[id] != fields {
			t.Errorf("memory %s stamps changed across the round trip:\n first  %s\n second %s", id, fields, b[id])
		}
	}
}

// stampFields maps each memory record's id to the raw bytes of its validity
// triple and confidence, as the artifact spells them.
func stampFields(t *testing.T, artifact []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range bytes.Split(artifact, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
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
		var id string
		_ = json.Unmarshal(rec.Memory["id"], &id)
		var sb strings.Builder
		for _, k := range []string{"valid_from", "valid_until", "verified_at", "confidence"} {
			sb.WriteString(k + "=" + string(rec.Memory[k]) + " ")
		}
		out[id] = sb.String()
	}
	return out
}
