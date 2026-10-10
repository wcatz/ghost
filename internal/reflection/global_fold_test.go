package reflection

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestPlanGlobalFold(t *testing.T) {
	mems := []memory.Memory{
		{ID: "a", Category: "preference", Content: "always run go vet before committing any change", Importance: 0.9, CreatedAt: "2026-01-01 00:00:00"},
		{ID: "b", Category: "preference", Content: "always run go vet before committing a change", Importance: 0.5, CreatedAt: "2026-02-01 00:00:00"},
		{ID: "c", Category: "preference", Content: "always run go vet before committing any change to the code", Importance: 0.6, CreatedAt: "2026-03-01 00:00:00"},
		{ID: "d", Category: "fact", Content: "cardano preprod uses network magic 1", CreatedAt: "2026-02-01 00:00:00"},
		// A numeric conflict is a different fact, not a duplicate.
		{ID: "e", Category: "fact", Content: "cardano preprod uses network magic 2", CreatedAt: "2026-02-02 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1: %+v", len(clusters), clusters)
	}
	c := clusters[0]
	if c.Survivor.ID != "c" || len(c.Folded) != 2 {
		t.Fatalf("survivor %q folded %d, want the containing row c with two folded", c.Survivor.ID, len(c.Folded))
	}
	for _, f := range c.Folded {
		if f.ID == "c" || f.ID == "d" || f.ID == "e" {
			t.Errorf("folded row %q does not belong", f.ID)
		}
	}
}

func TestPlanGlobalFoldNothingToFoldReturnsNil(t *testing.T) {
	if got := PlanGlobalFold([]memory.Memory{
		{ID: "a", Content: "use tabs not spaces"},
		{ID: "b", Content: "cardano preprod uses network magic 1"},
	}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestPlanGlobalFoldNamesTheOldestRowForIdenticalText(t *testing.T) {
	// The older row comes second in the input, so input order cannot be what picks it.
	mems := []memory.Memory{
		{ID: "2", Content: "run the linter before pushing", CreatedAt: "2026-02-01 00:00:00"},
		{ID: "1", Content: "run the linter before pushing", CreatedAt: "2026-01-01 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 || clusters[0].Survivor.ID != "1" {
		t.Fatalf("byte-identical rows keep the oldest, got %+v", clusters)
	}
}

// The cluster rule is Jaccard >= 0.5 as well as full containment, and a pair
// below it stays two rows. None of these rows contains another, so only the
// Jaccard bar decides them.
func TestPlanGlobalFoldJaccardBar(t *testing.T) {
	mems := []memory.Memory{
		{ID: "a", Content: "restart the relay after kernel updates", CreatedAt: "2026-01-01 00:00:00"},
		{ID: "b", Content: "restart the relay after kernel patches", CreatedAt: "2026-01-02 00:00:00"},
		{ID: "c", Content: "restart the relay with care", CreatedAt: "2026-01-03 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 || clusters[0].Survivor.ID != "b" || len(clusters[0].Folded) != 1 || clusters[0].Folded[0].ID != "a" {
		t.Fatalf("clusters = %+v, want a (0.67 similar) folded into b and c (0.33) left alone", clusters)
	}
}

// The older, longer wording carries a specific the newer subset lacks.
func TestPlanGlobalFoldKeepsTheRowThatContainsTheOthers(t *testing.T) {
	mems := []memory.Memory{
		{ID: "long", Content: "deploy requires helmfile diff then apply, and the sops age key must be exported first, and only from the dev machine", CreatedAt: "2026-01-01 00:00:00"},
		{ID: "short", Content: "deploy requires helmfile diff then apply", CreatedAt: "2026-02-01 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 || clusters[0].Survivor.ID != "long" {
		t.Fatalf("survivor = %+v, want the older row that contains the newer", clusters)
	}
}

// The containing row wins even when it is not the longest in characters.
func TestPlanGlobalFoldContainmentBeatsLength(t *testing.T) {
	mems := []memory.Memory{
		{ID: "padded", Content: "run run run go go vet vet before before committing committing", CreatedAt: "2026-03-01 00:00:00"},
		{ID: "full", Content: "run go vet before committing every change", CreatedAt: "2026-01-01 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 || clusters[0].Survivor.ID != "full" {
		t.Fatalf("survivor = %+v, want the row holding every token", clusters)
	}
}

func TestPlanGlobalFoldNeverClustersOppositePairs(t *testing.T) {
	for _, pair := range [][2]string{
		{"always run the full test suite before committing", "never run the full test suite before committing"},
		{"deploys must go through helmfile diff first", "deploys must not go through helmfile diff first"},
		{"do run the linter before pushing changes", "don't run the linter before pushing changes"},
		{"use the shared cache when building the image", "avoid the shared cache when building the image"},
		{"enable the vector index when building the store", "disable the vector index when building the store"},
		{"allow direct pushes to the release branch for hotfixes", "deny direct pushes to the release branch for hotfixes"},
		{"allow direct pushes to the release branch for hotfixes", "forbid direct pushes to the release branch for hotfixes"},
		{"always rebase the feature branch before opening the pull request", "avoid rebase the feature branch before opening the pull request"},
		{"prefer nerdctl over docker for running containers on the host", "prefer docker over nerdctl for running containers on the host"},
	} {
		if got := PlanGlobalFold([]memory.Memory{
			{ID: "a", Content: pair[0], CreatedAt: "2026-01-01 00:00:00"},
			{ID: "b", Content: pair[1], CreatedAt: "2026-01-02 00:00:00"},
		}); got != nil {
			t.Errorf("%q and %q were clustered: %+v", pair[0], pair[1], got)
		}
	}
}

// The same rows with matching stance still fold, so the guard is not a blanket.
func TestPlanGlobalFoldStillFoldsMatchingStance(t *testing.T) {
	for _, pair := range [][2]string{
		{"use the shared cache when building the image", "use the shared cache when building the image today"},
		{"prefer nerdctl over docker for running containers on the host", "prefer nerdctl over docker for running containers on this host"},
	} {
		if got := PlanGlobalFold([]memory.Memory{
			{ID: "a", Content: pair[0], CreatedAt: "2026-01-01 00:00:00"},
			{ID: "b", Content: pair[1], CreatedAt: "2026-01-02 00:00:00"},
		}); len(got) != 1 {
			t.Errorf("%q and %q did not cluster", pair[0], pair[1])
		}
	}
}
