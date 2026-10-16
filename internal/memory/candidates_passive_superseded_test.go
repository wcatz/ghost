package memory

import (
	"context"
	"testing"
)

// The tests below cover Candidate.SupersededBy on an UNPINNED passive row. A
// convention or a preference is never resolved (the standing-category guardrail
// on resolve), so after the lifecycle links a replaced one the supersedes edge is
// the only signal left: the lexical replacement guess is switched off for a pair
// the edge joins, and the row must still be named on its line. The slot rules are
// untouched: only a pinned row and its superseder are reserved.

// TestAnUnpinnedSupersededRowIsNamedByItsSuperseder is the defect: the edge
// sank the old row and named nothing, so the block rendered it unmarked.
func TestAnUnpinnedSupersededRowIsNamedByItsSuperseder(t *testing.T) {
	st := pinSlotStore(t, 3,
		pinSlotRow{id: "old_conv", project: "proj", category: "convention", importance: 0.8},
		pinSlotRow{id: "new_conv", project: "proj", category: "convention", importance: 0.8})
	if err := st.CreateLink(context.Background(), "new_conv", "old_conv", "supersedes", 1, "llm"); err != nil {
		t.Fatalf("link: %v", err)
	}
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			c, ok := pinCandidateByID(set, "old_conv")
			if !ok {
				t.Fatalf("the superseded row is not in the set: %v", passiveIDs(set))
			}
			if len(c.SupersededBy) != 1 || c.SupersededBy[0] != "new_conv" {
				t.Errorf("SupersededBy = %v, want [new_conv]: an unpinned superseded row is named by its superseder", c.SupersededBy)
			}
			if n, _ := pinCandidateByID(set, "new_conv"); len(n.SupersededBy) != 0 {
				t.Errorf("the superseder is named as superseded: %v", n.SupersededBy)
			}
		})
	}
}

// TestAnUnpinnedSupersededRowReservesNoSlot: naming an unpinned row does not
// widen the pin's reservation. Both rows rank last among 30 rows at 0.9, inside
// the over-fetch window (so the retriever does name the superseder) but behind
// the cap, and the superseder stays cut: only a PINNED row's superseder is
// reserved. The admitted head is the head with no edge at all.
func TestAnUnpinnedSupersededRowReservesNoSlot(t *testing.T) {
	st := pinSlotStore(t, 30,
		pinSlotRow{id: "old_conv", project: "proj", category: "fact", importance: 0.1},
		pinSlotRow{id: "new_conv", project: "proj", category: "fact", importance: 0.05})
	before := map[string][]string{}
	pols := map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()}
	for name, pol := range pols {
		set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
		if err != nil {
			t.Fatalf("Candidates: %v", err)
		}
		before[name] = firstN(passiveIDs(set), pol.ItemCap)
	}
	if err := st.CreateLink(context.Background(), "new_conv", "old_conv", "supersedes", 1, "llm"); err != nil {
		t.Fatalf("link: %v", err)
	}
	for name, pol := range pols {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if c, _ := pinCandidateByID(set, "old_conv"); len(c.SupersededBy) != 1 {
				t.Fatalf("the fixture must put both rows in the window, so the superseder is named: %v", c.SupersededBy)
			}
			head := firstN(passiveIDs(set), pol.ItemCap)
			if containsStr(head, "new_conv") {
				t.Errorf("the superseder of an unpinned row took a reserved slot: %v", head)
			}
			if len(head) != len(before[name]) {
				t.Fatalf("head length moved: %v -> %v", before[name], head)
			}
			for i := range head {
				if head[i] != before[name][i] {
					t.Errorf("the admitted head moved at %d: %v -> %v", i, before[name], head)
					break
				}
			}
		})
	}
}

// TestAWithdrawnOrScopeConflictingSupersedeNamesNothing: the naming reads the
// demotion's own edge rules, so an invalidated edge (supersede withdraws by
// invalidating) and an edge between two conflicting scopes name nothing, exactly
// as neither sinks the row.
func TestAWithdrawnOrScopeConflictingSupersedeNamesNothing(t *testing.T) {
	st := pinSlotStore(t, 3,
		pinSlotRow{id: "old_wd", project: "proj", category: "convention", importance: 0.8},
		pinSlotRow{id: "new_wd", project: "proj", category: "convention", importance: 0.8},
		pinSlotRow{id: "old_sc", project: "proj", category: "convention", importance: 0.8},
		pinSlotRow{id: "new_sc", project: "proj", category: "convention", importance: 0.8})
	ctx := context.Background()
	for _, l := range [][2]string{{"new_wd", "old_wd"}, {"new_sc", "old_sc"}} {
		if err := st.CreateLink(ctx, l[0], l[1], "supersedes", 1, "llm"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	if n, err := st.InvalidateLink(ctx, "new_wd", "old_wd", "supersedes"); err != nil || n != 1 {
		t.Fatalf("InvalidateLink = %d, %v", n, err)
	}
	for id, env := range map[string]string{"new_sc": "development", "old_sc": "production"} {
		if _, err := st.db.Exec(`UPDATE memories SET scope = ? WHERE id = ?`, `{"environment":"`+env+`"}`, id); err != nil {
			t.Fatalf("scope %s: %v", id, err)
		}
	}
	set, err := st.Candidates(ctx, passiveRequest("proj", pinUnionPolicy()))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, id := range []string{"old_wd", "old_sc"} {
		c, ok := pinCandidateByID(set, id)
		if !ok {
			t.Fatalf("%s is not in the set: %v", id, passiveIDs(set))
		}
		if len(c.SupersededBy) != 0 {
			t.Errorf("%s SupersededBy = %v, want none", id, c.SupersededBy)
		}
	}
}

// TestAKeepForeverSupersededRowIsNamedButNotSunk: a `retention=persistent` row is
// never sunk by a supersedes edge (supersedePenaltyRows exempts it), but the edge
// still switches the lexical guess off for the pair, so it must still be NAMED or
// the lifecycle leaves it unmarked. The name is for the marker only: the row
// keeps the place it holds with no edge at all, on a pinned row and an unpinned
// one, and a pinned keep-forever row is not moved behind its superseder nor is
// the superseder reserved.
func TestAKeepForeverSupersededRowIsNamedButNotSunk(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "unpinned"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			st := pinSlotStore(t, 30,
				pinSlotRow{id: "keep_old", project: "proj", category: "convention", importance: 0.95, pinned: pinned},
				pinSlotRow{id: "keep_new", project: "proj", category: "convention", importance: 0.05})
			if _, err := st.db.Exec(`UPDATE memories SET retention = 'persistent' WHERE id = 'keep_old'`); err != nil {
				t.Fatalf("retention: %v", err)
			}
			ctx := context.Background()
			pols := map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()}
			before := map[string][]string{}
			for pn, pol := range pols {
				set, err := st.Candidates(ctx, passiveRequest("proj", pol))
				if err != nil {
					t.Fatalf("Candidates: %v", err)
				}
				before[pn] = passiveIDs(set)
			}
			if err := st.CreateLink(ctx, "keep_new", "keep_old", "supersedes", 1, "llm"); err != nil {
				t.Fatalf("link: %v", err)
			}
			for pn, pol := range pols {
				set, err := st.Candidates(ctx, passiveRequest("proj", pol))
				if err != nil {
					t.Fatalf("Candidates: %v", err)
				}
				c, ok := pinCandidateByID(set, "keep_old")
				if !ok {
					t.Fatalf("%s: the keep-forever row is not in the set", pn)
				}
				if len(c.SupersededBy) != 1 || c.SupersededBy[0] != "keep_new" {
					t.Errorf("%s: SupersededBy = %v, want [keep_new]: a keep-forever row is named for its marker", pn, c.SupersededBy)
				}
				after := passiveIDs(set)
				if len(after) != len(before[pn]) {
					t.Fatalf("%s: set size moved: %d -> %d", pn, len(before[pn]), len(after))
				}
				for i := range after {
					if after[i] != before[pn][i] {
						t.Errorf("%s: the order moved at %d (a keep-forever row is never sunk, and its name reserves nothing): %v -> %v", pn, i, before[pn], after)
						break
					}
				}
			}
		})
	}
}
