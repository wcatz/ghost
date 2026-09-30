package portable

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// TestALegacyMemoryWithAHostileTagSurvivesExportAndImport is the round trip that
// matters, and it is the inverse of the one this branch first wrote.
//
// The first attempt put a tag-shape refusal on the import path and applied the
// importer's own checks in the exporter, so export→import would stay a round trip.
// That is a coherent rule and it loses data: `validateTags` checked only count and
// length, so `ghost_memory_save` with tags ["«urgent»"] was accepted, and the row
// was then LEFT OUT of every backup — silently for the user, loudly for the
// operator, on the day they needed the file. An import guard cannot protect a store
// it never sees, and the guard was on the wrong side of the only boundary an
// ordinary save passes through.
//
// So a tag is carried byte for byte and the RENDERER neutralises it. This test
// plants the row by raw SQL because that is the only way to reach the state — a
// pre-#811 save, a restored snapshot, a hand edit — and then asserts the three
// things that together mean "the user does not lose this memory":
//
//  1. the export KEEPS it, and says nothing about skipping it;
//  2. a fresh store takes the artifact with no rejection, tags verbatim;
//  3. the row still renders neutralised, so the stored value is legible without
//     being able to open a data block on the line.
func TestALegacyMemoryWithAHostileTagSurvivesExportAndImport(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	projectID := seed(t, src)

	// The shapes a store can already hold, none of which any writer would produce
	// now. Planted in SQL because that is the only route to them.
	//
	// The CONTROL characters are planted as their JSON ESCAPES, because the column
	// holds a JSON array and a raw NUL or newline inside a JSON string is not valid
	// JSON — `scanPortableMemory` ignores the unmarshal error and the row arrives
	// with NO tags at all, which reads as "the export lost the tag" when it is
	// really "the fixture was not a row". The first version planted them raw and
	// failed exactly that way.
	hostile := map[string]string{
		"m-guillemet": `["«urgent»"]`,
		"m-backtick":  `["a` + "`" + `b"]`,
		"m-newline":   `["a\nb"]`,
		"m-nul":       `["a\u0000b"]`,
	}
	for id, tags := range hostile {
		plantExportMemoryWithTags(t, db, id, projectID, tags)
	}
	// An ordinary one, so the fixture is not entirely made of problems and a filter
	// written too NARROW would be caught here rather than on a hostile row that a
	// renderer already handles.
	plantExportMemoryWithTags(t, db, "m-ordinary", projectID, `["ci timeouts","日本語"]`)

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	artifact := buf.Bytes()

	// (1) nothing is left out. This is the assertion the whole change turns on: a
	// row must never cost itself a place in a backup because of a tag.
	for id := range hostile {
		for _, sk := range stats.Skipped {
			if sk.Type == TypeMemory && sk.ID == id {
				t.Errorf("the export left %q out (%s) because of its tags; a label on an otherwise exportable row "+
					"is not a reason to drop the row", id, sk.Reason)
			}
		}
	}
	// And the count says so independently of the report, so a filter that dropped
	// a row AND hid the skip would still be caught. It is `stats.Memories` RATHER
	// THAN the store's row count because `seed` also writes a decision's companion
	// memory, so the store holds three more rows than the fixture's own; a count
	// that mixed the two reported a mismatch that had nothing to do with tags.
	before, err := src.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories(src): %v", err)
	}
	if stats.Memories != len(before) {
		t.Errorf("the export wrote %d memories, want the %d the store holds; tags cost a record a place in a "+
			"backup", stats.Memories, len(before))
	}

	// (2) the round trip, into a store that holds nothing.
	dst := newTestStore(t)
	report, err := Import(ctx, dst, bytes.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("the exported artifact was rejected %d time(s) by its own importer: %v", report.Rejected, report.Errors)
	}
	dstMemories, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories(dst): %v", err)
	}
	// Every planted memory arrives, with its tags BYTE FOR BYTE. Not "similar" and
	// not "the neutralised form" — the value the user's store holds, because a
	// restore that rewrites a label is a restore that does not restore.
	want := map[string]string{
		"m-guillemet": "«urgent»",
		"m-backtick":  "a`b",
		"m-newline":   "a\nb",
		"m-nul":       "a\x00b",
	}
	for id, tag := range want {
		if !hasMemory(dstMemories, id) {
			t.Errorf("the memory %q did not survive the round trip", id)
			continue
		}
		got := tagsOf(dstMemories, id)
		if len(got) != 1 || got[0] != tag {
			t.Errorf("%s came back with tags %q, want exactly [%q] byte for byte", id, got, tag)
		}
	}
	if !hasMemory(dstMemories, "m-ordinary") {
		t.Error("an ordinary memory did not survive the round trip")
	} else if got := tagsOf(dstMemories, "m-ordinary"); len(got) != 2 || got[0] != "ci timeouts" || got[1] != "日本語" {
		t.Errorf("the ordinary memory came back with tags %q, want [ci timeouts 日本語]", got)
	}

	// (3) and the restored row still renders neutralised. The round trip is only
	// half the property: the value is the user's, and it must not be able to open a
	// data block on the line an agent reads.
	for _, id := range []string{"m-guillemet", "m-backtick", "m-newline", "m-nul"} {
		line := assemble.Item{
			ID: id, Category: "gotcha", Content: "planted",
			Tags: tagsOf(dstMemories, id),
		}.Line()
		// Asserted on the LABEL, not on the whole line: a row always carries
		// backticks around its id, so "no backtick in the line" is unsatisfiable
		// and would have failed every case for a reason that has nothing to do with
		// the tag. The label is the span the tag reaches, and the first version of
		// this asserted the line and failed exactly that way.
		label := assemble.TagsLabel(tagsOf(dstMemories, id))
		if bytes.Contains([]byte(label), []byte("`")) {
			t.Errorf("%s: the tag label carries a raw backtick into a listing: %q", id, label)
		}
		if n := bytes.Count([]byte(line), []byte("«")); n != 1 {
			t.Errorf("%s: the restored row renders %d opening data delimiters, want only the content's:\n%s", id, n, line)
		}
		if n := bytes.Count([]byte(line), []byte("»")); n != 1 {
			t.Errorf("%s: the restored row renders %d closing data delimiters, want only the content's:\n%s", id, n, line)
		}
		// And the label ends the line's metadata: whatever the tag held, the
		// content's own block is what closes the row.
		if !bytes.HasSuffix([]byte(line), []byte("«planted»")) {
			t.Errorf("%s: the restored row does not end in the content's own data block:\n%s", id, line)
		}
	}
}

func tagsOf(rows []memory.PortableMemory, id string) []string {
	for _, m := range rows {
		if m.ID == id {
			return m.Tags
		}
	}
	return nil
}

func plantExportMemoryWithTags(t *testing.T, db *sql.DB, id, projectID, tags string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
	                          VALUES (?, ?, 'gotcha', 'planted', 'mcp', ?, datetime('now'), datetime('now'))`, id, projectID, tags); err != nil {
		t.Fatalf("plant a memory with tags %s under id %q: %v", tags, id, err)
	}
}
