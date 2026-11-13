package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// historyCompactTestStore is a store with two projects and one memory each, so
// a per-project report has something to be per.
func historyCompactTestStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	for _, p := range []struct{ id, path, name string }{
		{id: "alpha", path: "/tmp/alpha", name: "Alpha"},
		{id: "beta", path: "/tmp/beta", name: "Beta"},
	} {
		if err := s.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p.id, err)
		}
		if _, err := s.Create(ctx, p.id, memory.Memory{
			Category: "gotcha", Content: "a note in " + p.id, Source: "mcp", Importance: 0.5,
		}); err != nil {
			t.Fatalf("Create(%s): %v", p.id, err)
		}
	}
	return s
}

// TestParseHistoryCompactArgs: the flags are three and each changes what the
// command does, so a mistyped one is refused rather than ignored. The default is
// the one that writes nothing, which is why the parser's zero value has to mean
// it.
func TestParseHistoryCompactArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    historyCompactOptions
		wantErr bool
	}{
		{name: "no flags is a dry run over every project", args: nil},
		{name: "--apply writes", args: []string{"--apply"}, want: historyCompactOptions{Apply: true}},
		{name: "--fix-updated-at asks for the stamp repair", args: []string{"--fix-updated-at"},
			want: historyCompactOptions{FixUpdatedAt: true}},
		{name: "both", args: []string{"--apply", "--fix-updated-at"},
			want: historyCompactOptions{Apply: true, FixUpdatedAt: true}},
		{name: "--project scopes the report", args: []string{"--project", "alpha"},
			want: historyCompactOptions{Project: "alpha"}},
		{name: "--project= form", args: []string{"--project=alpha"}, want: historyCompactOptions{Project: "alpha"}},
		{name: "a project named like a flag still needs its own value", args: []string{"--project", "--apply"},
			want: historyCompactOptions{Project: "--apply"}},
		{name: "--before bounds the repair", args: []string{"--before", "2026-09-01"},
			want: historyCompactOptions{Before: "2026-09-01"}},
		{name: "--before= form", args: []string{"--before=2026-09-01T00:00:00Z"},
			want: historyCompactOptions{Before: "2026-09-01T00:00:00Z"}},
		{name: "a bound this command cannot read", args: []string{"--before", "sometime"}, wantErr: true},
		{name: "--before with no value", args: []string{"--before"}, wantErr: true},
		{name: "--before= with no value", args: []string{"--before="}, wantErr: true},
		{name: "an unknown flag", args: []string{"--fixit"}, wantErr: true},
		{name: "a second operand", args: []string{"alpha", "beta"}, wantErr: true},
		{name: "--limit has nothing to limit here", args: []string{"--limit", "2"}, wantErr: true},
		{name: "--json has nothing to print here", args: []string{"--json"}, wantErr: true},
		{name: "purge and compact are different requests", args: []string{"purge"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHistoryCompactArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseHistoryCompactArgs(%v) = %+v, want an error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHistoryCompactArgs(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseHistoryCompactArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHistoryCompactRoutedAheadOfTheMemoryID: `compact` and a memory id occupy
// the same position after `ghost history`, and the routing decides which one a
// reader typed. A memory id is 32 hex characters, so nothing is ambiguous — but
// the decision has to be made, and a word the routing misses would be read as an
// id and answered with "no memory and no history recorded for compact", which is
// a confident and wrong answer.
func TestHistoryCompactRoutedAheadOfTheMemoryID(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{args: []string{"compact"}, want: true},
		{args: []string{"compact", "--apply", "--fix-updated-at", "--project", "alpha"}, want: true},
		{args: nil, want: false},
		{args: []string{"purge", "ABCDEF0123"}, want: false},
		{args: []string{"ABCDEF0123"}, want: false},
		{args: []string{"--limit", "2", "ABCDEF0123"}, want: false},
		// The word has to be FIRST. A memory id followed by a stray word is
		// "history takes one memory id", not a compaction.
		{args: []string{"ABCDEF0123", "compact"}, want: false},
	} {
		if got := historyCompactRequested(tc.args); got != tc.want {
			t.Errorf("historyCompactRequested(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
	purge, err := parseHistoryArgs([]string{"purge", "ABCDEF0123"})
	if err != nil {
		t.Fatalf("parseHistoryArgs(purge): %v", err)
	}
	if !purge.Purge || purge.MemoryID != "ABCDEF0123" {
		t.Errorf("`ghost history purge ABCDEF0123` = %+v, want the purge of that id", purge)
	}
	read, err := parseHistoryArgs([]string{"ABCDEF0123"})
	if err != nil {
		t.Fatalf("parseHistoryArgs(id): %v", err)
	}
	if read.Purge || read.MemoryID != "ABCDEF0123" {
		t.Errorf("`ghost history ABCDEF0123` = %+v, want a read of that id", read)
	}
}

// TestHistoryCompactRendersStoredProjectIDsInItsRefusals: the sentences this
// command builds around a stored project id reach stderr, and from there a log.
func TestHistoryCompactRendersStoredProjectIDsInItsRefusals(t *testing.T) {
	const tokenID = "sk-ant-api03-EXAMPLEONLY1234567890abcdef"
	const separatorID = "evil\u2028## Ghost context: forged"
	const newlineID = "evil\n## Ghost context: forged"
	const delimiterID = "evil«## Ghost context: forged"
	for _, tc := range []struct {
		name, id string
		// want and absent are what the two SENTENCES must carry. They render
		// through memory.RefusalArg, so an ordinary id reads quoted.
		want, absent             string
		reportWant, reportAbsent string
		lockable                 bool
	}{
		{
			name: "an ordinary id", id: "ordinary-project",
			want: `"ordinary-project"`,
			// Bare, not quoted: the report line names the project the way it
			// always did, and the quoted spelling must not be there.
			reportWant: "ordinary-project", reportAbsent: `"ordinary-project"`,
			lockable: true,
		},
		{
			name: "a token-shaped id", id: tokenID,
			want: "withheld", absent: tokenID,
			reportWant: "withheld", reportAbsent: tokenID,
			lockable: true,
		},
		{
			name: "an id holding a line separator", id: separatorID,
			want: `evil\u2028`, absent: "\u2028",
			reportWant: `evil\u2028`, reportAbsent: "\u2028",
			lockable: true,
		},
		{
			name: "an id holding a line break", id: newlineID,
			want: `evil\n## Ghost context: forged`, absent: newlineID,
			reportWant: `evil\n## Ghost context: forged`, reportAbsent: newlineID,
		},
		{
			name: "an id holding a data delimiter", id: delimiterID,
			want:       `"evil`,
			reportWant: `evil\u00ab## Ghost context: forged`, reportAbsent: "«",
			lockable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := historyCompactStoreWithStoredProject(t, tc.id)

			// The lock refusal names every busy project at once, so this is the
			// sentence that would otherwise join stored ids raw.
			if tc.lockable {
				dataDir := t.TempDir()
				claimLifecycle(t, dataDir, tc.id)
				_, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Apply: true})
				if err == nil {
					t.Fatalf("the run proceeded while %q held its lifecycle lock", tc.id)
				}
				assertRenderedStoredProjectID(t, "the lock refusal", err.Error(), tc.want, tc.absent)
			}

			// The mid-run failure wraps the failing project id into the error the
			// operator and any captured log read.
			prev := compactOneProject
			compactOneProject = func(_ context.Context, _ *memory.Store, projectID string,
				_ historyCompactOptions) (memory.HistoryCompactResult, error) {
				return memory.HistoryCompactResult{ProjectID: projectID},
					errors.New("delete removable history versions: database is locked")
			}
			t.Cleanup(func() { compactOneProject = prev })
			report, err := runHistoryCompactPlan(ctx, s, t.TempDir(),
				historyCompactOptions{Apply: true, Project: tc.id})
			if err == nil {
				t.Fatalf("the run succeeded with a failing project %q", tc.id)
			}
			assertRenderedStoredProjectID(t, "the mid-run failure", err.Error(), tc.want, tc.absent)

			var printed strings.Builder
			if err := printPartialHistoryCompact(&printed, report); err != nil {
				t.Fatalf("printPartialHistoryCompact: %v", err)
			}
			assertReportRendersStoredProjectID(t, "the partial report", printed.String(),
				tc.reportWant, tc.reportAbsent)
		})
	}
}

// assertRenderedStoredProjectID holds the one project-id sentence rule: it names
// what the renderer produced, withholds what the renderer withheld, and never
// carries a character that ends the line.
func assertRenderedStoredProjectID(t *testing.T, surface, sentence, want, absent string) {
	t.Helper()
	if !strings.Contains(sentence, want) {
		t.Errorf("%s does not render the stored project id through the guard (want %q): %s",
			surface, want, sentence)
	}
	if absent != "" && strings.Contains(sentence, absent) {
		t.Errorf("%s echoes %q from the stored project id: %s", surface, absent, sentence)
	}
	if strings.ContainsAny(sentence, "\n\r\u2028\u2029") {
		t.Errorf("%s splits its sentence over more than one line: %q", surface, sentence)
	}
}

// assertReportRendersStoredProjectID holds the report line's half of the same
// rule. A report is several lines by nature, so what is checked is that the
// stored id reached it through the guard and the line renderer, that nothing
// the guard withholds is in it, and that no character a stored id carries — a
// line break, a line separator or a data delimiter — made it onto the page:
// those are the ways a stored id would otherwise become the report's text
// rather than the report's subject.
func assertReportRendersStoredProjectID(t *testing.T, surface, report, want, absent string) {
	t.Helper()
	if !strings.Contains(report, want) {
		t.Errorf("%s does not render the stored project id through the guard (want %q):\n%s",
			surface, want, report)
	}
	if absent != "" && strings.Contains(report, absent) {
		t.Errorf("%s echoes %q from the stored project id:\n%s", surface, absent, report)
	}
	for _, r := range []string{"\u2028", "\u2029", "«", "»"} {
		if strings.Contains(report, r) {
			t.Errorf("%s prints %q from the stored project id raw:\n%q", surface, r, report)
		}
	}
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "## Ghost context") {
			t.Errorf("%s prints a forged heading from the stored project id:\n%s", surface, report)
			return
		}
	}
}

// historyCompactStoreWithStoredProject holds one project whose id the current
// write boundaries would refuse. The row is written directly because that is how
// such an id is in a store at all: import accepted it before the shape check
// existed, or a restored snapshot carried it. The test is about what the command
// prints for a row it can be handed, not about how the row got there.
func historyCompactStoreWithStoredProject(t *testing.T, id string) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		id, "path-for-"+id, "Hostile"); err != nil {
		t.Fatalf("insert stored project %q: %v", id, err)
	}
	return memory.NewStore(db, nil)
}

// TestHistoryCompactReportsWhatAFailedRunAlreadyCompacted: the only path this
// command can destroy part of a store, and the one report whose job is to say how
// much of the store a run touched.
func TestHistoryCompactReportsWhatAFailedRunAlreadyCompacted(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()

	prev := compactOneProject
	var calls []string
	compactOneProject = func(_ context.Context, _ *memory.Store, projectID string,
		opts historyCompactOptions) (memory.HistoryCompactResult, error) {
		calls = append(calls, projectID)
		if projectID == "beta" {
			return memory.HistoryCompactResult{ProjectID: projectID, Removed: 5, UpdatedAt: 1},
				errors.New("database is locked")
		}
		return memory.HistoryCompactResult{ProjectID: projectID, Removed: 7, UpdatedAt: 3}, nil
	}
	t.Cleanup(func() { compactOneProject = prev })

	report, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Apply: true})
	if err == nil {
		t.Fatalf("the run succeeded with a failing project: %+v", report)
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("error %q does not name the project that failed", err)
	}
	// Both projects are in the report: the first finished, the second did not, and
	// each carries the batches it committed.
	if len(report.Projects) != 2 {
		t.Fatalf("report = %+v, want both projects — the one that finished and the one that "+
			"committed batches before it failed", report.Projects)
	}
	if report.Projects[0].ProjectID != "alpha" || report.Projects[0].Removed != 7 {
		t.Errorf("alpha = %+v, want the finished project's own counts", report.Projects[0])
	}
	if report.Projects[1].ProjectID != "beta" || report.Projects[1].Removed != 5 {
		t.Errorf("beta = %+v, want the batches it committed before failing", report.Projects[1])
	}
	if report.FailedProject != "beta" {
		t.Errorf("the report's failed project = %q, want beta: a line that cannot be read as "+
			"a finished repair is the only thing that keeps these counts from being a claim", report.FailedProject)
	}
	if calls[0] != "alpha" {
		t.Errorf("the first project compacted was %q, want alpha (the store's own order)", calls[0])
	}

	var out strings.Builder
	if err := printPartialHistoryCompact(&out, report); err != nil {
		t.Fatalf("printPartialHistoryCompact: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "7") {
		t.Errorf("the partial report does not carry what the run already did:\n%s", text)
	}
	if !strings.Contains(text, "stopped partway") {
		t.Errorf("the partial report has no header saying the run stopped:\n%s", text)
	}
	if !strings.Contains(text, "beta") {
		t.Errorf("the partial report drops the project that failed, whose committed batches are "+
			"the ones an operator most needs to know about:\n%s", text)
	}
	if !strings.Contains(text, "stopped partway here") {
		t.Errorf("the partial report does not mark which project it stopped in:\n%s", text)
	}
	// And the marker is on that project's line, not on the finished one: a marker on
	// every line tells the operator nothing.
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		marked := strings.Contains(line, "stopped partway here")
		if strings.Contains(line, "beta") && !marked {
			t.Errorf("beta's line is not marked as where the run stopped:\n%s", text)
		}
		if strings.Contains(line, "alpha") && marked {
			t.Errorf("alpha's line is marked as where the run stopped, and it finished:\n%s", text)
		}
	}

	empty := strings.Builder{}
	if err := printPartialHistoryCompact(&empty, historyCompactReport{Apply: true}); err != nil {
		t.Fatalf("printPartialHistoryCompact on an empty report: %v", err)
	}
	if empty.String() != "" {
		t.Errorf("an untouched run printed a partial report: %q", empty.String())
	}
}

// TestHistoryCompactRefusesACutItCannotRead: the bound decides what is deleted, so
// a --before the command cannot read is refused before any project is opened. The
// alternative — handing it down and letting the store refuse it — would fail on the
// first project of a twenty-project store, having already told the operator the
// store was compactable.
func TestHistoryCompactRefusesACutItCannotRead(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)

	var reached []string
	prev := compactOneProject
	compactOneProject = func(_ context.Context, _ *memory.Store, projectID string,
		_ historyCompactOptions) (memory.HistoryCompactResult, error) {
		reached = append(reached, projectID)
		return memory.HistoryCompactResult{ProjectID: projectID}, nil
	}
	t.Cleanup(func() { compactOneProject = prev })

	report, err := runHistoryCompactPlan(ctx, s, t.TempDir(),
		historyCompactOptions{Apply: true, Before: "the day before the fix"})
	if err == nil {
		t.Fatalf("a run with an unreadable --before succeeded: %+v", report)
	}
	if !strings.Contains(err.Error(), "day before") {
		t.Errorf("the refusal %q does not quote the bound it could not read", err)
	}
	if len(reached) != 0 {
		t.Errorf("the run reached %v after refusing its bound", reached)
	}
	if len(report.Projects) != 0 {
		t.Errorf("the refusal still reported %d project result(s)", len(report.Projects))
	}
}

// claimLifecycle writes a live lifecycle claim for projectID, which is what the
// coordinator's own lock leaves behind while a pass runs.
func claimLifecycle(t *testing.T, dataDir, projectID string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	path := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write lifecycle claim: %v", err)
	}
}

// TestHistoryCompactNamesTheStampsItCouldNotRestore: the report has to say which
// rows it left wrong, and it has to say the two different reasons separately.
func TestHistoryCompactNamesTheStampsItCouldNotRestore(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "compact.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "alpha", "/tmp/alpha", "Alpha"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.Create(ctx, "alpha", memory.Memory{
		Category: "gotcha", Content: "a pre-v17 memory resolved before this build recorded it",
		Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM memory_history WHERE memory_id = ?`, id); err != nil {
		t.Fatalf("drop the recorded history: %v", err)
	}
	if _, err := db.Exec(`UPDATE memories SET resolved_at = '2025-12-01 09:00:00' WHERE id = ?`, id); err != nil {
		t.Fatalf("resolve it: %v", err)
	}
	if n, err := s.ClearResolved(ctx, "alpha", []string{id}); err != nil || n != 1 {
		t.Fatalf("ClearResolved: %d rows (err %v), want 1", n, err)
	}
	for range 3 {
		if _, err := db.Exec(
			`INSERT INTO memory_history (memory_id, project_id, recorded_at, phase, content, category, importance, source)
			 SELECT id, project_id, '2026-08-01 09:00:00', 'reflect', content, category, importance, source
			 FROM memories WHERE id = ?`, id); err != nil {
			t.Fatalf("stage a no-op reflect: %v", err)
		}
	}
	if _, err := db.Exec(`UPDATE memories SET updated_at = '2026-08-01 09:30:00' WHERE id = ?`, id); err != nil {
		t.Fatalf("move the stamp: %v", err)
	}

	report, err := runHistoryCompactPlan(ctx, s, t.TempDir(), historyCompactOptions{FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("runHistoryCompactPlan: %v", err)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("report covers %d project(s), want 1: %+v", len(report.Projects), report.Projects)
	}
	got := report.Projects[0]
	if got.StampsUnrecorded != 1 {
		t.Errorf("StampsUnrecorded = %d, want 1: the memory's only version was written by a writer that "+
			"moves no stamp", got.StampsUnrecorded)
	}
	if got.StampsUnreadable != 0 {
		t.Errorf("StampsUnreadable = %d, want 0: nothing here is unreadable", got.StampsUnreadable)
	}
	if got.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: there is no instant to restore this memory's stamp to",
			got.UpdatedAt)
	}
	if got.Removed != 2 {
		t.Errorf("Removed = %d, want 2: the flood is real damage and does not depend on the stamp",
			got.Removed)
	}
	var out strings.Builder
	if err := printHistoryCompact(&out, report); err != nil {
		t.Fatalf("printHistoryCompact: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "1 stamp(s) not restorable, no recorded stamp write") {
		t.Errorf("the report does not disclose the stamp it could not restore:\n%s", text)
	}
	// The dry-run footer says nothing was moved, and that has to stay true — the
	// disclosure is about the apply, not about this run.
	if !strings.Contains(text, "Nothing removed and no updated_at moved") {
		t.Errorf("the disclosure turned a dry run into something that looks like it wrote:\n%s", text)
	}
}

// TestHistoryCompactResolvesAndRefusesAnUnknownProject: --project takes the same
// identifiers every other command's --project does, and one that names nothing is
// an error rather than an empty report — "no such project" and "nothing to
// compact" are different answers and only one of them is a repair.
func TestHistoryCompactResolvesAndRefusesAnUnknownProject(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()

	// By NAME, not only by id: the flag is documented to take what --project
	// takes everywhere else.
	report, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Project: "Alpha"})
	if err != nil {
		t.Fatalf("runHistoryCompact by name: %v", err)
	}
	if len(report.Projects) != 1 || report.Projects[0].ProjectID != "alpha" {
		t.Errorf("a project named by NAME resolved to %+v, want alpha", report.Projects)
	}

	if _, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Project: "nope"}); err == nil {
		t.Error("an unknown project compacted the whole store instead of refusing")
	}
}

// TestPrintHistoryCompactWarnings: the warnings ride the report, so a caller that
// has the report can print them — and printing them in the wrong place is the
// failure worth pinning, because a warning interleaved with the per-project lines
// is one a reader scrolls past on the way to the number they asked for.
func TestPrintHistoryCompactWarnings(t *testing.T) {
	var out strings.Builder
	report := historyCompactReport{
		Apply:  true,
		Before: "2026-10-01 00:00:00",
		Warnings: []string{
			"warning: --before 2026-10-01 00:00:00 reaches past 2026-09-28 17:14:07",
		},
		Projects: []memory.HistoryCompactResult{{ProjectID: "alpha", Removed: 3}},
	}
	if err := printHistoryCompact(&out, report); err != nil {
		t.Fatalf("printHistoryCompact: %v", err)
	}
	text := out.String()
	// The report's own lines are still the report's: a warning does not displace
	// the counts or the bound they were taken at.
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "2026-10-01 00:00:00") {
		t.Errorf("the report lost its counts or its bound to the warning:\n%s", text)
	}
	if strings.Contains(text, "warning:") {
		t.Errorf("printHistoryCompact wrote a warning to the report's stream:\n%s", text)
	}
	// And the caller has them, in order, ready for stderr.
	if len(report.Warnings) != 1 {
		t.Fatalf("the report carries %d warnings, want 1", len(report.Warnings))
	}
}

// TestPrintPartialHistoryCompactCarriesTheWarning: the run that most needs the
// widened-bound warning is the one that FAILED, because it has already removed rows
// under that bound and possibly moved stamps. The plan attaches the warning before
// it opens a project, so a failing run's report carries it — and the error path
// printed only the header and the per-project lines, so the one run that had
// already done the damage said nothing about the bound that shaped the numbers.
func TestPrintPartialHistoryCompactCarriesTheWarning(t *testing.T) {
	var out strings.Builder
	report := historyCompactReport{
		Apply:    true,
		Before:   "2030-01-01 00:00:00",
		Warnings: []string{"warning: --before 2030-01-01 00:00:00 reaches past 2026-09-28 17:14:07"},
		Projects: []memory.HistoryCompactResult{{ProjectID: "beta", Removed: 5, UpdatedAt: 1}},
	}
	if err := printPartialHistoryCompact(&out, report); err != nil {
		t.Fatalf("printPartialHistoryCompact: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "warning:") {
		t.Errorf("the partial report drops the warning, so a run that already removed rows under a "+
			"widened bound says nothing about it:\\n%s", text)
	}
	if !strings.Contains(text, "stopped partway") {
		t.Errorf("the partial report lost its header:\\n%s", text)
	}
	if !strings.Contains(text, "beta") || !strings.Contains(text, "5") {
		t.Errorf("the partial report lost its counts:\\n%s", text)
	}
	want := "  removing only versions recorded before 2030-01-01 00:00:00\n"
	if !strings.Contains(text, want) {
		t.Errorf("the partial report does not name the bound its counts were taken at (want %q):\\n%s", want, text)
	}
}
