package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// Issue #730 at the CLI layer. The store does the repair
// (internal/memory/history_compact_test.go covers what it removes and what it
// leaves); these are the three things only the command can be wrong about — the
// arguments, the refusal, and what an operator is told.

// TestHistoryCompactHelpAndDocsStateTheSameRule: the anchor rule lives in three
// places — the code, docs/cli.md, and the help an operator actually reads — and
// this is the test for the drift between them.
//
// A review of #735 found the help still promising the state-change anchor the PR
// removes, which is how 49 of 288 stamps got invented: a rule an operator reads is
// a rule they act on, and the one left behind is the wrong one. No behavioural test
// covers help text, and the e2e surface check only proves the help RUNS, so nothing
// would have failed.
//
// Both surfaces are matched with whitespace COLLAPSED, which is the whole difficulty:
// the help is hard-wrapped at one column and the doc at another, so a three-word
// phrase straddles a line break in one of them and not the other. A test matching
// the raw text passes on a help that says the wrong thing — mutating the help back
// to the retired sentence is what proves it — so the wrapping is removed before
// matching and a claim is a claim whether or not an editor reflowed the paragraph.
//
// Each surface also carries its OWN needles for the claims worded differently, and
// what must not differ is which claims are made, not how they read: a test demanding
// identical sentences would push the next edit into copy-paste instead of agreement.
// The retired sentences are the sharper half — a rule removed from the code and left
// in the help is worse than never documenting it, because it is the sentence a reader
// trusts.
func TestHistoryCompactHelpAndDocsStateTheSameRule(t *testing.T) {
	// Claims both surfaces must make. A surface has to SAY these, not merely avoid
	// contradicting them.
	shared := map[string]string{
		"the anchor is a stamp-moving writer, not a state change": "WRITER moved",
		"a memory with no anchor at all is disclosed":             "no recorded stamp write",
		"a deleted memory is not compacted":                       "since been deleted",
		// The count is load-bearing in its own right: the review's other finding was
		// a help still reading "Five things always stay" beside a sixth rule.
		"the retention list counts the deleted-memory rule": "Six things always stay",
	}
	// Claims each surface must make, in its own words.
	perSurface := map[string]map[string]string{
		"the shipped help (historyUsage)": {
			"a resolve is named as a writer that moves no stamp": "writes resolved_at and deliberately leaves updated_at alone",
			"a fold is named as a writer that moves no stamp":    "folding a duplicate save writes importance and moves nothing",
			"both kinds of unrestorable stamp are reported":      "a stamp no layout reads, and a stamp with no recorded write",
		},
		"docs/cli.md's compact section": {
			"a resolve is named as a writer that moves no stamp": "leaves `updated_at` alone",
			"a fold is named as a writer that moves no stamp":    "changes `importance` and moves nothing",
			"both kinds of unrestorable stamp are reported":      "counted separately (`stamps unreadable`)",
			"the deleted-memory rule gives its reason":           "nobody can restore",
		},
	}
	// Sentences the rule deleted. Each names a reading of the anchor that is wrong
	// in the permissive direction — the one that invents a stamp — so one surviving
	// anywhere is a live defect rather than a stale phrase.
	retired := map[string]string{
		"a non-removable row anchors on its own":        "whether or not it changed state",
		"an anchor may be a row that changed nothing":   "A row it will not remove is an anchor",
		"the retention list predates the sixth rule":    "Five things always stay",
		"the anchor is the last state-changing version": "newest version that CHANGED state",
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("read docs/cli.md: %v", err)
	}
	// Scoped to the compact section, so a claim satisfied by unrelated text in the
	// file cannot pass this — a long reference document repeats phrases.
	rest, ok := compactDocSection(string(doc))
	if !ok {
		t.Fatal(`docs/cli.md has no "#### ` + "`ghost history compact`" + `" section`)
	}

	for _, surface := range []struct{ name, text string }{
		{"the shipped help (historyUsage)", historyUsage},
		{"docs/cli.md's compact section", rest},
	} {
		flat := squashSpace(surface.text)
		for claim, needle := range shared {
			if !strings.Contains(flat, squashSpace(needle)) {
				t.Errorf("%s does not state %s: no %q. Shipped text is what an operator reads, and a "+
					"rule that lives only in a comment has not been shipped.",
					surface.name, claim, needle)
			}
		}
		for claim, needle := range perSurface[surface.name] {
			if !strings.Contains(flat, squashSpace(needle)) {
				t.Errorf("%s does not state %s: no %q", surface.name, claim, needle)
			}
		}
		for wrong, needle := range retired {
			if strings.Contains(flat, squashSpace(needle)) {
				t.Errorf("%s still says %s (%q): that is a reading of the anchor this change removed, and "+
					"the permissive one is the one that invents a stamp", surface.name, wrong, needle)
			}
		}
	}
}

// squashSpace collapses every run of whitespace to one space and trims, so a phrase
// matches a hard-wrapped surface whatever column the wrap landed at. The surfaces are
// hand-wrapped text, so this is not a convenience: it is the difference between the
// test seeing a sentence and seeing half of one.
func squashSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// compactDocSection returns docs/cli.md's `ghost history compact` section, up to the
// next heading. Scoping is the point: the file is a whole reference and will contain
// this section's vocabulary somewhere unrelated, and a claim satisfied over there is a
// claim that proves nothing.
func compactDocSection(doc string) (string, bool) {
	const start = "#### `ghost history compact`"
	i := strings.Index(doc, start)
	if i < 0 {
		return "", false
	}
	rest := doc[i+len(start):]
	if j := strings.Index(rest, "\n#### "); j >= 0 {
		rest = rest[:j]
	} else if j := strings.Index(rest, "\n### "); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

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
		// The bound is checked rather than passed through: a --before this
		// command cannot read is a bound the store would refuse anyway, and
		// refusing it here means the refusal arrives before any project is opened
		// rather than inside the first one.
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
	// And the read mode still reads: a bare word is a memory id, and `purge` is
	// still the redaction path. A routing that claimed either would be a
	// compaction that answers a read, or a purge that silently compacts.
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

// TestHistoryCompactRefusesWhileTheLifecycleLockIsHeld: the repair rewrites the
// two things an unattended lifecycle pass is appending to, so running it under
// one is the race the issue's own wording rules out. A refusal has to name the
// project, or an operator cannot tell which of twenty to wait for — and it has to
// refuse BEFORE any project is touched, since a run that compacted one project
// and then found another locked has already written.
func TestHistoryCompactRefusesWhileTheLifecycleLockIsHeld(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()
	claimLifecycle(t, dataDir, "beta")

	report, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Apply: true})
	if err == nil {
		t.Fatalf("compaction ran with beta's lifecycle lock held: %+v", report)
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("refusal %q does not name the project whose lock is held", err)
	}
	if len(report.Projects) != 0 {
		t.Errorf("the refusal still reported %d project result(s): %+v", len(report.Projects), report.Projects)
	}

	// Scoped to the idle project, the same store is compactable: the refusal is
	// about the named project, not about the store having one busy project.
	report, err = runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{Apply: true, Project: "alpha"})
	if err != nil {
		t.Fatalf("compaction scoped to the idle project: %v", err)
	}
	if len(report.Projects) != 1 || report.Projects[0].ProjectID != "alpha" {
		t.Errorf("scoped report = %+v, want one line for alpha", report.Projects)
	}
}

// TestHistoryCompactReportsWhatAFailedRunAlreadyCompacted: the only path this
// command can destroy part of a store, and the one report whose job is to say how
// much of the store a run touched.
//
// Each project is compacted in its own committed batches, so a failure on project
// N leaves projects 1..N-1 REWRITTEN. An error that carried no counts would answer
// "this run changed nothing" about a store it changed, and an operator reading it
// has no way to know which projects to re-check.
//
// The failing project is in that set too, and that is the half this pins: its
// batches are committed one at a time, so a store with tens of thousands of
// redundant versions has already deleted thousands of them by the time anything
// fails. The store carries those counts back on its error
// (HistoryCompactResult beside the error), and the run has to hand them on rather
// than dropping the project for having failed — a report that names only the
// projects that finished tells an operator to re-run the whole store, which
// re-checks twenty projects because one of them failed partway through its own
// batches.
func TestHistoryCompactReportsWhatAFailedRunAlreadyCompacted(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()

	// The seam: the first project succeeds, the second fails AFTER committing
	// batches of its own. A store that answers for one project and not the next
	// does not exist in a fixture, and this is exactly the path that needs one.
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

	// And it reaches the operator, under a header that cannot be read as a finished
	// repair, with the failure marked on its own line rather than only in the
	// header: an operator reading one line needs to know which project it is.
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

	// A refusal is not a partial: nothing was touched, so there is nothing to
	// disclose and printing "already compacted" over an empty set would be a claim
	// about a store this run never opened.
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

// TestHistoryCompactNamesTheCutInItsReport: every count this command prints is a
// count AT a bound, and "18 redundant versions" is a different number at each one.
// A report that printed the number without the bound would be reporting half an
// answer, and an operator with a store whose clock is behind has no way to tell
// whether the default bound already covered it.
func TestHistoryCompactNamesTheCutInItsReport(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)

	// The default, and the whole point of printing it: a store that predates #727
	// has been cut at this instant whether or not anybody asked for it.
	report, err := runHistoryCompactPlan(ctx, s, t.TempDir(), historyCompactOptions{})
	if err != nil {
		t.Fatalf("runHistoryCompactPlan: %v", err)
	}
	cut, err := memory.ResolveCompactCutoff("")
	if err != nil {
		t.Fatalf("resolve the default cut: %v", err)
	}
	if report.Before != cut {
		t.Errorf("the report's cut = %q, want the default %q", report.Before, cut)
	}
	for _, p := range report.Projects {
		if p.Before != cut {
			t.Errorf("%s reports the cut %q, want the same %q the report header names", p.ProjectID, p.Before, cut)
		}
	}
	var out strings.Builder
	if err := printHistoryCompact(&out, report); err != nil {
		t.Fatalf("printHistoryCompact: %v", err)
	}
	if !strings.Contains(out.String(), cut) {
		t.Errorf("the printed report does not name the cut %q it used:\n%s", cut, out.String())
	}

	// And a widened bound is the one named, not the default: printing the default
	// over a run that used another would be the half-answer with an extra step.
	report, err = runHistoryCompactPlan(ctx, s, t.TempDir(),
		historyCompactOptions{Before: "2026-09-28T18:00:01Z"})
	if err != nil {
		t.Fatalf("runHistoryCompactPlan with a bound: %v", err)
	}
	if report.Before != "2026-09-28 18:00:01" {
		t.Errorf("the report's cut = %q, want the normalized 2026-09-28 18:00:01", report.Before)
	}
	out.Reset()
	if err := printHistoryCompact(&out, report); err != nil {
		t.Fatalf("printHistoryCompact: %v", err)
	}
	if !strings.Contains(out.String(), "2026-09-28 18:00:01") {
		t.Errorf("the printed report does not name the cut this run used:\n%s", out.String())
	}
}

// TestHistoryCompactBeforeChangesWhatIsRemoved drives the flag all the way through:
// the string in argv, the parser, the resolver, the store's predicate and the
// printed report. The store tests prove what a bound means; this proves the
// command's flag is the one that means it, which is a different bug and just as
// expensive — a --before the parser swallowed would leave an operator widening a
// bound they had every reason to believe was in force.
//
// The fixture dates its own rows, because a store seeded with datetime('now') has
// nothing on either side of the default cut to tell the two answers apart.
func TestHistoryCompactBeforeChangesWhatIsRemoved(t *testing.T) {
	ctx := context.Background()
	// A dry run, so the two runs can be compared: the first has to leave the
	// store alone for the second to still be reading the same rows.
	seed := func(t *testing.T) *memory.Store {
		t.Helper()
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
			Category: "gotcha", Content: "the relay listens on port 2222 in staging",
			Source: "mcp", Importance: 0.5,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		restate(t, dbPath, id, cliPreFixReflectAt)
		restate(t, dbPath, id, cliPreFixReflectAt)
		restate(t, dbPath, id, cliPostFixReflectAt)
		restate(t, dbPath, id, cliPostFixReflectAt)
		// Four restatements, each byte-identical to the row before it. The two on
		// the older side of the cut are removable, and the two on the later side
		// are not: the first of those is spared by the cut and the second because it
		// is the memory's newest version, which nothing removes.
		return s
	}

	underDefault, err := runHistoryCompactPlan(ctx, seed(t), t.TempDir(), historyCompactOptions{})
	if err != nil {
		t.Fatalf("dry run under the default cut: %v", err)
	}
	if got := underDefault.Projects[0].Removed; got != 2 {
		t.Errorf("Removed = %d under the default cut, want 2 (the two restatements older than %s)",
			got, cliPreFixReflectAt)
	}
	widened, err := runHistoryCompactPlan(ctx, seed(t), t.TempDir(),
		historyCompactOptions{Before: "2026-09-28T18:00:01Z"})
	if err != nil {
		t.Fatalf("dry run with a widened cut: %v", err)
	}
	if got := widened.Projects[0].Removed; got != 3 {
		t.Errorf("Removed = %d with the cut past %s, want 3: --before did not reach the store",
			got, cliPostFixReflectAt)
	}
}

// The two sides of the #727 cut, in the store's own stamp layout. They are fixed
// instants rather than the clock: a bound is a historical fact, and a fixture that
// read datetime('now') would put every row on the same side of it forever.
const (
	cliPreFixReflectAt  = "2026-08-01 09:00:00"
	cliPostFixReflectAt = "2026-09-28 18:00:00"
)

// restate appends one reflect version that restates the memory's newest version
// byte for byte, recorded at the given instant. It copies the recorded state out of
// the row before it rather than writing literals, so the row it stages is exactly
// the kind this repair is meant to look at — and a fixture that hand-wrote the
// columns would be staging something the store's writers never produce.
func restate(t *testing.T, dbPath, memoryID, recordedAt string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open the store for staging: %v", err)
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec(`
		INSERT INTO memory_history
			(memory_id, project_id, phase, recorded_at,
			 content, category, importance, resolved_at, source)
		SELECT memory_id, project_id, 'reflect', ?,
			content, category, importance, resolved_at, source
		FROM memory_history
		WHERE rowid = (SELECT max(rowid) FROM memory_history WHERE memory_id = ?)`,
		recordedAt, memoryID)
	if err != nil {
		t.Fatalf("append a restatement at %s: %v", recordedAt, err)
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

// TestHistoryCompactReportsPerProject: the counts are per project, and the
// report is what an operator reads to decide whether to pass --apply. A report
// that summed them into one number would answer a question nobody asked.
func TestHistoryCompactReportsPerProject(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()

	report, err := runHistoryCompactPlan(ctx, s, dataDir, historyCompactOptions{})
	if err != nil {
		t.Fatalf("runHistoryCompact: %v", err)
	}
	if len(report.Projects) != 2 {
		t.Fatalf("report covers %d project(s), want 2: %+v", len(report.Projects), report.Projects)
	}
	if report.Projects[0].ProjectID != "alpha" || report.Projects[1].ProjectID != "beta" {
		t.Errorf("report order = %q, %q; want the store's own project order (alpha, beta)",
			report.Projects[0].ProjectID, report.Projects[1].ProjectID)
	}
	if report.Apply {
		t.Error("the report claims an apply when no --apply was given")
	}
	// Nothing to remove in a store with one version per memory, and the report
	// has to say so rather than print nothing: an empty report reads as "no
	// projects", which is a different claim.
	for _, p := range report.Projects {
		if p.Removed != 0 {
			t.Errorf("%s reports %d removals in an undamaged store", p.ProjectID, p.Removed)
		}
	}
	var out strings.Builder
	if err := printHistoryCompact(&out, report); err != nil {
		t.Fatalf("printHistoryCompact: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "beta") {
		t.Errorf("the report does not name both projects:\n%s", text)
	}
	if !strings.Contains(text, "dry run") {
		t.Errorf("a report from a run without --apply does not say it is a dry run:\n%s", text)
	}
}

// TestHistoryCompactNamesTheStampsItCouldNotRestore: the report has to say which
// rows it left wrong, and it has to say the two different reasons separately.
//
// A memory with a no-op flood above it and no recorded stamp write is not repaired —
// nothing in its history was written by a writer that moves updated_at, so there is
// no instant to put its stamp back to. That row's versions are still removed, so a
// report of "0 stamps restored" over a store full of removed versions reads as a
// finished repair. And the count is not the unreadable one: an unreadable stamp is
// a value that exists and cannot be parsed, and this is a value whose AUTHOR is
// absent — one number for two different faults would send an operator looking for a
// timestamp shape that is not the problem.
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
	// A pre-v17 memory: no save version, so the only version left is one whose
	// writer moves no stamp. Then the no-op flood above it, dated on the far side of
	// the default cut so the run has rows to remove.
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

// TestHistoryCompactWarnsOnABoundThatReachesRowsACurrentBuildWrote: a widened
// --before is the one thing an operator can ask for that the store cannot
// distinguish from a mistake, and it is not refused — a store whose clock was
// behind when the rows were written has no other way to be repaired. So it is
// warned about, and the warning has to be on the screen while the operator reads
// the numbers rather than after they have decided.
//
// The default does not warn, and that is the load-bearing half: the default IS
// reflectNoOpCutoff, so a test at-or-after the cutoff would have every invocation
// of a command whose zero configuration is the safe one printing a warning, and a
// warning that is always on is a warning nobody reads.
func TestHistoryCompactWarnsOnABoundThatReachesRowsACurrentBuildWrote(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	def, err := memory.ResolveCompactCutoff("")
	if err != nil {
		t.Fatalf("resolve the default cut: %v", err)
	}

	for _, tc := range []struct {
		name      string
		before    string
		wantWarns bool
	}{
		{name: "the default bound", before: "", wantWarns: false},
		{name: "the default bound, spelled out", before: def, wantWarns: false},
		{name: "a whole day earlier", before: "2026-01-01", wantWarns: false},
		{name: "a second earlier", before: "2026-09-28T17:14:06Z", wantWarns: false},
		{name: "a second later", before: "2026-09-28T17:14:08Z", wantWarns: true},
		{name: "a whole day later", before: "2026-10-01", wantWarns: true},
		{name: "far later", before: "2030-01-01T00:00:00Z", wantWarns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both modes, because a risk that only --apply realises is still a risk
			// the dry run has to disclose: `--before <t> --apply` is one command.
			for _, apply := range []bool{false, true} {
				report, err := runHistoryCompactPlan(ctx, s, t.TempDir(),
					historyCompactOptions{Apply: apply, Before: tc.before})
				if err != nil {
					t.Fatalf("runHistoryCompactPlan(apply=%v): %v", apply, err)
				}
				if got := len(report.Warnings) > 0; got != tc.wantWarns {
					t.Fatalf("apply=%v: warned = %v, want %v (warnings %v)",
						apply, got, tc.wantWarns, report.Warnings)
				}
				for _, w := range report.Warnings {
					// A warning that does not name the risk is a warning about a
					// bound, and the operator already knows which bound they typed.
					for _, want := range []string{def, "tags", "dry run"} {
						if !strings.Contains(w, want) {
							t.Errorf("the warning does not mention %q, so it does not say what "+
								"the widened bound risks:\n%s", want, w)
						}
					}
				}
			}
		})
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
	// printHistoryCompact does NOT print the warnings itself — they go to stderr
	// from runHistoryCompact, and a function that wrote them to this writer would
	// put a diagnostic on the wrong stream.
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
//
// The warning is the last thing to go, not the first: it is the context that makes
// the partial counts legible, and an operator reading "beta 5 redundant
// version(s)" has no way to know those five were removable under a bound that
// reaches past #727.
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
	// The warning and the header both have to be there: a header reading as a
	// finished repair, or a warning with no indication the run stopped, each tell
	// half the story.
	if !strings.Contains(text, "stopped partway") {
		t.Errorf("the partial report lost its header:\\n%s", text)
	}
	if !strings.Contains(text, "beta") || !strings.Contains(text, "5") {
		t.Errorf("the partial report lost its counts:\\n%s", text)
	}
}

// TestRunHistoryCompactWarnsBeforeItsReport is the ordering, and it is a separate
// thing from the warning existing. A warning printed after the counts is one a
// reader has already scrolled past, and the counts are what they asked for.
func TestPrintHistoryCompactWarningsOrder(t *testing.T) {
	var out strings.Builder
	if err := printHistoryCompactWarnings(&out, historyCompactReport{
		Warnings: []string{"warning: first", "warning: second"},
	}); err != nil {
		t.Fatalf("printHistoryCompactWarnings: %v", err)
	}
	if got := out.String(); got != "warning: first\nwarning: second\n" {
		t.Errorf("warnings printed as %q, want both in order, one per line", got)
	}
	// And a report with no warnings writes nothing at all — a blank line where the
	// risk would have been is its own kind of noise.
	empty := strings.Builder{}
	if err := printHistoryCompactWarnings(&empty, historyCompactReport{}); err != nil {
		t.Fatalf("printHistoryCompactWarnings on an empty report: %v", err)
	}
	if empty.String() != "" {
		t.Errorf("a run with nothing to warn about printed %q", empty.String())
	}
}
