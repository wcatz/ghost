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
	if !opts.Apply || opts.Grace != 48*time.Hour || !opts.HasGrace || opts.Project != "ghost" {
		t.Errorf("parsed %+v, want apply + 48h + ghost", opts)
	}

	inline, err := parsePruneArgs([]string{"--apply", "--grace=0s"})
	if err != nil {
		t.Fatalf("parsePruneArgs(--grace=0s): %v", err)
	}
	if !inline.HasGrace || inline.Grace != 0 {
		t.Errorf("--grace=0s parsed as %+v; an explicit zero is a request, not an absence", inline)
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
	if err := printPrune(&applied, pruneView{Applied: true, Candidates: candidates, Removed: 1, Grace: 168 * time.Hour}); err != nil {
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
