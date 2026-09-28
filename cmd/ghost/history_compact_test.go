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

// Issue #730 at the CLI layer. The store does the repair
// (internal/memory/history_compact_test.go covers what it removes and what it
// leaves); these are the three things only the command can be wrong about — the
// arguments, the refusal, and what an operator is told.

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
func TestHistoryCompactReportsWhatAFailedRunAlreadyCompacted(t *testing.T) {
	ctx := context.Background()
	s := historyCompactTestStore(t)
	dataDir := t.TempDir()

	// The seam: the first project succeeds, the second fails. A store that answers
	// for one project and not the next does not exist in a fixture, and this is
	// exactly the path that needs one.
	prev := compactOneProject
	var calls []string
	compactOneProject = func(_ context.Context, _ *memory.Store, projectID string,
		opts historyCompactOptions) (memory.HistoryCompactResult, error) {
		calls = append(calls, projectID)
		if projectID == "beta" {
			return memory.HistoryCompactResult{}, errors.New("database is locked")
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
	// The first project WAS compacted, so its counts are in the report that comes
	// back with the error.
	if len(report.Projects) != 1 || report.Projects[0].ProjectID != "alpha" {
		t.Fatalf("report = %+v, want the one project that was compacted before the failure", report.Projects)
	}
	if calls[0] != "alpha" {
		t.Errorf("the first project compacted was %q, want alpha (the store's own order)", calls[0])
	}

	// And it reaches the operator, under a header that cannot be read as a finished
	// repair.
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
	if strings.Contains(text, "beta") {
		t.Errorf("the partial report names the project that FAILED as compacted:\n%s", text)
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
