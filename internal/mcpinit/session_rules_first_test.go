package mcpinit

import (
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

func TestSessionRowsFromOrdersRulesBeforeFacts(t *testing.T) {
	items := []assemble.Item{
		{ID: "fact-high", Category: "fact"},
		{ID: "pref", Category: "preference"},
		{ID: "pinned-fact", Category: "fact", Pinned: true},
		{ID: "gotcha", Category: "gotcha"},
		{ID: "fact-low", Category: "architecture"},
		{ID: "g-fact", Category: "fact", ProjectID: memory.GlobalProjectID},
		{ID: "g-conv", Category: "convention", ProjectID: memory.GlobalProjectID},
	}
	mems, globals := sessionRowsFrom(items)
	ids := func(rows []sessionMemory) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	want := []string{"pinned-fact", "pref", "gotcha", "fact-high", "fact-low"}
	got := ids(mems)
	if len(got) != len(want) {
		t.Fatalf("project order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("project order = %v, want %v", got, want)
		}
	}
	if g := ids(globals); len(g) != 2 || g[0] != "g-conv" || g[1] != "g-fact" {
		t.Fatalf("global order = %v", g)
	}
}
