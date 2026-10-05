package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestParsePruneArgsIsDryRunByDefault: the default is the whole safety argument of
// this command. It deletes, so the shape that runs without an explicit --apply
// has to be the one that reports.
func TestParsePruneArgsIsDryRunByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"--project", "ghost"}, {"--grace", "48h"}, {"--grace=30m", "--project=ghost"}} {
		opts, err := parsePruneArgs(args)
		if err != nil {
			t.Fatalf("parsePruneArgs(%v): %v", args, err)
		}
		if opts.Apply {
			t.Errorf("parsePruneArgs(%v) = apply, want the dry run: nothing deletes without --apply", args)
		}
	}
}

// TestParsePruneArgsReadsTheFlags: the three flags, in both spellings, and the
// grace's two states — asked for and not asked for — which are different requests
// (prune anything expired vs. wait a week) and not the same zero.
func TestParsePruneArgsReadsTheFlags(t *testing.T) {
	opts, err := parsePruneArgs([]string{"--apply", "--grace", "48h", "--project", "ghost"})
	if err != nil {
		t.Fatalf("parsePruneArgs: %v", err)
	}
	if !opts.Apply || opts.Grace != 48*time.Hour || opts.Project != "ghost" {
		t.Errorf("parsed %+v, want apply + 48h + ghost", opts)
	}

	// A zero grace is REFUSED rather than folded into the default. The store
	// resolves 0 to the documented week, so honouring an explicit zero would
	// silently run a different command than the one that was asked for — and the
	// report would carry the default's number, so the substitution would be
	// invisible in the output too.
	if _, err := parsePruneArgs([]string{"--grace=0s"}); err == nil {
		t.Error("--grace=0s was accepted, and will be answered with the default week")
	} else if !strings.Contains(err.Error(), "1s") || !strings.Contains(err.Error(), "168h0m0s") {
		t.Errorf("the zero-grace refusal does not name the shortest accepted grace and the default: %v", err)
	}

	// A bare --grace with no value is a mistake, and the message says what a value
	// looks like rather than reporting a parse error about "".
	if _, err := parsePruneArgs([]string{"--grace"}); err == nil {
		t.Error("--grace with no value was accepted")
	} else if !strings.Contains(err.Error(), "duration") {
		t.Errorf("--grace with no value says %q, want it to name the unit", err)
	}
	if _, err := parsePruneArgs([]string{"--grace", "7d"}); err == nil {
		t.Error("--grace 7d was accepted; \"d\" is not a Go duration unit")
	}
	if _, err := parsePruneArgs([]string{"--grace", "-1h"}); err == nil {
		t.Error("a negative grace was accepted")
	}
	if _, err := parsePruneArgs([]string{"--project", "ghost", "--project", "other"}); err == nil {
		t.Error("--project twice was accepted; the second scope would silently win")
	}
}

// TestParsePruneArgsRefusesWhatItDoesNotUnderstand: a mistyped flag has to be an
// error. An ignored --aply would report a deletion that did not happen, and an
// ignored positional would silently drop the scope the operator asked for.
func TestParsePruneArgsRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, args := range [][]string{
		{"--aply"},
		{"--dry-run"},
		{"ghost"},
		{"--project"},
		{"-h"},
	} {
		if opts, err := parsePruneArgs(args); err == nil {
			t.Errorf("parsePruneArgs(%v) was accepted as %+v", args, opts)
		}
	}
}

// TestRenderApplyFailureShowsWhatActuallyLanded: PruneSessionMemories returns
// its report alongside the error so a half-finished prune is visible, and the
// handler must print the batches that committed before it exits non-zero — a
// failure that removed rows in two batches and then hit a busy database would
// otherwise tell the operator nothing but the error, leaving the destruction
// visible only as tombstones nobody knew to look for. The report is filtered to
// the rows that LANDED: the candidate list is the full selection, and the
// headline counts candidates, so printing the raw report would headline a
// removal count no one actually performed.
func TestRenderApplyFailureShowsWhatActuallyLanded(t *testing.T) {
	report := memory.PruneReport{
		Applied: true,
		Grace:   168 * time.Hour,
		Candidates: []memory.PruneCandidate{
			{ID: "row-1", ProjectID: "p", Category: "fact", Content: "the first batch", Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00"},
			{ID: "row-2", ProjectID: "p", Category: "fact", Content: "the second batch", Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00"},
			{ID: "row-3", ProjectID: "p", Category: "fact", Content: "the batch that never committed", Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00"},
		},
		Removed:    2,
		RemovedIDs: []string{"row-1", "row-2"},
	}
	var out bytes.Buffer
	if err := renderApplyFailure(&out, report, "ghost"); err != nil {
		t.Fatalf("renderApplyFailure: %v", err)
	}
	for _, want := range []string{"2 session memories removed", "row-1", "row-2", "the first batch"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the failed apply does not report %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "row-3") || strings.Contains(out.String(), "the batch that never committed") {
		t.Errorf("the failed apply names a row the failed batch never removed:\n%s", out.String())
	}
	if strings.Contains(out.String(), "3 session memories") {
		t.Errorf("the failed apply headlines the candidate count, not the rows that landed:\n%s", out.String())
	}

	// Nothing landed: the failure changed nothing in the store, and the error
	// line already says the run failed — an empty render keeps that path clean.
	out.Reset()
	if err := renderApplyFailure(&out, memory.PruneReport{Applied: true}, "ghost"); err != nil {
		t.Fatalf("renderApplyFailure(empty): %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed apply that landed nothing printed: %q", out.String())
	}
}

// TestPrintPruneSaysWhichRunItWas: the dry run and the apply print the same rows,
// so the headline is the only thing that tells a reader whether anything was
// destroyed — and the dry run's closing line has to name the flag that would do
// it, or a reader is left to infer from the absence of a change they may not have
// checked.
func TestPrintPruneSaysWhichRunItWas(t *testing.T) {
	candidates := []memory.PruneCandidate{{
		ID: "abc123", ProjectID: "p", Category: "fact", Content: "a session note that has expired",
		Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00",
	}}

	var dry bytes.Buffer
	if err := printPrune(&dry, pruneView{Candidates: candidates, Grace: 168 * time.Hour}); err != nil {
		t.Fatalf("printPrune(dry): %v", err)
	}
	for _, want := range []string{"1 session memory would be removed", "dry run, nothing was written", "abc123", "--apply", "a session note that has expired"} {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("the dry run does not say %q:\n%s", want, dry.String())
		}
	}

	var applied bytes.Buffer
	// An applied view carries the ids it removed, not just the count: that is what
	// the headline is counted from, so a run that selected a row and spared it
	// cannot be rendered as a removal. The store always fills both
	// (PruneReport.RemovedIDs is appended beside Removed), so a fixture with a
	// count and no ids is a shape the renderer refuses to overstate.
	if err := printPrune(&applied, pruneView{Applied: true, Candidates: candidates, Removed: 1, RemovedIDs: []string{"abc123"}, Grace: 168 * time.Hour}); err != nil {
		t.Fatalf("printPrune(apply): %v", err)
	}
	for _, want := range []string{"1 session memory removed", "phase=delete", "abc123"} {
		if !strings.Contains(applied.String(), want) {
			t.Errorf("the applied run does not say %q:\n%s", want, applied.String())
		}
	}
	if strings.Contains(applied.String(), "dry run") {
		t.Errorf("an applied run describes itself as a dry run:\n%s", applied.String())
	}
}

// TestPrintPruneReportsOnlyWhatItRemoved: on an apply the candidate list is what
// the PREVIEW selected, while every batch is re-derived from the same predicate
// under the write lock — so a row a concurrent save pinned, re-tiered or re-saved
// between the preview and its own batch is correctly spared, and it is still in
// that list. Headlining len(Candidates) on the success path therefore counted
// rows the run never removed: measured 3050 in the headline against 3049 removed,
// with the spared row still live in the store. The applied report counts and
// lists RemovedIDs, and the rest are named as spared rather than dropped
// silently — a row the operator asked about has to be accounted for either way.
//
// This is the same filter renderApplyFailure applies, one path earlier: without
// RemovedIDs the renderer cannot tell a removal from a prediction, so the split
// is the renderer's own and both call sites inherit it.
func TestPrintPruneReportsOnlyWhatItRemoved(t *testing.T) {
	candidates := []memory.PruneCandidate{
		{ID: "row-1", ProjectID: "p", Category: "fact", Content: "the row the run removed", Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00"},
		{ID: "row-2", ProjectID: "p", Category: "fact", Content: "the row a concurrent save pinned", Retention: memory.RetentionSession, ExpiresAt: "2026-09-01 10:00:00", ActivityAt: "2026-08-20 09:00:00"},
	}
	var out bytes.Buffer
	if err := printPrune(&out, pruneView{
		Applied: true, Candidates: candidates, Removed: 1, RemovedIDs: []string{"row-1"}, Grace: 168 * time.Hour,
	}); err != nil {
		t.Fatalf("printPrune: %v", err)
	}
	report := out.String()
	for _, want := range []string{"1 session memory removed", "row-1", "the row the run removed"} {
		if !strings.Contains(report, want) {
			t.Errorf("the applied report does not say %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "2 session memories") || strings.Contains(report, "removed 2 memories") {
		t.Errorf("the applied report headlines or closes with the candidate count, not the rows it removed:\n%s", report)
	}
	// The spared row is named, with the reason it was spared, and it sits BELOW the
	// removed listing: an operator reading top to bottom has to be able to tell
	// which rows are gone from the ones that are still there.
	spared := strings.Index(report, "spared:")
	if spared < 0 {
		t.Errorf("the applied report does not name the row it spared:\n%s", report)
	} else {
		if !strings.Contains(report[spared:], "spared: 1 row") {
			t.Errorf("the spared line does not count what it spared:\n%s", report)
		}
		if strings.Contains(report[:spared], "the row a concurrent save pinned") {
			t.Errorf("the spared row is listed among the removals:\n%s", report)
		}
		if !strings.Contains(report[spared:], "row-2") {
			t.Errorf("the spared row is not named under the spared line:\n%s", report)
		}
	}

	// Every selected row spared: the headline says nothing was removed, and the
	// closing line must NOT claim there was nothing to remove — this run did
	// select a row, and it stopped matching while the run was walking the batches.
	out.Reset()
	if err := printPrune(&out, pruneView{
		Applied: true, Candidates: candidates[:1], Grace: 168 * time.Hour,
	}); err != nil {
		t.Fatalf("printPrune(all spared): %v", err)
	}
	if !strings.Contains(out.String(), "0 session memories removed") || !strings.Contains(out.String(), "spared: 1 row") {
		t.Errorf("a run where the only candidate was spared does not say so:\n%s", out.String())
	}
	if strings.Contains(out.String(), "no expired session memories") {
		t.Errorf("a run that selected a row and spared it claims nothing was ever prunable:\n%s", out.String())
	}
}

// TestPrintPruneNamesTheScope: a count beside a project name has to mean that
// project, and the default has to say it is every project — an unlabelled count on
// a store-wide run is read as the project's own.
func TestPrintPruneNamesTheScope(t *testing.T) {
	var every bytes.Buffer
	if err := printPrune(&every, pruneView{}); err != nil {
		t.Fatalf("printPrune: %v", err)
	}
	if !strings.Contains(every.String(), "every project") {
		t.Errorf("the store-wide report does not say its scope:\n%s", every.String())
	}

	var one bytes.Buffer
	if err := printPrune(&one, pruneView{Scope: "ghost"}); err != nil {
		t.Fatalf("printPrune: %v", err)
	}
	if !strings.Contains(one.String(), "project ghost") {
		t.Errorf("a project-scoped report does not name the project:\n%s", one.String())
	}
}

// TestPrintPruneStatesAnAbsentDatabase: a machine that has never run Ghost has
// nothing to prune, and "0 session memories would be removed" there reads as a
// measurement of a store that does not exist.
func TestPrintPruneStatesAnAbsentDatabase(t *testing.T) {
	var out bytes.Buffer
	if err := printPrune(&out, pruneView{NoDatabase: true}); err != nil {
		t.Fatalf("printPrune: %v", err)
	}
	if !strings.Contains(out.String(), "no Ghost database") {
		t.Errorf("the no-database report does not say so:\n%s", out.String())
	}
	if strings.Contains(out.String(), "0 session") {
		t.Errorf("the no-database report counts rows of a store it never opened:\n%s", out.String())
	}
}

// TestPruneIsRegisteredInTheHelpTable: a subcommand that is not in the usage table
// has no -h, which is the help contract cmd/ghost/help_test.go enforces for every
// other command; this is the same claim from the command's own side.
func TestPruneIsRegisteredInTheHelpTable(t *testing.T) {
	if _, ok := usageByCommand["prune"]; !ok {
		t.Fatal("prune has no usage entry, so `ghost prune -h` would print the command list")
	}
	if usageByCommand["prune"] != pruneUsage {
		t.Error("the registered prune usage is not the command's own text")
	}
	// The flags whose value the help scan must not mistake for a help request.
	for _, flag := range []string{"--grace", "--project"} {
		if !helpValueFlagsByCommand["prune"][flag] {
			t.Errorf("%s is not registered as a value flag, so `ghost prune %s -h` would run the prune", flag, flag)
		}
	}
}

// TestPruneUsageSaysItIsNeverAutomatic: the one property of this command that
// cannot be enforced in code is the promise that nothing else calls it, and the
// usage text is where a user looks before deleting. It has to say so, and to say
// that the default does not.
func TestPruneUsageSaysItIsNeverAutomatic(t *testing.T) {
	for _, want := range []string{"Dry-run by default", "--apply", "never run for you", "session", "persistent"} {
		if !strings.Contains(pruneUsage, want) {
			t.Errorf("the prune usage does not say %q", want)
		}
	}
}

// TestPrintPruneNamesTheGraceBasisWithoutClaimingItWasATouch is the renderer half
// of #772. Putting expires_at into prune's activity term made the grace basis and
// the row's last real activity two different values, and a report that projects
// the first under the name "last touched" claims an event that did not happen: the
// expiry is derived FORWARD at save time, so on a row never edited since its save
// the two are the same instant and the line carries one timestamp twice under two
// labels.
//
// Both directions are checked, because the fix is not "print more". When the two
// agree there is nothing for a second label to add, so the line must not repeat
// the stamp; when they differ — a row EDITED after its save, which is what makes
// it eligible now rather than a month ago — the basis has to be there. The case
// that is NOT here is a folded row: a fold refreshes expires_at and leaves
// updated_at alone, so its basis is the expiry and the label is omitted anyway.
// TestPruneReportsTheGraceBasisSeparatelyFromActivity pins that, and it is why
// printPruneRow's condition compares stamp VALUES rather than asking what
// touched the row.
func TestPrintPruneNamesTheGraceBasisWithoutClaimingItWasATouch(t *testing.T) {
	t.Run("agrees with the expiry, so the stamp is not repeated", func(t *testing.T) {
		// A row saved and never touched since: the ordinary shape, and the one the
		// save's own derived expiry makes self-referential.
		const expiry = "2026-09-01 10:00:00"
		c := memory.PruneCandidate{
			ID: "abc123", ProjectID: "p", Category: "fact", Content: "a session note nobody has touched",
			Retention:  memory.RetentionSession,
			ExpiresAt:  expiry,
			ActivityAt: "2026-08-20 09:00:00",
			GraceFrom:  expiry,
		}
		var out bytes.Buffer
		if err := printPruneRow(&out, c, "  "); err != nil {
			t.Fatalf("printPruneRow: %v", err)
		}
		line := strings.SplitN(out.String(), "\n", 2)[0]
		if got := strings.Count(line, expiry); got != 1 {
			t.Errorf("the expiry %s appears %d times in %q, want once: the grace basis IS the expiry here, and repeating it reads as two events",
				expiry, got, line)
		}
		if !strings.Contains(line, "last touched 2026-08-20 09:00:00") {
			t.Errorf("the line does not carry the row's real last-touched stamp: %q", line)
		}
		if strings.Contains(line, "grace from") {
			t.Errorf("the line names a grace basis that adds nothing over the expiry it repeats: %q", line)
		}
	})

	t.Run("differs from the expiry, so the basis is named", func(t *testing.T) {
		// A row EDITED after its save, so updated_at is later than the expiry the
		// save derived. The grace then runs from the edit rather than from the
		// expiry, which is the whole reason the two are separate facts: this row is
		// eligible now because of something that happened, not because a value ran
		// out.
		c := memory.PruneCandidate{
			ID: "def456", ProjectID: "p", Category: "fact", Content: "a session note edited after its save",
			Retention:  memory.RetentionSession,
			ExpiresAt:  "2026-09-01 10:00:00",
			ActivityAt: "2026-09-05 09:00:00",
			GraceFrom:  "2026-09-05 09:00:00",
		}
		var out bytes.Buffer
		if err := printPruneRow(&out, c, "  "); err != nil {
			t.Fatalf("printPruneRow: %v", err)
		}
		line := strings.SplitN(out.String(), "\n", 2)[0]
		if !strings.Contains(line, "grace from 2026-09-05 09:00:00") {
			t.Errorf("the line does not name the instant the grace was measured from: %q", line)
		}
		if strings.Count(line, "2026-09-05 09:00:00") != 2 {
			t.Errorf("the line does not carry both readings — the last write and the grace basis are the same instant HERE, and only one of them is the expiry: %q", line)
		}
	})
}

// TestParsePruneArgsRefusesAnEmptyScope: `--project=` is not the same command line as
// no `--project`, and on this command the difference is a DELETION.
//
// `runPrune` resolves a scope only when `opts.Project != ""`, so an empty value falls
// through to `scope = ""` and `PruneSessionMemories` takes its store-wide branch —
// which reaches `_global` rows too, deliberately (docs/cli.md). With `--apply` that
// deletes expired session rows in every project on the machine. The way to ask for a
// store-wide prune is to OMIT the flag, which the report names as "every project";
// spelling it as `--project=` is a script with an unset variable, and it is the one
// spelling that gets there silently.
//
// So the empty value is refused, which is what `parseReflectArgs` and
// `parseSupersedeArgs` already do with the same flag — prune was the outlier.
//
// Occurrences are counted rather than tested with `opts.Project != ""`, for the same
// reason the --audit parser counts them: that test cannot see `--project=
// --project=ghost`, where the first occurrence is empty and the second silently
// becomes the scope.
func TestParsePruneArgsRefusesAnEmptyScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantFail string
	}{
		{name: "an empty attached --project", args: []string{"--project="}, wantFail: "empty"},
		{name: "an empty --project value", args: []string{"--project", ""}, wantFail: "empty"},
		// The dangerous pair: --apply is what turns the silent scope into a
		// store-wide deletion, so it is named in the case rather than left out.
		{name: "an empty --project with --apply", args: []string{"--project=", "--apply"}, wantFail: "empty"},
		{name: "a detached empty --project with --apply", args: []string{"--apply", "--project", ""}, wantFail: "empty"},
		// Two empties stop at the FIRST one, for the reason the --audit parser's
		// matching case gives: there is no scope in the command line at all, so
		// the thing to tell the reader is the empty value.
		{name: "two empties stop at the first", args: []string{"--project=", "--project="}, wantFail: "empty"},
		// A named scope followed by an empty one is still twice, and the empty
		// value is never assigned, so the scope cannot become store-wide on the
		// way out.
		{name: "a named scope then an empty one is twice", args: []string{"--project", "ghost", "--project="}, wantFail: "twice"},
		// The shape the `!= ""` guard cannot see: the first occurrence is empty,
		// so the second looked like the first one had not been given, and it
		// became the scope with no refusal at all. What this case exists for is
		// that silence, not for which of the two refusals wins now — the empty
		// value is refused first, exactly as context_audit_test.go pins for the
		// --audit parser, because a duplicate guard cannot fire until there IS
		// a first value and here there is none.
		{name: "an empty scope then a named one stops at the empty", args: []string{"--project=", "--project", "ghost"}, wantFail: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parsePruneArgs(tc.args)
			if err == nil {
				t.Fatalf("parsePruneArgs(%v) = %+v, want a refusal: an empty scope is a store-wide prune", tc.args, opts)
			}
			if !strings.Contains(err.Error(), tc.wantFail) {
				t.Errorf("refusal = %q, want it to name %q", err.Error(), tc.wantFail)
			}
		})
	}

	// Omitting the flag is still the documented store-wide default, and it is the
	// ONLY way to reach it now. Asserted because refusing the empty value would be
	// a bug if it also removed the default.
	opts, err := parsePruneArgs([]string{"--apply"})
	if err != nil {
		t.Fatalf("parsePruneArgs(--apply): %v", err)
	}
	if opts.Project != "" {
		t.Errorf("Project = %q with no --project, want \"\" — the report names that as every project", opts.Project)
	}
}
