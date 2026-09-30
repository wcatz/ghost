package portable

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// TestALegacyDecisionWithAHostileTagSurvivesExportAndImport is #822, and it is the
// decision half of what #817 settled for a memory.
//
// The issue as filed asks for a tag SHAPE check on `ImportDecision`, and that is the
// wrong answer — it is the answer #817 already gave up for memories, having
// measured what it costs. The exporter applies the importer's own predicates
// (`CheckImportedProject`/`Memory`/`Task`/`Decision`), so a tag shape refused at
// import is a tag shape that puts the whole DECISION outside every `ghost export`,
// along with the companion memory `RecordDecision` wrote beside it. Nothing refused
// such a tag on the write path for the whole life of the feature, so any store
// written through it can hold one, and the user loses their decision log on the day
// they need the backup.
//
// So the rule is the memory rule, unchanged: a decision's tags are refused at WRITE
// time by `validateTags` — the same function `ghost_memory_save` uses, called by
// `ghost_decision_record`, which is enumerated as one of the four writers in
// `mcpserver.TestEveryTagBearingWriteToolRefusesATagThatCouldForgeALine` — carried
// byte for byte through export and import, and made safe to READ by the renderer.
//
// The fixture proves the middle clause, because it is the one `ImportDecision`
// could break and does not: it marshals `d.Tags` straight into the column, and
// `CheckImportedDecision` judges a decision's id, project, required fields, status,
// text and alternatives — and never its tags, for shape or anything else. A tag's
// characters are a fact about a RENDERING, not about a record, and a predicate that
// judged them would make an otherwise-valid decision unbacked-up for a label the
// user cannot even see. What this test does NOT hold is the credential guard
// `CheckImportedMemory` applies to a memory's tags: a decision's tags column is
// unguarded for that too, which is a separate gap with its own issue (#835) and is
// deliberately not filled here, because any tag refusal added to import_check.go —
// a shape check or a credential one — puts the decision outside every export all
// over again.
//
// Planted in SQL, because that is the only route to the state: a pre-#811 save, a
// `RestoreSnapshot`, a hand edit, an artifact from another tool.
func TestALegacyDecisionWithAHostileTagSurvivesExportAndImport(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	projectID := seed(t, src)

	// The same four shapes the memory fixture plants, and the reasoning for their
	// spelling is that fixture's: the column holds a JSON array, so a control
	// character must be planted as its JSON ESCAPE. A raw NUL or newline inside a
	// JSON string is not valid JSON, the decoder's error is ignored, and the row
	// arrives with NO tags at all — which reads as "the export lost the tag" when it
	// is really "the fixture was not a row".
	hostile := map[string]string{
		"d-guillemet": `["«urgent»"]`,
		"d-backtick":  `["a` + "`" + `b"]`,
		"d-newline":   `["a\nb"]`,
		"d-nul":       `["a\u0000b"]`,
	}
	for id, tags := range hostile {
		plantExportDecisionWithTags(t, db, id, projectID, tags)
	}
	// An ordinary one, so a filter written too NARROW — one that dropped any row
	// whose tags column merely parsed — would be caught here rather than passing on
	// a hostile row a renderer already handles.
	plantExportDecisionWithTags(t, db, "d-ordinary", projectID, `["ci timeouts","日本語"]`)

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	artifact := buf.Bytes()

	// (1) nothing is left out, and said independently of the report: a filter that
	// dropped a row AND hid the skip would pass a report-only assertion.
	for id := range hostile {
		for _, sk := range stats.Skipped {
			if sk.Type == TypeDecision && sk.ID == id {
				t.Errorf("the export left the decision %q out (%s) because of its tags; a label on an otherwise "+
					"exportable record is not a reason to drop the record", id, sk.Reason)
			}
		}
	}
	if got, want := stats.Decisions, len(plantedDecisionIDs(hostile, "d-ordinary"))+1; got != want {
		// The +1 is the decision `seed` writes; the planted rows are the rest.
		t.Errorf("the export wrote %d decision(s), want the %d the fixture holds", got, want)
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
	dstDecisions, err := dst.ListDecisions(ctx, projectID, "", 100)
	if err != nil {
		t.Fatalf("ListDecisions(dst): %v", err)
	}
	// Every planted decision arrives, with its tags BYTE FOR BYTE. Not "similar",
	// and not the neutralised form: the value the user's store holds, because a
	// restore that rewrites a label is a restore that does not restore.
	want := map[string]string{
		"d-guillemet": "«urgent»",
		"d-backtick":  "a`b",
		"d-newline":   "a\nb",
		"d-nul":       "a\x00b",
	}
	for id, tag := range want {
		got := decisionTagsOf(dstDecisions, id)
		if got == nil {
			t.Errorf("the decision %q did not survive the round trip", id)
			continue
		}
		if len(got) != 1 || got[0] != tag {
			t.Errorf("%s came back with tags %q, want exactly [%q] byte for byte", id, got, tag)
		}
	}
	if got := decisionTagsOf(dstDecisions, "d-ordinary"); len(got) != 2 || got[0] != "ci timeouts" || got[1] != "日本語" {
		t.Errorf("the ordinary decision came back with tags %q, want [ci timeouts 日本語]", got)
	}

	// (3) and the restored decision still renders SAFE. A decision's own tags are
	// not rendered by any listing today — `ghost_decisions_list` prints the title,
	// decision, rationale and alternatives, and no tag — so the surface that carries
	// them is the COMPANION memory `RecordDecision` wrote from the same tag list,
	// which is an ordinary memory: assembled into every search row and quoted into
	// the next reflect prompt. That is asserted here rather than asserted absent,
	// because the exposure is real and `assemble.TagsLabel` is the layer that closes
	// it — the same one the memory fixture of #817 asserts.
	for _, id := range []string{"d-guillemet", "d-backtick", "d-newline", "d-nul"} {
		tags := decisionTagsOf(dstDecisions, id)
		if tags == nil {
			continue
		}
		label := assemble.TagsLabel(tags)
		if bytes.Contains([]byte(label), []byte("`")) {
			t.Errorf("%s: the tag label carries a raw backtick into a listing: %q", id, label)
		}
		line := assemble.Item{
			ID: id, Category: "decision", Content: "planted", Tags: tags,
		}.Line()
		if n := bytes.Count([]byte(line), []byte("«")); n != 1 {
			t.Errorf("%s: a decision's tags render %d opening data delimiters, want only the content's:\n%s", id, n, line)
		}
		if !bytes.HasSuffix([]byte(line), []byte("«planted»")) {
			t.Errorf("%s: the row does not end in the content's own data block:\n%s", id, line)
		}
	}
}

// plantedDecisionIDs is the fixture's own arithmetic, so the count assertion reads
// as "every row I planted plus the seeder's" rather than as a literal that a
// changed fixture would silently falsify.
func plantedDecisionIDs(hostile map[string]string, ordinary string) []string {
	ids := make([]string, 0, len(hostile)+1)
	for id := range hostile {
		ids = append(ids, id)
	}
	return append(ids, ordinary)
}

func decisionTagsOf(rows []memory.Decision, id string) []string {
	for _, d := range rows {
		if d.ID == id {
			return d.Tags
		}
	}
	return nil
}

func plantExportDecisionWithTags(t *testing.T, db *sql.DB, id, projectID, tags string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status, tags, created_at, updated_at)
	                          VALUES (?, ?, 'planted', 'd', 'r', 'active', ?, datetime('now'), datetime('now'))`,
		id, projectID, tags); err != nil {
		t.Fatalf("plant a decision with tags %s under id %q: %v", tags, id, err)
	}
}
