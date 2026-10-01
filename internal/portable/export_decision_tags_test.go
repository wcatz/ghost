package portable

import (
	"bytes"
	"context"
	"database/sql"
	"strconv"
	"strings"
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
// `CheckImportedDecision` judges a decision's id, project, required fields, status
// and text, and judges its tags for CREDENTIALS ONLY (see below) — never for shape.
// A tag's characters are a fact about a RENDERING, not about a record, and a
// predicate that judged them would make an otherwise-valid decision unbacked-up
// for a label the user cannot even see.
//
// What this test does NOT hold is a CREDENTIAL guard, and that omission is now a
// decision rather than an accident. #835 added one: `CheckImportedDecision` asks
// `rejectSecretList("tags", …)`, exactly as `CheckImportedMemory` does, so a
// decision carrying a token-shaped tag is NAMED in the export's skipped list rather
// than re-emitted into the file an operator is about to paste somewhere. That is
// the one tag refusal which does put a decision outside every export, and it is
// accepted for the reason the memory rule accepts the same cost on content,
// source_ref, agent, session_id and evidence: the value is the leak, the label is
// not, and a `!` line naming the field is a repair rather than a silent loss. So
// the four shapes below stay in the artifact and round-trip byte for byte, and a
// credential does not — which is the whole distinction the two halves of this file
// draw. `TestADecisionTagCarryingACredentialIsNamedNotCarried` is the other half.
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

// TestADecisionTagCarryingACredentialIsNamedNotCarried is #835, and it is the other
// half of the fixture above.
//
// The rule a memory's tags already follow is now a decision's tags, clause for
// clause, and this holds three of the four:
//
//	import  — refused, naming the FIELD and never the value;
//	export  — left out and NAMED, with `Secret` set so the report prints the
//	          credential advice instead of a bare delete suggestion;
//	ordinary — carried byte for byte, because the guard refuses a credential SHAPE
//	          and not a tag, exactly as `CheckImportedMemory` behaves.
//
// The fourth clause is the write path, and it is not re-asserted here because it
// changed nothing: `RecordDecision` has asked `rejectSecretList("tags", …)` since
// #656 (internal/memory/decisions.go, and the RecordDecision case of
// `TestEveryTagBearingWriterIsGuarded`). What was missing was the OTHER route into
// the same column — `ImportDecision`'s raw INSERT — and the export side that shares
// the importer's predicate.
//
// The token shape is the OpenAI legacy key, `sk-` plus 48 base62 characters, which
// is what `internal/secret`'s `openai-key` rule matches (`\bsk-[A-Za-z0-9]{48}\b`,
// pinned to that exact length with a word boundary so a hostname beginning `sk-`
// cannot match). The issue's reproduction shape — `sk-` plus THIRTY-TWO — was
// measured against this build's detector and is not one of its findings: the rule
// needs 48, so a 32-character tail is a false negative of the DETECTOR and no tag
// check can close it. The rule here inherits the detector's floors rather than
// adding new ones, which is why the fixture uses a shape the detector recognises.
func TestADecisionTagCarryingACredentialIsNamedNotCarried(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	projectID := seed(t, src)
	// Assembled rather than written out, and never as a bare literal: a complete
	// provider token in a diff is what push protection matches.
	credential := "sk-" + strings.Repeat("a1B2c3D4e5F6", 4)

	plantExportDecisionWithTags(t, db, "d-cred", projectID,
		`["ops",`+strconv.Quote(credential)+`]`)
	// An ordinary tag list in the SAME export, because a fix that refused every
	// tagged decision would satisfy every assertion in this test.
	plantExportDecisionWithTags(t, db, "d-plain", projectID, `["pool keys","cardano"]`)

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	artifact := buf.Bytes()

	// (1) NAMED, as a decision, with the credential marker set — which is what
	// makes the CLI print the field-level advice rather than the delete sentence.
	var found *SkippedRecord
	for i, sk := range stats.Skipped {
		if sk.Type == TypeDecision && sk.ID == "d-cred" {
			found = &stats.Skipped[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the export reported no decision \"d-cred\" as left out, so a credential "+
			"in a decision's tags was carried into the artifact with no report; it named %v", stats.Skipped)
	}
	if !found.Secret {
		t.Error("the refusal was not marked Secret, so the report cannot name the field to fix")
	}
	if !strings.Contains(found.Reason, "tags[1]") {
		t.Errorf("the reason does not name the tag to fix: %q", found.Reason)
	}
	// (2) Not in the artifact, and not in the struct the report is built from: the
	// reason is the only string the export hands a writer, so it is the one place
	// a leak would appear.
	if strings.Contains(found.Reason, credential) {
		t.Errorf("the export reason printed the credential itself: %q", found.Reason)
	}
	if bytes.Contains(artifact, []byte(credential)) {
		t.Error("the artifact still carries the credential out of a decision's tags column")
	}
	if bytes.Contains(artifact, []byte(`"d-cred"`)) {
		t.Error("the artifact still carries the refused decision")
	}

	// (3) The ordinary decision beside it is untouched, in both directions.
	dst := newTestStore(t)
	report, err := Import(ctx, dst, bytes.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("the exported artifact was rejected %d time(s) by its own importer: %v", report.Rejected, report.Errors)
	}
	rows, err := dst.ListDecisions(ctx, projectID, "", 100)
	if err != nil {
		t.Fatalf("ListDecisions(dst): %v", err)
	}
	got := decisionTagsOf(rows, "d-plain")
	if len(got) != 2 || got[0] != "pool keys" || got[1] != "cardano" {
		t.Errorf("the ordinary decision came back with tags %q, want [pool keys cardano]", got)
	}
}

// TestADecisionTagCarryingACredentialIsRefusedAtImport states the same rule from the
// importer's side, because "the exporter refused it" is a statement about the
// exporter. This is the refusal `ghost import` returns for an ARTIFACT carrying it,
// and the refusal is the sentence the operator sees when the file arrived from
// somewhere else — so it names the field and holds no copy of the value.
func TestADecisionTagCarryingACredentialIsRefusedAtImport(t *testing.T) {
	credential := "sk-" + strings.Repeat("a1B2c3D4e5F6", 4)
	d := memory.Decision{
		ID: "d1", ProjectID: "p1", Title: "rotate the pool keys",
		Decision: "rotate them quarterly", Rationale: "quarterly is enough",
		Status: "active", Tags: []string{"ops", credential},
	}
	err := memory.CheckImportedDecision(d)
	if err == nil {
		t.Fatal("CheckImportedDecision accepted a credential-shaped tag, so an artifact " +
			"carrying one is written verbatim and re-emitted by the next export")
	}
	if !strings.Contains(err.Error(), "tags[1]") {
		t.Errorf("the refusal does not name the tag to fix: %v", err)
	}
	if strings.Contains(err.Error(), credential) {
		t.Errorf("the refusal printed the value: %v", err)
	}
	// The same predicate the memory's own tag check uses, asked about the same
	// value — one rule and one sentence, so a decision and a memory cannot drift
	// apart on the field an operator has to fix.
	memRefused := memory.CheckImportedMemory(memory.PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "an ordinary fact",
		Source: "mcp", Tags: []string{"ops", credential},
	})
	if memRefused == nil {
		t.Fatal("CheckImportedMemory accepted the same value as a tag, so there is no " +
			"memory rule for the decision one to mirror")
	}
	if got, want := err.Error(), memRefused.Error(); got != want {
		t.Errorf("the decision's refusal is a second phrasing of the memory rule:\n  memory:   %q\n  decision: %q", want, got)
	}
	// And an ordinary tag list is still accepted — a guard that refused tags would
	// fail this and quietly make every decision unbacked-up.
	ordinary := d
	ordinary.Tags = []string{"pool keys", "cardano"}
	if err := memory.CheckImportedDecision(ordinary); err != nil {
		t.Errorf("an ordinary tag list was refused: %v", err)
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
