package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// pinSlotFixture is a project holding `high` unpinned rows at importance 0.9
// plus the given extra rows, so a pin's slot is a property of the policy and
// not of a corpus that happened to fit the window.
type pinSlotRow struct {
	id         string
	project    string
	category   string
	importance float32
	pinned     bool
	validUntil string
}

func pinSlotStore(t *testing.T, high int, extra ...pinSlotRow) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "pin.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	for _, p := range []string{"proj", "_global"} {
		if err := st.EnsureProject(ctx, p, p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	rows := make([]pinSlotRow, 0, high+len(extra))
	for i := 0; i < high; i++ {
		rows = append(rows, pinSlotRow{id: fmt.Sprintf("hi_%03d", i), project: "proj", category: "architecture", importance: 0.9})
	}
	rows = append(rows, extra...)
	for _, r := range rows {
		// Old created_at, so the decay ranking is a function of the fixture.
		stamp := "2026-01-01 00:00:00"
		var until any
		if r.validUntil != "" {
			until = r.validUntil
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, valid_until, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, ?, ?)`,
			r.id, r.project, r.category, "content of "+r.id, r.importance, r.pinned, until, stamp, stamp,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return st
}

// The three passive slice shapes the pin must hold on: the session start's
// project bucket (decay, behavioural floor), the project-context union
// (decay, no floor, `_global` admitted) and the `_global` bucket's own order.
func pinSessionPolicy() SlicePolicy {
	return SlicePolicy{
		Bucket: "proj", Order: OrderDecay, TwoPass: true, BehaviorFloor: 3,
		BehaviorCategories: []string{"gotcha", "convention"},
		OverFetch:          45, ItemCap: 15, DemoteOnlyWhenOverCap: true, DemotionThreshold: 0.9,
	}
}

func pinUnionPolicy() SlicePolicy {
	return SlicePolicy{
		Bucket: "proj", Order: OrderDecay, IncludeGlobal: true,
		OverFetch: 40, ItemCap: 20, DemoteOnlyWhenOverCap: true,
	}
}

func firstN(ids []string, n int) []string {
	if len(ids) < n {
		n = len(ids)
	}
	return ids[:n]
}

// TestAPinnedRowAtLowImportanceGetsASlotOnEverySlicePolicy: a pinned row at
// importance 0.1 among 30 rows at 0.9. The cap-sized head of the set is what the
// assembler admits, so the pin has to be in it. The pinned row is `fact`, which
// no behavioural floor names, so the floor cannot be what shows it.
func TestAPinnedRowAtLowImportanceGetsASlotOnEverySlicePolicy(t *testing.T) {
	st := pinSlotStore(t, 30, pinSlotRow{id: "the_pin", project: "proj", category: "fact", importance: 0.1, pinned: true})
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			head := firstN(passiveIDs(set), pol.ItemCap)
			if !containsStr(head, "the_pin") {
				t.Fatalf("the pinned row is outside the %d admitted rows: %v", pol.ItemCap, head)
			}
		})
	}
}

// TestAPinnedRowBeyondTheOverFetchWindowStillGetsASlot: 60 rows at 0.9 against a
// 40-row window. The pinned row ranks last, so it is not in the window unless
// the fetch itself puts pinned rows first. This is the proof of the SQL half;
// the 30-row test above fits inside the window and cannot tell.
func TestAPinnedRowBeyondTheOverFetchWindowStillGetsASlot(t *testing.T) {
	st := pinSlotStore(t, 60, pinSlotRow{id: "the_pin", project: "proj", category: "fact", importance: 0.1, pinned: true})
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if head := firstN(passiveIDs(set), pol.ItemCap); !containsStr(head, "the_pin") {
				t.Fatalf("the pinned row is outside the %d admitted rows: %v", pol.ItemCap, head)
			}
		})
	}
}

// TestAPinnedRowHoldsItsSlotOnTheGlobalBucket: the `_global` order already puts
// pinned first. This is a guard that the new reservation leaves it so.
func TestAPinnedRowHoldsItsSlotOnTheGlobalBucket(t *testing.T) {
	var extra []pinSlotRow
	for i := 0; i < 30; i++ {
		extra = append(extra, pinSlotRow{id: fmt.Sprintf("g_hi_%02d", i), project: "_global", category: "preference", importance: 0.9})
	}
	extra = append(extra, pinSlotRow{id: "g_pin", project: "_global", category: "preference", importance: 0.1, pinned: true})
	st := pinSlotStore(t, 0, extra...)
	pol := SlicePolicy{Bucket: "_global", Order: OrderPinnedImportanceUpdated, OverFetch: 16, ItemCap: 8, DropDemotedLosers: true, DemotionThreshold: 0.85}
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if head := firstN(passiveIDs(set), pol.ItemCap); !containsStr(head, "g_pin") {
		t.Fatalf("the pinned global row is outside the admitted rows: %v", head)
	}
}

// TestAnExpiredPinnedRowStaysWithheld: the pin is a slot guarantee, not an
// exemption from the validity window.
func TestAnExpiredPinnedRowStaysWithheld(t *testing.T) {
	st := pinSlotStore(t, 30,
		pinSlotRow{id: "expired_pin", project: "proj", category: "fact", importance: 0.1, pinned: true, validUntil: "2026-01-15 00:00:00"})
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if containsStr(passiveIDs(set), "expired_pin") {
				t.Fatalf("an expired pinned row reached the candidate set: %v", passiveIDs(set))
			}
		})
	}
}

// TestMorePinnedRowsThanTheCapAreRankedAndTheOverflowIsReported: 25 pinned rows
// against a cap of 20, the lowest-ranked five being the lowest-importance ones.
// The head is the 20 best pinned rows, and the five behind the window (the
// window is 22) are reported rather than silently absent.
func TestMorePinnedRowsThanTheCapAreRankedAndTheOverflowIsReported(t *testing.T) {
	var extra []pinSlotRow
	for i := 0; i < 25; i++ {
		extra = append(extra, pinSlotRow{
			id: fmt.Sprintf("pin_%02d", i), project: "proj", category: "fact",
			importance: 0.30 + float32(25-i)*0.01, pinned: true,
		})
	}
	st := pinSlotStore(t, 10, extra...)
	pol := pinUnionPolicy()
	pol.ItemCap, pol.OverFetch = 20, 22
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	head := firstN(passiveIDs(set), 20)
	for i, id := range head {
		if want := fmt.Sprintf("pin_%02d", i); id != want {
			t.Fatalf("head[%d] = %s, want %s (the pinned rows ranked among themselves): %v", i, id, want, head)
		}
	}
	// 22 rows fetched, so 3 pinned rows never entered the window.
	if got := set.PinnedBeyond["proj"]; got != 3 {
		t.Errorf("PinnedBeyond[proj] = %d, want 3 (25 pinned, 22 fetched)", got)
	}
}

// TestPinnedRowsInsideTheWindowReportNothingBeyond: a window with room reports
// no pinned rows beyond it.
func TestPinnedRowsInsideTheWindowReportNothingBeyond(t *testing.T) {
	st := pinSlotStore(t, 10, pinSlotRow{id: "the_pin", project: "proj", category: "fact", importance: 0.1, pinned: true})
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pinUnionPolicy()))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if n := set.PinnedBeyond["proj"]; n != 0 {
		t.Errorf("PinnedBeyond[proj] = %d, want 0", n)
	}
}

// candidateRows returns the set's rows by id, for a test that reads a field.
func pinCandidateByID(set *CandidateSet, id string) (Candidate, bool) {
	for _, r := range set.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return Candidate{}, false
}

// TestASupersededPinnedRowKeepsItsSlotButRanksAfterItsReplacement: a pin
// guarantees a slot, not a rank above the row that replaced it. The superseded
// pinned row stays in the admitted head, directly behind its superseder, and the
// retriever names the superseder on it.
func TestASupersededPinnedRowKeepsItsSlotButRanksAfterItsReplacement(t *testing.T) {
	// A second pin that outranks the superseded one puts a reserved row ahead of
	// it, so the order cannot hold by the two simply being adjacent.
	st := pinSlotStore(t, 30,
		pinSlotRow{id: "the_pin", project: "proj", category: "fact", importance: 0.1, pinned: true},
		pinSlotRow{id: "other_pin", project: "proj", category: "fact", importance: 0.5, pinned: true},
		pinSlotRow{id: "second_pin", project: "proj", category: "fact", importance: 0.2, pinned: true})
	// Two superseded pins, so the demotion's habit of sinking a row to the back
	// cannot leave each one behind its own replacement by accident.
	for _, l := range [][2]string{{"hi_000", "the_pin"}, {"hi_001", "second_pin"}} {
		if err := st.CreateLink(context.Background(), l[0], l[1], "supersedes", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			ids := passiveIDs(set)
			head := firstN(ids, pol.ItemCap)
			if !containsStr(head, "the_pin") {
				t.Fatalf("a superseded pinned row lost its slot: %v", head)
			}
			if got, want := indexOf(ids, "the_pin"), indexOf(ids, "hi_000")+1; got != want {
				t.Errorf("the superseded pin is at %d, want directly behind its replacement at %d: %v", got, want, head)
			}
			if got, want := indexOf(ids, "second_pin"), indexOf(ids, "hi_001")+1; got != want {
				t.Errorf("the second superseded pin is at %d, want directly behind its replacement at %d: %v", got, want, head)
			}
			c, _ := pinCandidateByID(set, "the_pin")
			if len(c.SupersededBy) != 1 || c.SupersededBy[0] != "hi_000" {
				t.Errorf("SupersededBy = %v, want [hi_000]", c.SupersededBy)
			}
		})
	}
}

// TestTheReplacementOfASupersededPinnedRowGetsASlotToo: the replacement ranks
// last among 60 rows at 0.9, so a cap would cut it; because the pinned row it
// replaced holds a slot, the replacement takes one ahead of unpinned rows.
func TestTheReplacementOfASupersededPinnedRowGetsASlotToo(t *testing.T) {
	st := pinSlotStore(t, 60,
		pinSlotRow{id: "the_pin", project: "proj", category: "fact", importance: 0.1, pinned: true},
		pinSlotRow{id: "the_new", project: "proj", category: "fact", importance: 0.05})
	if err := st.CreateLink(context.Background(), "the_new", "the_pin", "supersedes", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	for name, pol := range map[string]SlicePolicy{"session": pinSessionPolicy(), "union": pinUnionPolicy()} {
		t.Run(name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			ids := passiveIDs(set)
			head := firstN(ids, pol.ItemCap)
			if !containsStr(head, "the_new") || !containsStr(head, "the_pin") {
				t.Fatalf("the replacement and the superseded pin must both hold slots: %v", head)
			}
			if indexOf(ids, "the_pin") != indexOf(ids, "the_new")+1 {
				t.Errorf("the superseded pin must sit directly behind its replacement: %v", head)
			}
		})
	}
}
