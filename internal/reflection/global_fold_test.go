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
// below it stays two rows. None of these rows contains another, and each side of
// the difference is three tokens (a paraphrase, not a swap), so only the Jaccard
// bar decides them.
func TestPlanGlobalFoldJaccardBar(t *testing.T) {
	mems := []memory.Memory{
		{ID: "a", Content: "restart relay after kernel updates friday morning evening night", CreatedAt: "2026-01-01 00:00:00"},
		{ID: "b", Content: "restart relay after kernel updates friday patches security hotfix", CreatedAt: "2026-01-02 00:00:00"},
		{ID: "c", Content: "restart relay with care", CreatedAt: "2026-01-03 00:00:00"},
	}
	clusters := PlanGlobalFold(mems)
	if len(clusters) != 1 || len(clusters[0].Folded) != 1 {
		t.Fatalf("clusters = %+v, want a and b (0.5 similar) together and c (0.3) left alone", clusters)
	}
	for _, id := range []string{clusters[0].Survivor.ID, clusters[0].Folded[0].ID} {
		if id == "c" {
			t.Fatalf("c was clustered: %+v", clusters)
		}
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

// A neutral head is near both of two opposite rows; the guard has to hold between
// the rows themselves, not only against the head.
func TestPlanGlobalFoldGuardsEveryClusterMemberNotJustTheHead(t *testing.T) {
	mems := []memory.Memory{
		{ID: "head", Content: "the shared cache when building the image", CreatedAt: "2026-01-01 00:00:00"},
		{ID: "use", Content: "use the shared cache when building the image", CreatedAt: "2026-01-02 00:00:00"},
		{ID: "avoid", Content: "avoid the shared cache when building the image", CreatedAt: "2026-01-03 00:00:00"},
	}
	for _, c := range PlanGlobalFold(mems) {
		ids := map[string]bool{c.Survivor.ID: true}
		for _, f := range c.Folded {
			ids[f.ID] = true
		}
		if ids["use"] && ids["avoid"] {
			t.Fatalf("opposite rows were clustered through a neutral head: %+v", c)
		}
	}
}

// One word swapped for its opposite scores as a near-duplicate and no list of
// opposites is complete, so a swap (each side holds a token the other lacks, the
// smaller side at most two) never clusters, while one row adding detail to the
// other still folds.
func TestPlanGlobalFoldNeverClustersASwap(t *testing.T) {
	for _, pair := range [][2]string{
		{"indent go files with tabs in this style guide", "indent go files with spaces in this style guide"},
		{"turn on the vector index when building the store", "turn off the vector index when building the store"},
		{"include the generated files when building the release", "exclude the generated files when building the release"},
		{"run the slow integration tests before every release", "skip the slow integration tests before every release"},
		{"the dry run flag defaults to true for the destructive commands", "the dry run flag defaults to false for the destructive commands"},
		{"release commits must be signed before they are merged", "release commits must be unsigned before they are merged"},
		{"squash the feature branch commits before merging to main", "rebase the feature branch commits before merging to main"},
		{"add the sops age key to the runner before deploying", "remove the sops age key from the runner before deploying"},
	} {
		if got := PlanGlobalFold([]memory.Memory{
			{ID: "a", Content: pair[0], CreatedAt: "2026-01-01 00:00:00"},
			{ID: "b", Content: pair[1], CreatedAt: "2026-01-02 00:00:00"},
		}); got != nil {
			t.Errorf("%q and %q were clustered: %+v", pair[0], pair[1], got)
		}
	}
	got := PlanGlobalFold([]memory.Memory{
		{ID: "short", Content: "deploy requires helmfile diff then apply", CreatedAt: "2026-02-01 00:00:00"},
		{ID: "long", Content: "deploy requires helmfile diff then apply, and the sops age key must be exported first, and only from the dev machine", CreatedAt: "2026-01-01 00:00:00"},
	})
	if len(got) != 1 || got[0].Survivor.ID != "long" {
		t.Errorf("a pure addition must still fold into the longer row: %+v", got)
	}
}
