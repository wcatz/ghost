package memory

import (
	"context"
	"path/filepath"
	"testing"
)

func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }

// TestUpsertWithProvenanceRecordsAgentAndConfidence pins that provenance
// passed at write time survives a round trip through the read path.
//
// The columns alone are not the feature: a column nobody writes and nobody
// reads is inert schema. This asserts both halves — the INSERT carries the
// values and scanMemories returns them — so a SELECT list that grows a
// column without the scanner (or vice versa) fails loudly instead of
// silently dropping provenance on every read.
func TestUpsertWithProvenanceRecordsAgentAndConfidence(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "provenance.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/prov", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"the API gateway listens on port 8443", "mcp", 0.7, []string{"net"},
		Provenance{Agent: "opencode", SessionID: "ses_123", SourceRef: "helmfile.yaml:L20", Confidence: f64Ptr(0.9)})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}

	got, err := s.GetAll(ctx, testProject, 50)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var found *Memory
	for i := range got {
		if got[i].ID == id {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("saved memory %s not returned by GetAll", id)
	}
	if found.Agent != "opencode" {
		t.Errorf("Agent = %q, want opencode", found.Agent)
	}
	if found.SessionID != "ses_123" {
		t.Errorf("SessionID = %q, want ses_123", found.SessionID)
	}
	if found.SourceRef != "helmfile.yaml:L20" {
		t.Errorf("SourceRef = %q, want helmfile.yaml:L20", found.SourceRef)
	}
	if found.Confidence == nil || *found.Confidence != 0.9 {
		t.Errorf("Confidence = %v, want 0.9", derefF(found.Confidence))
	}
}

// TestUpsertWithoutProvenanceLeavesColumnsNull is the compatibility contract:
// the60 existing Upsert callers pass no provenance, and none of them may
// start recording a fabricated value. An agent column that guessed
// "claude" because the process happened to be a child of it would be a
// provenance claim nobody made.
func TestUpsertWithoutProvenanceLeavesColumnsNull(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "provenance-null.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/prov2", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a plain memory with no provenance", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.GetAll(ctx, testProject, 50)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range got {
		if m.ID != id {
			continue
		}
		if m.Agent != "" {
			t.Errorf("Agent = %q, want empty — Upsert with no provenance must not invent one", m.Agent)
		}
		if m.Confidence != nil {
			t.Errorf("Confidence = %v, want nil", *m.Confidence)
		}
		return
	}
	t.Fatalf("saved memory %s not returned", id)
}

func deref(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
