package bench

import (
	"strings"
	"testing"
)

// TestSeedRefusesAnUnorderableSupersedesGraph: a supersedes cycle is a state the
// demote cannot order — both rows are penalised by the other, so neither sinks and
// the pair's relative position is left to the base score. The store accepts the
// edge, so nothing downstream would notice; the fixture loader has to.
func TestSeedRefusesAnUnorderableSupersedesGraph(t *testing.T) {
	star := []MemorySpec{
		{Key: "v1", Category: "dependency", Content: "one"},
		{Key: "v2", Category: "dependency", Content: "two", Supersedes: []string{"v1"}},
		{Key: "v3", Category: "dependency", Content: "three", Supersedes: []string{"v2", "v1"}},
	}
	if cycle := supersedesCycle(star); cycle != "" {
		t.Errorf("a three-deep star link is a DAG, not a cycle, but the walk found %q", cycle)
	}

	// Close the star into a cycle by pointing v1 at v3: v1 -> v3 -> v1.
	cyclic := append([]MemorySpec{}, star...)
	cyclic[0].Supersedes = []string{"v3"}
	cycle := supersedesCycle(cyclic)
	if cycle == "" {
		t.Fatal("a cycle v1 -> v3 -> v1 was accepted as acyclic")
	}
	for _, want := range []string{"v1", "v3"} {
		if !strings.Contains(cycle, want) {
			t.Errorf("cycle %q does not name %q", cycle, want)
		}
	}

	// And it has to reach Seed, not just the helper: a dataset that loads and
	// scores with a cycle in it is the whole failure.
	ds := Dataset{
		Project: "p",
		Memories: []MemorySpec{
			{Key: "a", Category: "fact", Content: "a", Supersedes: []string{"a2"}},
			{Key: "b", Category: "fact", Content: "b", Supersedes: []string{"a"}},
			{Key: "a2", Category: "fact", Content: "a again", Supersedes: []string{"b", "a"}},
		},
		Queries: []QuerySpec{{Name: "q", Text: "a", Rel: map[string]int{"a": 1}}},
	}
	store, db := newBenchStoreWithDB(t)
	_, err := Seed(t.Context(), store, db, ds, Vectors{"a": {1, 0}, "b": {0, 1}, "a2": {1, 1}, "q": {1, 0}})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("Seed accepted a supersedes cycle: %v", err)
	}

	// A self-edge is refused in the same place, and named as itself rather than
	// reported as a cycle or a generic store error.
	ds.Memories[0].Supersedes = nil
	ds.Memories[2].Supersedes = []string{"a2"}
	store, db = newBenchStoreWithDB(t)
	_, err = Seed(t.Context(), store, db, ds, Vectors{"a": {1, 0}, "b": {0, 1}, "a2": {1, 1}, "q": {1, 0}})
	if err == nil || !strings.Contains(err.Error(), "supersedes itself") {
		t.Errorf("Seed accepted a self-supersede: %v", err)
	}
}

// TestLoadTrapScenariosRefusesARowLabelAsCategory: the report's pooled rows are
// labelled with the same words a scenario's category could be given, and a
// collision would count a category into the pool that shares its name and render
// it under the pool's heading — a wrong number, not a visible failure.
func TestLoadTrapScenariosRefusesARowLabelAsCategory(t *testing.T) {
	for _, label := range []string{trapAllLabel, trapDecayingLabel, trapNeverDecayLabel} {
		line := `{"name":"x","category":"` + label + `","correct":{"content":"a","age_days":10},` +
			`"traps":[{"content":"b","age_days":1}],"probes":[{"text":"a"}]}`
		if _, err := LoadTrapScenarios(strings.NewReader(line)); err == nil {
			t.Errorf("LoadTrapScenarios accepted the report row label %q as a category", label)
		}
	}
	// A category that merely CONTAINS one of the words is fine: the check is
	// equality, because a category is a free-form word from the fixture.
	line := `{"name":"x","category":"fact (all probes)","correct":{"content":"a","age_days":10},` +
		`"traps":[{"content":"b","age_days":1}],"probes":[{"text":"a"}]}`
	if _, err := LoadTrapScenarios(strings.NewReader(line)); err != nil {
		t.Errorf("LoadTrapScenarios refused a category that is not exactly a row label: %v", err)
	}
}
