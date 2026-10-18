package obsidian

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// collisionNotes maps each note under <vault>/ghost/<kind> to the ghost_id its
// front matter carries.
func collisionNotes(t *testing.T, vault, kind string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(vault, "ghost", kind, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		id := ""
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "ghost_id: "); ok {
				id = v
				break
			}
		}
		got[filepath.Base(p)] = id
	}
	return got
}

func noteIDs(notes map[string]string) []string {
	ids := make([]string, 0, len(notes))
	for _, id := range notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestExportKeepsSameTitleRecordsWithASharedIDPrefixApart: a note's name is the
// title slug plus the first 8 bytes of the id, so two records with one title and
// ids that agree on those 8 bytes were given one name, and the second write
// overwrote the first with Export returning nil. Imported ids reach this easily.
// Each record must keep its own note, the record with the smallest id must keep
// the plain name every non-colliding record still gets, and a second pass must
// neither rewrite nor delete either note.
func TestExportKeepsSameTitleRecordsWithASharedIDPrefixApart(t *testing.T) {
	db, store := seedStoreWithDB(t)
	ctx := context.Background()

	// Inserted larger id first, so the smallest-id-keeps-the-plain-name rule is
	// not satisfied by insertion order alone.
	for _, id := range []string{"task-0002", "task-0001"} {
		if _, _, err := store.ImportTask(ctx, memory.Task{
			ID: id, ProjectID: "ghost", Title: "Fix the deploy", Status: "pending", Priority: 2,
		}, true); err != nil {
			t.Fatalf("ImportTask %s: %v", id, err)
		}
	}
	for _, id := range []string{"decision-b", "decision-a"} {
		if _, _, err := store.ImportDecision(ctx, memory.Decision{
			ID: id, ProjectID: "ghost", Title: "Use SQLite", Decision: "SQLite it is", Rationale: "zero infra", Status: "active",
		}, true); err != nil {
			t.Fatalf("ImportDecision %s: %v", id, err)
		}
	}
	for _, id := range []string{"memory-02", "memory-01"} {
		plantMemory(t, db, "ghost", id, "The deploy needs a manual step after every release.")
	}
	if err := store.CreateLink(ctx, "memory-01", "memory-02", "related", 0.9, "auto"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}

	cases := []struct {
		kind, plain string
		ids         []string
	}{
		{"Tasks", fileNameFor("Fix the deploy", "task-0001"), []string{"task-0001", "task-0002"}},
		{"Decisions", fileNameFor("Use SQLite", "decision-a"), []string{"decision-a", "decision-b"}},
		{"Memories", fileName(memory.Memory{ID: "memory-01", Content: "The deploy needs a manual step after every release."}), []string{"memory-01", "memory-02"}},
	}
	for _, tc := range cases {
		notes := collisionNotes(t, vault, tc.kind)
		if got := noteIDs(notes); !reflect.DeepEqual(got, tc.ids) {
			t.Errorf("%s: notes hold ids %v, want %v — one record overwrote the other\n  notes: %v", tc.kind, got, tc.ids, notes)
		}
		if notes[tc.plain] != tc.ids[0] {
			t.Errorf("%s: plain name %s holds %q, want %q (the smallest id keeps the name it always had)\n  notes: %v",
				tc.kind, tc.plain, notes[tc.plain], tc.ids[0], notes)
		}
	}

	// The wikilink from memory-01 must point at memory-02's own note, not at
	// the plain name the two used to share.
	mems := collisionNotes(t, vault, "Memories")
	var m02 string
	for name, id := range mems {
		if id == "memory-02" {
			m02 = name
		}
	}
	body, err := os.ReadFile(filepath.Join(vault, "ghost", "Memories", cases[2].plain))
	if err != nil {
		t.Fatal(err)
	}
	if want := "[[" + strings.TrimSuffix(m02, ".md") + "]]"; m02 == "" || !strings.Contains(string(body), want) {
		t.Errorf("memory-01's note does not link %s:\n%s", want, body)
	}

	// Second pass: no churn and no deletion.
	before := snapshotVault(t, vault)
	mtimes := make(map[string]int64)
	for rel := range before {
		info, err := os.Stat(filepath.Join(vault, rel))
		if err != nil {
			t.Fatal(err)
		}
		mtimes[rel] = info.ModTime().UnixNano()
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("second export: %v", err)
	}
	after := snapshotVault(t, vault)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("second export changed the vault:\n  before: %v\n  after:  %v", keysOf(before), keysOf(after))
	}
	for rel, mt := range mtimes {
		info, err := os.Stat(filepath.Join(vault, rel))
		if err != nil {
			t.Errorf("second export removed %s: %v", rel, err)
			continue
		}
		if info.ModTime().UnixNano() != mt {
			t.Errorf("second export rewrote %s", rel)
		}
	}

	// Retention still keys on the path: deleting the record that held the plain
	// name hands it to the survivor, and the survivor's old note is pruned.
	if err := store.Delete(ctx, "memory-01"); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export after delete: %v", err)
	}
	if got, want := collisionNotes(t, vault, "Memories"), map[string]string{
		fileName(memory.Memory{ID: "memory-02", Content: "The deploy needs a manual step after every release."}): "memory-02",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("after deleting memory-01 the Memories folder holds %v, want %v", got, want)
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestNoteNamesIsOrderIndependentAndKeepsPlainNames pins the naming rule
// itself: a name nobody else wants is left exactly as fileNameFor builds it, a
// shared name goes to the smallest id whatever order the records arrive in, and
// every other record of the group gets a distinct, single-component name.
func TestNoteNamesIsOrderIndependentAndKeepsPlainNames(t *testing.T) {
	recs := []noteRecord{
		{id: "task-0003", name: fileNameFor("Fix the deploy", "task-0003")},
		{id: "task-0001", name: fileNameFor("Fix the deploy", "task-0001")},
		{id: "cafe0000aa", name: fileNameFor("Unrelated", "cafe0000aa")},
		{id: "task-0002", name: fileNameFor("Fix the deploy", "task-0002")},
		{id: "TASK-0001", name: fileNameFor("Fix the deploy", "TASK-0001")}, // case-folds onto the group
	}
	want := noteNames(recs)

	rev := make([]noteRecord, len(recs))
	for i, r := range recs {
		rev[len(recs)-1-i] = r
	}
	if got := noteNames(rev); !reflect.DeepEqual(got, want) {
		t.Errorf("order changed the names:\n  forward: %v\n  reverse: %v", want, got)
	}

	if want["cafe0000aa"] != fileNameFor("Unrelated", "cafe0000aa") {
		t.Errorf("a non-colliding record was renamed to %q", want["cafe0000aa"])
	}
	// "TASK-0001" < "task-0001" bytewise, so it keeps the shared plain name.
	if want["TASK-0001"] != fileNameFor("Fix the deploy", "TASK-0001") {
		t.Errorf("smallest id did not keep its plain name: %v", want)
	}
	seen := make(map[string]string)
	for id, name := range want {
		if prev, dup := seen[strings.ToLower(name)]; dup {
			t.Errorf("%s and %s share %q", prev, id, name)
		}
		seen[strings.ToLower(name)] = id
		if filepath.Base(name) != name || len(name) > 255 || !strings.HasSuffix(name, ".md") {
			t.Errorf("%s got an unusable name %q", id, name)
		}
	}
}

// TestNoteNamesSuffixCannotTakeAnotherRecordsName: the suffixed name a losing
// record is given is itself a name some other record's title and id can build,
// and an imported id can be chosen to build it. The record whose built name it
// is keeps it, and the loser moves to the full-hash name.
func TestNoteNamesSuffixCannotTakeAnotherRecordsName(t *testing.T) {
	sum := sha256.Sum256([]byte("task-0002"))
	h8 := hex.EncodeToString(sum[:])[:8]
	squatter := h8 + "zz"
	recs := []noteRecord{
		{id: "task-0001", name: fileNameFor("Fix the deploy", "task-0001")},
		{id: "task-0002", name: fileNameFor("Fix the deploy", "task-0002")},
		{id: squatter, name: fileNameFor("Fix the deploy task 000", squatter)},
	}
	if recs[2].name != "fix-the-deploy-task-000-"+h8+".md" {
		t.Fatalf("fixture does not build the squatting name: %q", recs[2].name)
	}
	got := noteNames(recs)
	if got[squatter] != recs[2].name {
		t.Errorf("the record whose built name it is lost it: %v", got)
	}
	if got["task-0002"] == got[squatter] || got["task-0002"] == got["task-0001"] {
		t.Errorf("two records share a name: %v", got)
	}
	if want := "fix-the-deploy-task-000-" + hex.EncodeToString(sum[:]) + ".md"; got["task-0002"] != want {
		t.Errorf("task-0002 = %q, want the full-hash name %q", got["task-0002"], want)
	}
}
