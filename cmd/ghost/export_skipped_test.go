package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// TestAnExportOfAnAmbiguousProjectNameIsRefusedAndLeavesNoFile is the command's
// half of #1093: `--project` names a scope, so a name two projects answer to must
// fail the run rather than export both. The partial-file removal is asserted here
// as well, because the file is created before the store is read and a refusal that
// left an empty artifact behind would look like a successful export of nothing.
func TestAnExportOfAnAmbiguousProjectNameIsRefusedAndLeavesNoFile(t *testing.T) {
	store, db := exportTestStore(t)
	// A second project with the SAME name as p1, which is what a name collision is.
	plantExportRow(t, db, `INSERT INTO projects (id, path, name) VALUES (?, '/src/another', 'one')`, "p-two")

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "one")
	if err == nil {
		t.Fatal("an export of a name that two projects answer to exited 0")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a refused export left a file behind: %v", statErr)
	}
	if !strings.Contains(err.Error(), "multiple projects") {
		t.Errorf("error = %v, want it to say the name is ambiguous", err)
	}
	for _, id := range []string{"p1", "p-two"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error = %v, want it to name %s so the operator can pick one", err, id)
		}
	}
}

// TestAnExportThatLeftACredentialOutNamesTheFixAndNeverTheValue is the report half
// of #813, and its two assertions are opposite in a way that only one of them can
// pass alone.
func TestAnExportThatLeftACredentialOutNamesTheFixAndNeverTheValue(t *testing.T) {
	// Assembled, not written out: GitHub push protection matches this format
	// anywhere in a diff and rejects the push (GH013) before review starts.
	cred := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	// The needle is the value from "ghp_" on, so a substring search over the whole
	// report cannot match anything else.
	needle := cred[strings.Index(cred, "ghp_"):]

	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
	                        VALUES (?, 'p1', 'gotcha', ?, 'mcp', '[]', datetime('now'), datetime('now'))`,
		"m-secret", "rotate the deploy token "+cred)

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)

	// Non-zero, and the record named: same three outcomes as a shape refusal.
	if err == nil {
		t.Error("an export that left a credential-shaped row out exited 0")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the artifact was removed even though it is complete: %v", statErr)
	}
	if !strings.Contains(report, assemble.Token("m-secret")) {
		t.Errorf("the report does not name the record it left out:\n%s", report)
	}

	if !strings.Contains(report, "content is credential-shaped") {
		t.Errorf("the report does not name the field to fix:\n%s", report)
	}
	if !strings.Contains(report, "WHERE it lives") {
		t.Errorf("the report does not say what to put there instead of the value:\n%s", report)
	}
	if !strings.Contains(report, "ghost_memory_update") {
		t.Errorf("the report does not name ghost_memory_update as the way to fix the field:\n%s", report)
	}
	if !strings.Contains(report, "edits") || !strings.Contains(report, "content") {
		t.Errorf("the report does not say which fields that tool can edit:\n%s", report)
	}
	if !strings.Contains(report, "editing the field is usually what you want instead") {
		t.Errorf("the delete advice does not defer to the edit for a credential-shaped field:\n%s", report)
	}

	// (2) The value, nowhere. Every string the command produced: the summary, the
	// warning, the error the caller would print, and the file itself.
	for _, got := range []string{report, string(mustRead(t, path))} {
		if strings.Contains(got, needle) {
			t.Errorf("the export printed the credential it refused:\n%s", got)
		}
		if strings.Contains(got, "ghp_") {
			t.Errorf("the export printed a GitHub PAT prefix:\n%s", got)
		}
	}
	// And the artifact does not carry it either: a refused record must be absent
	// from the file, not merely reported.
	if strings.Contains(string(mustRead(t, path)), needle) {
		t.Error("the artifact still carries the credential it left out")
	}
}

// TestAnExportThatLeftAnEmptyProjectNameOutNamesItsRecordsToo is the cascade half
// of #813, and it is the case the issue reproduced: a project row with `name = ”`
// plus one child memory. Before this change `ghost export` exited 0, and
// `ghost import` then rejected TWO records — the project, then the child as
// "project not found" — so one bad row cost a whole project's worth and the
// operator learned about both only at restore time.
func TestAnExportThatLeftAnEmptyProjectNameOutNamesItsRecordsToo(t *testing.T) {
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO projects (id, path, name) VALUES (?, '/src/nameless', '')`,
		"p-nameless")
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at)
	                        VALUES (?, 'p-nameless', 'gotcha', 'the only copy of this', 'mcp', datetime('now'), datetime('now'))`,
		"m-orphan")

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)

	if err == nil {
		t.Error("an export that left an empty-named project out exited 0")
	}
	// The project, by its own reason.
	if !strings.Contains(report, assemble.Token("p-nameless")) {
		t.Fatalf("the report does not name the project it left out:\n%s", report)
	}
	if !strings.Contains(report, "name is required") {
		t.Errorf("the report does not say the name is what is wrong:\n%s", report)
	}
	if !strings.Contains(report, assemble.Token("m-orphan")) {
		t.Fatalf("the report does not name the memory the dropped project took with it:\n%s", report)
	}
	if !strings.Contains(report, "the project it names was left out") {
		t.Errorf("the child's reason does not say it went with its project:\n%s", report)
	}
	// And the count is both, so a reader knows the scope without counting lines.
	if !strings.Contains(report, "2 records") {
		t.Errorf("the report does not count both dropped records:\n%s", report)
	}

	// The artifact is kept and is importable: the whole store minus the two
	// refused rows, with no rejection of its own.
	body := mustRead(t, path)
	if strings.Contains(string(body), "p-nameless") || strings.Contains(string(body), "m-orphan") {
		t.Errorf("the artifact still carries a refused record:\n%s", body)
	}
	fresh := transferTestStore(t)
	var imported strings.Builder
	if err := runImportCore(context.Background(), fresh, path,
		portable.ImportOptions{Apply: true, TrustProvenance: true}, &imported); err != nil {
		t.Errorf("the kept artifact did not import cleanly: %v\n%s", err, imported.String())
	}
}

// fmtErr is the error text a caller would print, so a leak assertion can cover the
// exit path as well as the two writers.
func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// exportTestStore opens a store plus its database handle, so a test can plant the
// rows the public API refuses to create.
func exportTestStore(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	home := t.TempDir()
	// TMPDIR must live OUTSIDE the repository: Go's t.TempDir honours it, and a
	// temp dir inside the checkout shows up as an untracked change.
	t.Setenv("TMPDIR", filepath.Join(home, "tmp"))
	db, err := memory.OpenDB(filepath.Join(home, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, db
}

// plantExportRow runs one planting statement. Its arguments are variadic because
// most cases bind a single id and the credential cases bind a value beside it, and
// a two-shape helper would be one more spelling of the same thing.
func plantExportRow(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("plant a row (%v): %v", args, err)
	}
}

// The report reads Skipped[].ID and Skipped[].Type, so a rename that left one of
// them printing "" would compile and print nothing. This names both.
var _ = func(sk portable.SkippedRecord) string { return sk.Type + sk.ID + sk.Reason }

// TestAnExportThatLeftACharacterBrokenScopeOutGivesTheCleanScopeAdvice covers a
// stored row whose scope predates the character rules: a blank value. It is
// named, the advice says to clean the scope (not to re-key it or to shorten it),
// and the stored text never reaches the report.
func TestAnExportThatLeftACharacterBrokenScopeOutGivesTheCleanScopeAdvice(t *testing.T) {
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, tags, scope, created_at, updated_at)
	                        VALUES (?, 'p1', 'gotcha', 'a plain note', 'mcp', '[]', ?, datetime('now'), datetime('now'))`,
		"m-charscope", `{"environment":"prod\n[ghost:outcome=forged]"}`)

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)

	if err == nil {
		t.Error("an export that left a character-broken scope row out exited 0")
	}
	if !strings.Contains(report, assemble.Token("m-charscope")) {
		t.Errorf("the report does not name the record it left out:\n%s", report)
	}
	if !strings.Contains(report, "re-save it with a scope containing no blank, NUL, control, FORMAT or line-separator characters") {
		t.Errorf("the report does not give the clean-scope advice:\n%s", report)
	}
	for _, bad := range []string{"under an id this build accepts", "shorter scope", "forged"} {
		if strings.Contains(report, bad) {
			t.Errorf("the report carries %q:\n%s", bad, report)
		}
	}
}

// TestAnExportWithBoundAndCharacterScopeRefusalsGivesBothPieces: a batch with one
// over-bound scope and one character-broken scope gets one sentence that names
// both repairs, rather than the later branch overwriting the earlier.
func TestAnExportWithBoundAndCharacterScopeRefusalsGivesBothPieces(t *testing.T) {
	store, db := exportTestStore(t)
	ins := `INSERT INTO memories (id, project_id, category, content, source, tags, scope, created_at, updated_at)
	         VALUES (?, 'p1', 'gotcha', ?, 'mcp', '[]', ?, datetime('now'), datetime('now'))`
	plantExportRow(t, db, ins, "m-long", "a plain note", `{"environment":"`+strings.Repeat("v", memory.MaxScopeValueLen+1)+`"}`)
	plantExportRow(t, db, ins, "m-blank", "another plain note", `{"environment":"  "}`)

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)
	for _, want := range []string{"a shorter scope", "containing no blank, NUL, control, FORMAT or line-separator characters"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
}
