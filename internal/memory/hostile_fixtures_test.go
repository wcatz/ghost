package memory

import (
	"context"
	"math"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
)

// importProjectForFixture creates the project an imported record names, so a
// refusal is about the field under test rather than a missing project.
func importProjectForFixture(t *testing.T, store *Store) {
	t.Helper()
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
}

// TestSharedMalformedStampsAtTheImportBoundary feeds the shared malformed-stamp
// set through the import path. The save tool refuses an unreadable stamp, and
// the import path is the sibling boundary that applies the same rule.
func TestSharedMalformedStampsAtTheImportBoundary(t *testing.T) {
	for _, p := range adversarial.MalformedStamps() {
		t.Run(p.Name, func(t *testing.T) {
			store := portableTestStore(t)
			importProjectForFixture(t, store)
			stamp := p.Text
			_, _, _, err := store.ImportMemory(context.Background(), PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "a fact",
				Source: "mcp", ValidFrom: &stamp,
			}, ImportOptions{Apply: true})
			if err == nil {
				t.Errorf("ImportMemory stored an unreadable valid_from (%s)", p.Name)
			}
		})
	}
}

// TestSharedMalformedStampsAtTheEvidenceBoundary feeds the same set through an
// EVIDENCE record's own stamp, which is a moment a reader interprets exactly as
// a memory's own is: the support summary counts a stamped record as a
// verification, so one nobody can read is a claim the count would rest on.
func TestSharedMalformedStampsAtTheEvidenceBoundary(t *testing.T) {
	for _, p := range adversarial.MalformedStamps() {
		t.Run(p.Name, func(t *testing.T) {
			store := portableTestStore(t)
			importProjectForFixture(t, store)
			stamp := p.Text
			_, _, _, err := store.ImportMemory(context.Background(), PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "a fact",
				Source:   "mcp",
				Evidence: []PortableEvidence{{Kind: "observed", ObservedAt: &stamp}},
			}, ImportOptions{Apply: true})
			if err == nil {
				t.Errorf("ImportMemory stored an unreadable evidence observed_at (%s)", p.Name)
			}
		})
	}
}

// TestSharedOutOfRangeConfidenceAtTheImportBoundary feeds the shared confidence
// class through the import path, which is the sibling of the save tool's range
// check.
func TestSharedOutOfRangeConfidenceAtTheImportBoundary(t *testing.T) {
	for name, confidence := range map[string]float64{
		"below-zero":   -0.1,
		"above-one":    1.1,
		"nan":          math.NaN(),
		"positive-inf": math.Inf(1),
		"negative-inf": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			store := portableTestStore(t)
			importProjectForFixture(t, store)
			c := confidence
			_, _, _, err := store.ImportMemory(context.Background(), PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "a fact",
				Source: "mcp", Confidence: &c,
			}, ImportOptions{Apply: true})
			if err == nil {
				t.Errorf("ImportMemory stored an out-of-range confidence (%s)", name)
			}
		})
	}
}

// TestSharedTokenShapedTaskBranchAtTheImportBoundary feeds the shared
// token-shaped set through the import path's task record, whose branch field
// the token guard covers.
func TestSharedTokenShapedTaskBranchAtTheImportBoundary(t *testing.T) {
	for _, p := range adversarial.TokenShaped() {
		t.Run(p.Name, func(t *testing.T) {
			store := portableTestStore(t)
			importProjectForFixture(t, store)
			_, _, err := store.ImportTask(context.Background(), Task{
				ID: "t1", ProjectID: "p1", Title: "a task", Status: "pending",
				Branch: p.Text,
			}, true)
			if err == nil {
				t.Errorf("ImportTask stored a token-shaped branch (%s)", p.Name)
			}
		})
	}
}
