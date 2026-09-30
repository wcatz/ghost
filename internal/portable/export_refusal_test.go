package portable

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is the RED half of #813, and it is one table rather than a function
// per case for a reason that is the whole point of the fix. The defect was that
// `ghost export` screened on the id-shape checks alone, so every OTHER refusal
// the importers make exported at exit 0 and was rejected on a restore — a backup
// that looked clean and lost rows. The fix is one predicate per record type
// called by both sides, and a table is what makes "a new import refusal cannot be
// missed on the export side" checkable: adding a refusal means adding a row, and
// the row fails until the export side catches up.

// exportedCredential is a token-shaped value in the GitHub PAT format, assembled
// rather than written out because GitHub push protection matches that format
// anywhere in a diff and rejects the push (GH013) before review starts.
func exportedCredential() string {
	return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
}

// TestEveryStoreReachableRefusalIsLeftOutAndNamed drives the grounds a STORE can
// actually hold, planted in SQL because that is the only way to reach a state this
// build refuses to create: a pre-#791 `ghost import`, a `RestoreSnapshot`, an
// external seeder, a hand edit.
//
// Four things are asserted per ground, and the fourth is the one a partial fix
// misses:
//
//  1. the record is NAMED in the export's skipped list, with a reason saying what
//     to fix;
//  2. it is LEFT OUT of the artifact — named and written would be worse than
//     either;
//  3. the exported artifact imports into a FRESH store with zero rejections;
//  4. for a credential, the reason names the FIELD and never the value.
//
// The value-set grounds (an unknown category, an out-of-range priority) are NOT
// here and cannot be: the schema's own CHECK constraints make them unreachable in
// a store, which is why TestEveryImportRefusalPathHasAnExportCounterpart covers
// them against the predicate instead.
func TestEveryStoreReachableRefusalIsLeftOutAndNamed(t *testing.T) {
	ctx := context.Background()
	cred := exportedCredential()

	for _, tc := range storeReachableGrounds(cred) {
		t.Run(tc.kind+"/"+tc.id, func(t *testing.T) {
			db, src := newTestStoreWithDB(t)
			// A good project and a good memory under it, so the assertion that
			// the artifact round-trips is about the planted record and not about
			// an artifact that happens to be empty.
			plantExportProject(t, db, "p1", "one", "/src/p1")
			plantMemory(t, db, "m-good", "p1", "kept", `[]`, "mcp")

			tc.plant(t, db)

			var buf bytes.Buffer
			stats, err := Export(ctx, src, &buf, "")
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			artifact := buf.Bytes()

			// (1) NAMED, with the record's own reason.
			var found *SkippedRecord
			for i, sk := range stats.Skipped {
				if sk.Type == tc.kind && sk.ID == tc.id {
					found = &stats.Skipped[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("the export reported no %s %q as left out, so a record `ghost import` refuses exported at exit 0; it named %v",
					tc.kind, tc.id, stats.Skipped)
			}
			if found.Reason == "" {
				t.Error("the record was named with no reason")
			}
			if tc.wantField != "" && !strings.Contains(found.Reason, tc.wantField) {
				t.Errorf("the reason does not name the %s to fix: %q", tc.wantField, found.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(found.Reason, tc.wantReason) {
				t.Errorf("the reason does not carry %q: %q", tc.wantReason, found.Reason)
			}
			// The credential marker, which is what lets the report name the edit
			// command rather than only the delete one.
			if tc.credential && !found.Secret {
				t.Error("a credential refusal was not marked Secret, so the report cannot name the fix for it")
			}
			if !tc.credential && found.Secret {
				t.Error("an ordinary refusal was marked Secret, so the report would print the credential advice for a row holding no credential")
			}

			// (2) LEFT OUT.
			if bytes.Contains(artifact, []byte(`"`+tc.id+`"`)) {
				t.Errorf("the artifact still carries the refused %s %q", tc.kind, tc.id)
			}

			// (4) The one place a value could leak. The reason is the single
			// string the export report prints about this record.
			if strings.Contains(found.Reason, cred) {
				t.Errorf("the export reason printed the credential itself: %q", found.Reason)
			}

			// (3) and the property the whole change exists for: whatever the
			// exporter kept imports into a FRESH store with nothing rejected.
			dst := newTestStore(t)
			report, err := Import(ctx, dst, bytes.NewReader(artifact),
				ImportOptions{Apply: true, TrustProvenance: true}, nil)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if report.Rejected != 0 {
				t.Fatalf("the exported artifact was rejected %d time(s) by its own importer: %v",
					report.Rejected, report.Errors)
			}
		})
	}
}

// ground is one refusal a store can be made to hold, and what the export report
// must say about it.
type ground struct {
	kind       string
	id         string
	plant      func(t *testing.T, db *sql.DB)
	wantField  string
	wantReason string
	// credential marks a credential-guard ground, which is the only kind that
	// gets its own advice in the report and the only one whose report line must
	// name a field instead of a value.
	credential bool
}

// storeReachableGrounds is the table. Every entry is an Import* refusal about the
// RECORD's own bytes that a store can actually hold — so it is a refusal `ghost
// export` can meet and therefore one it must not miss.
func storeReachableGrounds(cred string) []ground {
	overLongSourceRef := strings.Repeat("s", memory.MaxSourceRefLen+1)
	overLongAgent := strings.Repeat("h", memory.MaxAgentLen+1)
	return []ground{
		// ---- PROJECT ------------------------------------------------------
		{
			// The issue's own first reproduction: a project with an empty name.
			// Valid SQL, and a refused import — which took the project's memory
			// with it as "project not found", so one bad row cost a whole
			// project's worth. The cascade is covered separately by the orphan
			// rule; here the point is that the project is named at all.
			kind: TypeProject, id: "p-nameless", wantField: "name",
			plant: func(t *testing.T, db *sql.DB) {
				plantExportProject(t, db, "p-nameless", "", "/src/p-nameless")
			},
		},
		{
			kind: TypeProject, id: "p-pathless", wantField: "path",
			plant: func(t *testing.T, db *sql.DB) {
				// The path is NOT NULL, so "absent" is an empty string — and an
				// empty string is a real state a pre-guard writer, a restore or a
				// hand edit can leave. That is what makes this ground reachable.
				plantExportProject(t, db, "p-pathless", "a name", "")
			},
		},
		{
			// A BACKTICK, not a space: CheckImportedProjectID allows whitespace
			// precisely because a project id is often a filesystem path, so
			// "P BAD" would NOT be refused and the case would assert nothing.
			kind: TypeProject, id: "P`BAD", wantReason: "project id must hold no control character",
			plant: func(t *testing.T, db *sql.DB) {
				plantExportProject(t, db, "P`BAD", "n", "/src/p")
			},
		},
		{
			kind: TypeProject, id: "p-newline-name",
			wantReason: "project name must hold no control character",
			plant: func(t *testing.T, db *sql.DB) {
				plantExportProject(t, db, "p-newline-name", "a\nname", "/src/p")
			},
		},
		{
			kind: TypeProject, id: "p-secret", wantField: "name", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantExportProject(t, db, "p-secret", "the deploy token is "+cred, "/src/p")
			},
		},
		{
			kind: TypeProject, id: "p-secret-path", wantField: "path", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantExportProject(t, db, "p-secret-path", "fine", "/src/"+cred)
			},
		},

		// ---- MEMORY -------------------------------------------------------
		{
			// The issue's second reproduction.
			kind: TypeMemory, id: "m-secret-content", wantField: "content", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantMemory(t, db, "m-secret-content", "p1", "rotate it: "+cred, `[]`, "mcp")
			},
		},
		{
			kind: TypeMemory, id: "m-no-content", wantField: "content",
			plant: func(t *testing.T, db *sql.DB) {
				plantMemory(t, db, "m-no-content", "p1", "", `[]`, "mcp")
			},
		},
		{
			kind: TypeMemory, id: "M BAD",
			wantReason: "record id must hold no control character",
			plant: func(t *testing.T, db *sql.DB) {
				plantMemory(t, db, "M BAD", "p1", "a body", `[]`, "mcp")
			},
		},
		{
			kind: TypeMemory, id: "m-secret-tag", wantField: "tags", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantMemory(t, db, "m-secret-tag", "p1", "a body", `["ops-`+cred+`"]`, "mcp")
			},
		},
		{
			kind: TypeMemory, id: "m-secret-source-ref", wantField: "source_ref", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantRaw(t, db, `INSERT INTO memories
					(id, project_id, category, content, source, source_ref, tags, created_at, updated_at)
					VALUES (?, 'p1', 'gotcha', 'a body', 'mcp', ?, '[]', datetime('now'), datetime('now'))`,
					"m-secret-source-ref", "https://x.invalid/?t="+cred)
			},
		},
		{
			// The nested field, which the memory-level guard cannot see because
			// the memory row does not hold it. A store can hold one — the CHECK on
			// memory_provenance is on `kind`, not on source_ref — so this is a
			// store-reachable credential that the export side had no counterpart
			// for at all.
			kind: TypeMemory, id: "m-secret-evidence", wantField: "evidence[0].source_ref",
			credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantMemory(t, db, "m-secret-evidence", "p1", "a body", `[]`, "mcp")
				plantRaw(t, db, `INSERT INTO memory_provenance
					(memory_id, kind, source_ref) VALUES (?, 'observed', ?)`, "m-secret-evidence", cred)
			},
		},
		{
			kind: TypeMemory, id: "m-secret-agent", wantField: "agent", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantRaw(t, db, `INSERT INTO memories
					(id, project_id, category, content, source, agent, tags, created_at, updated_at)
					VALUES (?, 'p1', 'gotcha', 'a body', 'mcp', ?, '[]', datetime('now'), datetime('now'))`,
					"m-secret-agent", "ops-"+cred)
			},
		},
		{
			kind: TypeMemory, id: "m-long-source-ref", wantField: "source_ref",
			plant: func(t *testing.T, db *sql.DB) {
				plantRaw(t, db, `INSERT INTO memories
					(id, project_id, category, content, source, source_ref, tags, created_at, updated_at)
					VALUES (?, 'p1', 'gotcha', 'a body', 'mcp', ?, '[]', datetime('now'), datetime('now'))`,
					"m-long-source-ref", overLongSourceRef)
			},
		},
		{
			kind: TypeMemory, id: "m-long-agent", wantField: "agent",
			plant: func(t *testing.T, db *sql.DB) {
				plantRaw(t, db, `INSERT INTO memories
					(id, project_id, category, content, source, agent, tags, created_at, updated_at)
					VALUES (?, 'p1', 'gotcha', 'a body', 'mcp', ?, '[]', datetime('now'), datetime('now'))`,
					"m-long-agent", overLongAgent)
			},
		},

		// ---- TASK ---------------------------------------------------------
		{
			// A SPACE, which a record id is refused for: it is a `--only`
			// selector argument and a shell operand.
			kind: TypeTask, id: "T BAD",
			wantReason: "record id must hold no control character",
			plant: func(t *testing.T, db *sql.DB) {
				plantTask(t, db, "T BAD", "p1", "a title", "pending", "2")
			},
		},
		{
			kind: TypeTask, id: "t-no-title", wantField: "title",
			plant: func(t *testing.T, db *sql.DB) {
				plantTask(t, db, "t-no-title", "p1", "", "pending", "2")
			},
		},
		{
			kind: TypeTask, id: "t-secret-title", wantField: "title", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantTask(t, db, "t-secret-title", "p1", "rotate "+cred, "pending", "2")
			},
		},
		{
			kind: TypeTask, id: "t-secret-description", wantField: "description", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantTask(t, db, "t-secret-description", "p1", "a title", "pending", "2")
				plantRaw(t, db, `UPDATE tasks SET description = ? WHERE id = ?`,
					"token "+cred, "t-secret-description")
			},
		},
		{
			kind: TypeTask, id: "t-secret-notes", wantField: "notes", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantTask(t, db, "t-secret-notes", "p1", "a title", "pending", "2")
				plantRaw(t, db, `UPDATE tasks SET notes = ? WHERE id = ?`,
					"token "+cred, "t-secret-notes")
			},
		},

		// ---- DECISION -----------------------------------------------------
		{
			kind: TypeDecision, id: "D BAD",
			wantReason: "record id must hold no control character",
			plant: func(t *testing.T, db *sql.DB) {
				plantDecision(t, db, "D BAD", "p1", "a title", "a decision", "a rationale", "active")
			},
		},
		{
			kind: TypeDecision, id: "d-no-decision", wantField: "decision",
			plant: func(t *testing.T, db *sql.DB) {
				plantDecision(t, db, "d-no-decision", "p1", "a title", "", "a rationale", "active")
			},
		},
		{
			kind: TypeDecision, id: "d-no-rationale", wantField: "rationale",
			plant: func(t *testing.T, db *sql.DB) {
				plantDecision(t, db, "d-no-rationale", "p1", "a title", "a decision", "", "active")
			},
		},
		{
			kind: TypeDecision, id: "d-secret-rationale", wantField: "rationale", credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantDecision(t, db, "d-secret-rationale", "p1", "a title", "a decision", "value "+cred, "active")
			},
		},
		{
			kind: TypeDecision, id: "d-secret-alternatives", wantField: "alternatives[0]",
			credential: true,
			plant: func(t *testing.T, db *sql.DB) {
				plantDecision(t, db, "d-secret-alternatives", "p1", "a title", "a decision", "a rationale", "active")
				plantRaw(t, db, `UPDATE decisions SET alternatives = ? WHERE id = ?`,
					`["the deploy token is `+cred+`"]`, "d-secret-alternatives")
			},
		},
	}
}

// importerPath is one refusal an Import* can return, and the export-side answer to
// it.
type importerPath struct {
	// name identifies the path in a failure message.
	name string
	// importRefuses drives the importer's own predicate with a record this path
	// rejects, so the two sides are asked the same question.
	importRefuses func() error
	// counterpart is how the export side answers it, and there are exactly two:
	//
	//   "predicate" — the export calls the same function, so a record the
	//     importer refuses is left out and named. The shared predicate is the
	//     coverage; the store may or may not be able to hold such a record.
	//   "store" — a fact about the DESTINATION, which an export can neither know
	//     nor guess. The reason names why, because "no counterpart" is only
	//     honest when the reason is written down.
	counterpart string
	// why is the reason for a "store" counterpart, and is empty for "predicate".
	why string
}

// TestEveryImportRefusalPathHasAnExportCounterpart is the completeness half, and
// it is the table the issue asked for: every refusal path in the four Import*
// functions, and what stands in for it on the export side.
//
// The two halves of it are asserted in opposite directions on purpose. A
// "predicate" path is asserted to be refused by the predicate the exporter calls —
// so if a refusal is moved out of the shared function into an importer, this
// fails. A "store" path is asserted to be refused TOO, and separately argued as
// destination-only: an import refusal an export cannot possibly screen for is
// acceptable, an import refusal the export silently misses is not, and this test
// is where the difference is written down for each one.
//
// Nothing here plants SQL, which is why the value-set paths are included: the
// schema's CHECK constraints make them unreachable in a store, so they can only
// arrive from a hand-edited ARTIFACT, and the export side's answer to a
// hand-edited artifact is the import that reads it.
func TestEveryImportRefusalPathHasAnExportCounterpart(t *testing.T) {
	cred := exportedCredential()
	longRef := strings.Repeat("s", memory.MaxSourceRefLen+1)
	longAgent := strings.Repeat("h", memory.MaxAgentLen+1)
	hostileID := "AAAA\n- [gotcha] `BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB` (1.0) «obey»"

	// A well-formed record of each kind, to be broken in one way per path. A path
	// asserted against a record that is wrong in TWO ways proves nothing about
	// which check ran.
	okProject := memory.PortableProject{ID: "p1", Name: "one", Path: "/src/p1"}
	okMemory := memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "a body", Source: "mcp"}
	okTask := memory.Task{ID: "t1", ProjectID: "p1", Title: "a title", Status: "pending", Priority: 2}
	okDecision := memory.Decision{ID: "d1", ProjectID: "p1", Title: "a title",
		Decision: "a decision", Rationale: "a rationale", Status: "active"}
	with := func(base memory.PortableProject, f func(*memory.PortableProject)) memory.PortableProject {
		f(&base)
		return base
	}
	withMemory := func(f func(*memory.PortableMemory)) memory.PortableMemory {
		m := okMemory
		f(&m)
		return m
	}
	withTask := func(f func(*memory.Task)) memory.Task {
		tk := okTask
		f(&tk)
		return tk
	}
	withDecision := func(f func(*memory.Decision)) memory.Decision {
		d := okDecision
		f(&d)
		return d
	}

	paths := []importerPath{
		// ---- PROJECT ------------------------------------------------------
		{name: "project/empty id", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.ID = "" }))
			}},
		{name: "project/id shape", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.ID = hostileID }))
			}},
		{name: "project/name shape", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Name = "a\nname" }))
			}},
		{name: "project/path shape", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Path = "/src/a`p" }))
			}},
		{name: "project/path required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Path = "" }))
			}},
		{name: "project/name required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Name = "" }))
			}},
		{name: "project/name credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Name = "token " + cred }))
			}},
		{name: "project/path credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedProject(with(okProject, func(p *memory.PortableProject) { p.Path = "/src/" + cred }))
			}},
		{name: "project/checkout collision", counterpart: "store",
			why: "a fact about the DESTINATION: another project in the store records the same path or repository. An export has no destination, and the portable importer resolves the collision before it asks — it maps the artifact's project onto the one this store already has, so a project that collides is ADOPTED, not refused."},

		// ---- MEMORY -------------------------------------------------------
		{name: "memory/empty id", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.ID = "" }))
			}},
		{name: "memory/id shape", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.ID = hostileID }))
			}},
		{name: "memory/id length", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) {
					m.ID = strings.Repeat("a", memory.MaxImportedIDLen+1)
				}))
			}},
		{name: "memory/project_id required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.ProjectID = "" }))
			}},
		{name: "memory/content required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Content = "" }))
			}},
		{name: "memory/category value", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Category = "not-a-category" }))
			}},
		{name: "memory/source value", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Source = "telepathy" }))
			}},
		{name: "memory/content credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Content = "token " + cred }))
			}},
		{name: "memory/source_ref credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.SourceRef = "https://x.invalid/?t=" + cred }))
			}},
		{name: "memory/agent credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Agent = "ops-" + cred }))
			}},
		{name: "memory/session_id credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.SessionID = "ses-" + cred }))
			}},
		{name: "memory/source_ref length", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.SourceRef = longRef }))
			}},
		{name: "memory/agent length", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Agent = longAgent }))
			}},
		{name: "memory/tags credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) { m.Tags = []string{"ops-" + cred} }))
			}},
		{name: "memory/evidence kind", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) {
					m.Evidence = []memory.PortableEvidence{{Kind: "rumour"}}
				}))
			}},
		{name: "memory/evidence agent credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) {
					m.Evidence = []memory.PortableEvidence{{Kind: "observed", Agent: "ops-" + cred}}
				}))
			}},
		{name: "memory/evidence session_id credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) {
					m.Evidence = []memory.PortableEvidence{{Kind: "observed", SessionID: "ses-" + cred}}
				}))
			}},
		{name: "memory/evidence source_ref credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedMemory(withMemory(func(m *memory.PortableMemory) {
					m.Evidence = []memory.PortableEvidence{{Kind: "observed", SourceRef: cred}}
				}))
			}},
		{name: "memory/recorded history in the destination", counterpart: "store",
			why: "a fact about the DESTINATION, and screening on it would be actively wrong: every write appends a memory_history row, so every memory a store exports has one and an export that checked would leave out the whole store. The importer runs it after the presence check, which is the only order in which it means anything — a memory already in the store is a skip."},
		{name: "memory/project not found", counterpart: "store",
			why: "a fact about the DESTINATION, and the export side has the mirror of it: Export leaves a record whose project was left out of the artifact out WITH that project, because the importer resolves a record's project against the artifact. A memory whose project no store holds at all is unreachable in a store — projects.id is the parent of a foreign key with foreign_keys(ON)."},

		// ---- TASK ---------------------------------------------------------
		{name: "task/empty id", counterpart: "predicate",
			importRefuses: func() error { return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.ID = "" })) }},
		{name: "task/id shape", counterpart: "predicate",
			importRefuses: func() error { return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.ID = hostileID })) }},
		{name: "task/project_id required", counterpart: "predicate",
			importRefuses: func() error { return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.ProjectID = "" })) }},
		{name: "task/title required", counterpart: "predicate",
			importRefuses: func() error { return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Title = "" })) }},
		{name: "task/status value", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Status = "sideways" }))
			}},
		{name: "task/priority range", counterpart: "predicate",
			importRefuses: func() error { return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Priority = 99 })) }},
		{name: "task/title credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Title = "token " + cred }))
			}},
		{name: "task/description credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Description = "token " + cred }))
			}},
		{name: "task/notes credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedTask(withTask(func(tk *memory.Task) { tk.Notes = "token " + cred }))
			}},
		{name: "task/blocker not in this store", counterpart: "store",
			why: "a fact about the DESTINATION, and the portable importer resolves it before the store is asked: orderTasks drops a pointer to a task the artifact does not contain, and orders the rest so a blocker exists first. A task that is not blocked is the honest state, since the blocker does not exist to be blocked by."},
		{name: "task/project not found", counterpart: "store",
			why: "as with a memory: the export side's counterpart is the orphan rule, and a task whose project no store holds is unreachable because projects.id is the parent of a foreign key with foreign_keys(ON)."},

		// ---- DECISION -----------------------------------------------------
		{name: "decision/empty id", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.ID = "" }))
			}},
		{name: "decision/id shape", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.ID = hostileID }))
			}},
		{name: "decision/project_id required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.ProjectID = "" }))
			}},
		{name: "decision/title required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Title = "" }))
			}},
		{name: "decision/decision required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Decision = "" }))
			}},
		{name: "decision/rationale required", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Rationale = "" }))
			}},
		{name: "decision/status value", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Status = "maybe" }))
			}},
		{name: "decision/title credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Title = "token " + cred }))
			}},
		{name: "decision/decision credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Decision = "token " + cred }))
			}},
		{name: "decision/rationale credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Rationale = "token " + cred }))
			}},
		{name: "decision/alternatives credential", counterpart: "predicate",
			importRefuses: func() error {
				return memory.CheckImportedDecision(withDecision(func(d *memory.Decision) { d.Alternatives = []string{"token " + cred} }))
			}},
		{name: "decision/superseder not in this store", counterpart: "store",
			why: "a fact about the DESTINATION, and orderDecisions drops a pointer to a decision the artifact does not contain, exactly as orderTasks does for a blocker."},
		{name: "decision/project not found", counterpart: "store",
			why: "as with a memory and a task: the orphan rule is the export-side counterpart, and a dangling project_id is unreachable because projects.id is the parent of a foreign key with foreign_keys(ON)."},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			switch p.counterpart {
			case "predicate":
				if p.why != "" {
					t.Error(`a "predicate" counterpart must not also carry a "store" explanation`)
				}
				// The coverage IS the shared predicate, so assert the predicate
				// refuses. If a refusal is ever moved back into an importer's own
				// body — where the export cannot see it — this fails.
				if err := p.importRefuses(); err == nil {
					t.Errorf("the shared predicate the exporter calls ACCEPTED a record the importer refuses (%s), so the export side has no counterpart for this path", p.name)
				}
				// And that the reason is a usable one: it must name something.
				if err := p.importRefuses(); err != nil && strings.TrimSpace(err.Error()) == "" {
					t.Error("the refusal carries an empty reason, so the report would name a record and say nothing")
				}
				// And that a credential refusal never carries the value, since
				// this reason is what the export report prints.
				if err := p.importRefuses(); err != nil {
					if strings.Contains(err.Error(), cred) {
						t.Errorf("the refusal printed the credential it refused: %v", err)
					}
					if strings.ContainsAny(err.Error(), "\n\r") {
						t.Errorf("the refusal carries a line break, so it forges a report line: %v", err)
					}
				}
			case "store":
				if p.why == "" {
					t.Error(`a "store" counterpart must say why an export cannot screen for it — "no counterpart" is only honest when the reason is written down`)
				}
				if p.importRefuses != nil {
					t.Error(`a "store" path must not assert a predicate refusal; it has none by definition`)
				}
			default:
				t.Fatalf("counterpart = %q, want \"predicate\" or \"store\"", p.counterpart)
			}
		})
	}

	// The table itself has to be exhaustive in the way that matters: every
	// CREDENTIAL-GUARDED FIELD has a row. A field added to a predicate's guard
	// without a row here is one the export report would name a field for without
	// knowing whether it can print a fix — and, before #813, one nothing screened
	// at all. The count is what makes the test fail when a field is added, and it
	// is counted from the predicates rather than transcribed, so it cannot drift
	// from them by being wrong.
	//
	// 17 = a project's name and path (2) + a memory's content, source_ref, agent,
	// session_id and tags (5) + an evidence record's agent, session_id and
	// source_ref (3) + a task's title, description and notes (3) + a decision's
	// title, decision, rationale and alternatives (4).
	if got, want := credentialFields(), 17; got != want {
		t.Errorf("the table covers %d credential-guarded field(s), want %d — a field was added to a predicate's guard, so it needs a row here", got, want)
	}
}

// credentialFields counts the credential-guarded fields across the four
// predicates: a project has 2, a memory has 4 at the row level plus 3 per evidence
// record, a task has 3, a decision has 4 (3 fields plus alternatives). It is
// computed by driving each predicate and reading which field the refusal names, so
// it cannot drift from the predicates by construction.
func credentialFields() int {
	cred := exportedCredential()
	var n int
	guard := func(err error) {
		if err == nil {
			return
		}
		var refused *memory.SecretContentError
		if !errors.As(err, &refused) {
			return
		}
		n++
	}
	guard(memory.CheckImportedProject(memory.PortableProject{ID: "p1", Name: "token " + cred, Path: "/src/p1"}))
	guard(memory.CheckImportedProject(memory.PortableProject{ID: "p1", Name: "n", Path: "/src/" + cred}))
	guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "token " + cred, Source: "mcp"}))
	guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "c", Source: "mcp", SourceRef: cred}))
	guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "c", Source: "mcp", Agent: "ops-" + cred}))
	guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "c", Source: "mcp", SessionID: "ses-" + cred}))
	guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "c", Source: "mcp", Tags: []string{cred}}))
	for _, e := range []memory.PortableEvidence{
		{Kind: "observed", Agent: "ops-" + cred},
		{Kind: "observed", SessionID: "ses-" + cred},
		{Kind: "observed", SourceRef: cred},
	} {
		guard(memory.CheckImportedMemory(memory.PortableMemory{ID: "m1", ProjectID: "p1",
			Category: "gotcha", Content: "c", Source: "mcp", Evidence: []memory.PortableEvidence{e}}))
	}
	guard(memory.CheckImportedTask(memory.Task{ID: "t1", ProjectID: "p1", Title: "token " + cred, Status: "pending"}))
	guard(memory.CheckImportedTask(memory.Task{ID: "t1", ProjectID: "p1", Title: "t", Description: "token " + cred, Status: "pending"}))
	guard(memory.CheckImportedTask(memory.Task{ID: "t1", ProjectID: "p1", Title: "t", Notes: "token " + cred, Status: "pending"}))
	guard(memory.CheckImportedDecision(memory.Decision{ID: "d1", ProjectID: "p1", Title: "token " + cred,
		Decision: "d", Rationale: "r", Status: "active"}))
	guard(memory.CheckImportedDecision(memory.Decision{ID: "d1", ProjectID: "p1", Title: "t",
		Decision: "token " + cred, Rationale: "r", Status: "active"}))
	guard(memory.CheckImportedDecision(memory.Decision{ID: "d1", ProjectID: "p1", Title: "t",
		Decision: "d", Rationale: "token " + cred, Status: "active"}))
	guard(memory.CheckImportedDecision(memory.Decision{ID: "d1", ProjectID: "p1", Title: "t",
		Decision: "d", Rationale: "r", Alternatives: []string{cred}, Status: "active"}))
	return n
}

// TestTheExportReportNeverPrintsACredential is the leak check on the OTHER side of
// the same table, and it is separate because it is a property of the whole report
// rather than of one line: every refused credential in ONE batch, so a renderer
// that leaked on the third case of the table above is not merely unexercised.
func TestTheExportReportNeverPrintsACredential(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	plantExportProject(t, db, "p1", "one", "/src/p1")
	cred := exportedCredential()

	for _, id := range []string{"m-a", "m-b", "m-c"} {
		plantMemory(t, db, id, "p1", "the deploy token is "+cred, `[]`, "mcp")
	}

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	reasons := make([]string, 0, len(stats.Skipped))
	for _, sk := range stats.Skipped {
		reasons = append(reasons, sk.Reason)
	}
	if len(reasons) != 3 {
		t.Fatalf("Skipped holds %d record(s), want 3: %v", len(reasons), stats.Skipped)
	}
	for _, r := range reasons {
		if strings.Contains(r, cred) {
			t.Errorf("the export report printed the credential itself: %q", r)
		}
		// The field is still named, which is what makes the line actionable. The
		// distinction is the point: a refusal that says nothing and a refusal
		// that names the field are both safe, and only the second is useful.
		if !strings.Contains(r, "content") {
			t.Errorf("the credential refusal does not name the field to fix: %q", r)
		}
	}
	// And over the joined report, since a report is built by joining: a
	// token-shaped prefix anywhere is a leak whatever line it is on.
	if strings.Contains(strings.Join(reasons, "\n"), "ghp_") {
		t.Error("the joined report carries a GitHub PAT prefix")
	}
}

// TestExportRefusalCarriesTheImportersOwnSentence pins the mechanism by which the
// two sides cannot drift: the export reason is the IMPORTER's message, not a
// second phrasing of the rule. A second phrasing is what #796 shipped, and the
// documented admission that it could not cover the other refusals is what #813
// exists to remove.
func TestExportRefusalCarriesTheImportersOwnSentence(t *testing.T) {
	refused := memory.CheckImportedMemory(memory.PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "not-a-category", Content: "a body", Source: "mcp",
	})
	if refused == nil {
		t.Fatal("the predicate accepted a memory with an unknown category, so there is nothing to render")
	}
	reason, secret := exportRefusal(refused)
	if secret {
		t.Error("an ordinary field refusal was reported as a credential refusal")
	}
	if !strings.Contains(reason, refused.Error()) {
		t.Errorf("the export reason does not carry the importer's own sentence verbatim:\n  import:  %q\n  export: %q",
			refused.Error(), reason)
	}

	// A credential refusal is the one case with its own sentence, and it must
	// name the field without naming the value.
	cred := exportedCredential()
	secretRefused := memory.CheckImportedMemory(memory.PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "token " + cred, Source: "mcp",
	})
	if secretRefused == nil {
		t.Fatal("the predicate accepted a credential-shaped content, so there is nothing to render")
	}
	reason, secret = exportRefusal(secretRefused)
	if !secret {
		t.Error("a credential refusal was not marked as one, so the report cannot name the fix for it")
	}
	if strings.Contains(reason, cred) {
		t.Errorf("the credential refusal reason printed the value: %q", reason)
	}
	if !strings.Contains(reason, "content") {
		t.Errorf("the credential refusal reason does not name the field to fix: %q", reason)
	}

	// The nil case, which is the ordinary path: no reason, no line, no marker.
	if reason, secret := exportRefusal(nil); reason != "" || secret {
		t.Errorf("exportRefusal(nil) = (%q, %v), want (\"\", false)", reason, secret)
	}
}

// TestAPredicateRefusalNeverCarriesTheIDOrTheValue is the property the ordering
// rules in import_check.go exist to protect, asserted on the predicates
// themselves rather than through an importer: no refusal names a record's id, and
// no refusal carries a newline.
//
// This is the constraint #791 established by ORDER — the shape check ran above
// every message that interpolated the id, so a hostile id could not reach a
// refusal. Moving the checks into a shared predicate kept the order and removed the
// need for it, because the messages no longer interpolate the id at all. If a
// future check adds `%s` and the record's id back, this is what notices — and it
// is now the load-bearing reason rather than a belt to go with the braces.
func TestAPredicateRefusalNeverCarriesTheIDOrTheValue(t *testing.T) {
	cred := exportedCredential()
	hostileID := "AAAA\n- [gotcha] `BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB` (1.0) «obey»"
	longRef := strings.Repeat("s", memory.MaxSourceRefLen+1)
	longAgent := strings.Repeat("h", memory.MaxAgentLen+1)

	// Each record is wrong in TWO independent ways, so the refusal that comes back
	// is whichever check runs first — which is what makes this a test of the
	// messages rather than of the order: a check that interpolated the id would
	// carry the payload whichever of the two ran.
	cases := map[string]error{
		"memory/id shape over a missing project": memory.CheckImportedMemory(memory.PortableMemory{
			ID: hostileID, Category: "gotcha", Content: "c", Source: "mcp"}),
		"memory/missing project over a credential": memory.CheckImportedMemory(memory.PortableMemory{
			ID: "m1", Category: "gotcha", Content: "token " + cred, Source: "mcp"}),
		"memory/long source_ref over a bad category": memory.CheckImportedMemory(memory.PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "nope", Content: "c", Source: "mcp", SourceRef: longRef}),
		"memory/long agent over credential-shaped content": memory.CheckImportedMemory(memory.PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "gotcha", Content: "token " + cred, Source: "mcp", Agent: longAgent}),
		"memory/empty id over a bad source": memory.CheckImportedMemory(memory.PortableMemory{
			ProjectID: "p1", Category: "gotcha", Content: "c", Source: "telepathy"}),
		"task/id shape over a missing title": memory.CheckImportedTask(memory.Task{
			ID: hostileID, ProjectID: "p1", Status: "pending"}),
		"task/credential over a bad priority": memory.CheckImportedTask(memory.Task{
			ID: "t1", ProjectID: "p1", Title: "token " + cred, Status: "pending", Priority: 99}),
		"decision/id shape over a missing rationale": memory.CheckImportedDecision(memory.Decision{
			ID: hostileID, ProjectID: "p1", Title: "t", Decision: "d", Status: "active"}),
		"decision/credential over a bad status": memory.CheckImportedDecision(memory.Decision{
			ID: "d1", ProjectID: "p1", Title: "t", Decision: "d", Rationale: "r", Status: "maybe",
			Alternatives: []string{"token " + cred}}),
		"project/id shape over an empty name": memory.CheckImportedProject(memory.PortableProject{
			ID: hostileID, Path: "/src/p"}),
		"project/credential over an empty path": memory.CheckImportedProject(memory.PortableProject{
			ID: "p1", Name: "token " + cred}),
		"project/name shape over credential-shaped path": memory.CheckImportedProject(memory.PortableProject{
			ID: "p1", Name: "a\nname", Path: "/src/" + cred}),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("the predicate accepted a record it should refuse")
			}
			msg := err.Error()
			if strings.Contains(msg, hostileID) {
				t.Errorf("the refusal echoes the hostile id back, which is the value it refused:\n%v", err)
			}
			if strings.Contains(msg, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") {
				t.Errorf("the refusal carries the payload it refused:\n%v", err)
			}
			if strings.Contains(msg, cred) {
				t.Errorf("the refusal carries the credential it refused:\n%v", err)
			}
			// A line break in the message forges a report line, and the report
			// prints this string verbatim on both sides — the import's rejected
			// list and the export's `!` line.
			if strings.ContainsAny(msg, "\n\r") {
				t.Errorf("the refusal carries a line break, so it forges a report line:\n%v", err)
			}
		})
	}
}

// TestAPredicateRefusalNamesAFieldAndNeverTheRecord is the direct statement of the
// invariant, and it is separate from the hostile-id test above for a reason that
// took a mutation to find.
//
// The hostile-id cases cannot catch a field message that interpolates the id. Their
// records are wrong in two ways and the SHAPE check runs first, so it is always the
// shape message that comes back and the id never reaches a field message — which
// means adding `%s` to every field message in a predicate changes nothing those
// cases can see. The order is what hides it, and the order is load-bearing for a
// different reason.
//
// So these cases use an id this build ACCEPTS and which is distinctive enough to
// find in a message — `would-be-quoted-1` and friends. A refusal naming that id is
// a refusal that put a record's identity into a string the export report prints and
// the import report embeds, and the id is then quoted by nothing: `assemble.Token`
// only renders ids the report CHOOSES to print, so an id interpolated into a reason
// is the unquoted kind, which is the class #791 was about. One per record type, on
// the field check each type puts after the shape check, is enough to fail the moment
// anyone adds the id back.
func TestAPredicateRefusalNamesAFieldAndNeverTheRecord(t *testing.T) {
	cases := map[string]struct {
		id  string
		err error
	}{
		"memory": {
			id:  "would-be-quoted-1",
			err: memory.CheckImportedMemory(memory.PortableMemory{ID: "would-be-quoted-1", Category: "gotcha", Content: "c", Source: "mcp"}),
		},
		// Each broken in the FIRST field check its type runs after the shape
		// check, and with nothing else wrong, so the message that comes back is
		// unambiguously a field message.
		"task": {
			id:  "would-be-quoted-2",
			err: memory.CheckImportedTask(memory.Task{ID: "would-be-quoted-2", Title: "t", Status: "pending"}),
		},
		"decision": {
			id: "would-be-quoted-3",
			err: memory.CheckImportedDecision(memory.Decision{ID: "would-be-quoted-3",
				Title: "t", Decision: "d", Rationale: "r", Status: "active"}),
		},
		"project": {
			id:  "would-be-quoted-4",
			err: memory.CheckImportedProject(memory.PortableProject{ID: "would-be-quoted-4", Name: "", Path: "/src/p"}),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("the predicate accepted a record it should refuse")
			}
			// The field IS named, so the line is actionable — that is what the
			// export report depends on, and it is the other half of this change
			// from naming the record.
			if !strings.Contains(tc.err.Error(), "is required") {
				t.Errorf("the refusal does not name the field to fix: %v", tc.err)
			}
			if strings.Contains(tc.err.Error(), tc.id) {
				t.Errorf("the refusal names the record (%s), so the report would print an identity through no quoting of its own: %v",
					tc.id, tc.err)
			}
		})
	}
}

// plantRaw runs one statement with its args, for the cases that need more than a
// single INSERT of a well-formed row.
func plantRaw(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("plant: %v", err)
	}
}

// plantMemory inserts a memory with the given id, project, content, tags JSON and
// source — in SQL, because a store call cannot create the states under test.
func plantMemory(t *testing.T, db *sql.DB, id, projectID, content, tagsJSON, source string) {
	t.Helper()
	plantRaw(t, db, `INSERT INTO memories
		(id, project_id, category, content, source, tags, created_at, updated_at)
		VALUES (?, ?, 'gotcha', ?, ?, ?, datetime('now'), datetime('now'))`,
		id, projectID, content, source, tagsJSON)
}

// plantTask inserts a task with the given id, project, title, status and priority.
func plantTask(t *testing.T, db *sql.DB, id, projectID, title, status, priority string) {
	t.Helper()
	plantRaw(t, db, `INSERT INTO tasks
		(id, project_id, title, description, status, priority, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, datetime('now'), datetime('now'))`,
		id, projectID, title, status, priority)
}

// plantDecision inserts a decision with the given id, project, title, decision,
// rationale and status.
func plantDecision(t *testing.T, db *sql.DB, id, projectID, title, decision, rationale, status string) {
	t.Helper()
	plantRaw(t, db, `INSERT INTO decisions
		(id, project_id, title, decision, rationale, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		id, projectID, title, decision, rationale, status)
}
