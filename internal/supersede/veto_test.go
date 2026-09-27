package supersede

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestVetoSupersedeKeepsARuleTheNewerNoteDoesNotRetire is the unit half of
// #686's deterministic veto. A standing rule is not a stale fact: the older
// note's imperative only stops binding when a newer note says so, and every
// wrong edge #686's judge found demoted exactly such a note. The vocabulary is
// resolve's, so these cases hold the two passes to one list.
func TestVetoSupersedeKeepsARuleTheNewerNoteDoesNotRetire(t *testing.T) {
	cases := []struct {
		name       string
		older      string
		newer      string
		wantVetoed bool
		wantReason string
	}{
		// A rule the newer note never mentions, or mentions without retiring.
		{
			name:       "imperative, unrelated follow-up",
			older:      "NEVER run the restore with source and target on the same spindle.",
			newer:      "A restore that spans two spindles took 41 minutes last night and the row count matched afterwards.",
			wantVetoed: true,
			wantReason: "never",
		},
		{
			name:       "must, follow-up on the same subsystem",
			older:      "The deploy script must run the migration before the service restart.",
			newer:      "The restart sent in-flight uploads to a retry queue for about 90 seconds, tracked as a separate issue.",
			wantVetoed: true,
			wantReason: "must",
		},
		{
			name:       "do not, addendum about a second host",
			older:      "Do not edit the ledger table directly; the API is the only supported writer.",
			newer:      "The ledger gained an index on (tenant_id, created_at) in the schema migration.",
			wantVetoed: true,
			wantReason: "do not",
		},
		{
			name:       "always, restatement of the same rule",
			older:      "Always run the preflight check before promoting a canary.",
			newer:      "Preflight promotes a canary only after the shadow bucket drains, which is the behaviour the current script assumes.",
			wantVetoed: true,
			wantReason: "always",
		},
		{
			name:       "required, a follow-up round",
			older:      "A changelog entry is required for every user-visible change.",
			newer:      "Three user-visible changes landed this week: the timeout, the retry budget and the new flag.",
			wantVetoed: true,
			wantReason: "required",
		},
		// A newer note that names the rule as retired or changed is the real
		// supersession of an imperative, and must reach the classifier.
		{
			name:  "retired rule",
			older: "NEVER merge on Fridays.",
			newer: "The no-Friday-merge rule is retired: release trains now cut on Tuesdays.",
		},
		{
			name:  "no longer required",
			older: "A soak test is required before every release.",
			newer: "The pre-release soak is no longer required; the canary gate covers the same window.",
		},
		{
			name:  "rule removed",
			older: "Do not merge without a second reviewer's approval.",
			newer: "The second-reviewer requirement was removed from the branch policy in favour of the CODEOWNERS gate.",
		},
		{
			name:  "rule superseded",
			older: "Always pin the release candidate before a release.",
			newer: "That pinning rule is superseded by the immutable tag workflow.",
		},
		{
			name:  "rule relaxed",
			older: "Do not merge a changeset touching the ledger schema without a migration review.",
			newer: "The migration-review requirement was relaxed for additive columns; destructive changes still need it.",
		},
		// No imperative in the older note: nothing to veto, whatever the newer
		// note says.
		{
			name:  "two true facts",
			older: "The pool timeout is 5 seconds in the development profile.",
			newer: "The production profile's pool timeout is 30 seconds, set at boot from the profile file.",
		},
		{
			name:  "an open marker is not an imperative",
			older: "STILL OPEN: the ledger rewrite is not finished.",
			newer: "The rewrite's second milestone landed; the third has not started.",
		},
		{
			name:  "an event record with a modal verb is not an imperative",
			older: "The outage on 2026-04-02 lasted 31 minutes and the mitigation was a cache flush.",
			newer: "The postmortem for that outage is filed with three follow-up actions.",
		},
	}
	for _, c := range cases {
		reason, vetoed := VetoSupersede(Candidate{OlderContent: c.older, NewerContent: c.newer})
		if vetoed != c.wantVetoed {
			t.Errorf("%s: vetoed = %v, want %v (reason %q)", c.name, vetoed, c.wantVetoed, reason)
			continue
		}
		if c.wantVetoed && !strings.Contains(reason, c.wantReason) {
			t.Errorf("%s: reason %q does not name the fired pattern %q", c.name, reason, c.wantReason)
		}
		if c.wantVetoed && !strings.Contains(reason, "does not retire") {
			t.Errorf("%s: reason %q does not say the newer note left the rule standing", c.name, reason)
		}
	}
}

// TestVetoSupersedeSharesResolveVocabulary: the veto reads resolve's imperative
// list rather than a copy of it, so a pattern added to protect a memory from
// being buried by resolve also protects it from being demoted here. It is
// checked in the direction that matters — every imperative resolve vetoes on
// vetoes here — and in the direction that keeps the lists from merging by
// accident: resolve's open markers are not imperatives.
func TestVetoSupersedeSharesResolveVocabulary(t *testing.T) {
	imperatives := []string{
		"never do this", "Do not do this", "don't do this", "you must do this",
		"always do this", "this is required",
	}
	for _, text := range imperatives {
		if _, vetoed := VetoSupersede(Candidate{OlderContent: text, NewerContent: "a follow-up note"}); !vetoed {
			t.Errorf("%q: an imperative resolve vetoes on must settle the supersede veto too", text)
		}
	}
	// resolve's open markers say a problem is unresolved, which a later note
	// can genuinely overturn; an imperative is a rule.
	openMarkers := []string{
		"not yet fixed", "outstanding", "still pending", "still open", "still stale",
		"unresolved", "todo",
	}
	for _, text := range openMarkers {
		if _, vetoed := VetoSupersede(Candidate{OlderContent: text, NewerContent: "it is fixed now"}); vetoed {
			t.Errorf("%q: an open marker is not a rule and must not settle the supersede veto", text)
		}
	}
}

// TestRunVetoesARuleBeforeAnyHarnessCall is the pass half: a vetoed pair costs no
// classify call, writes no link, and is counted so the operator can see the
// pass declined work it did not do. The pair is left out of the NEITHER cache on
// purpose — the veto is recomputed free on every pass, and a cache row would
// only add a stale row to reason about.
func TestRunVetoesARuleBeforeAnyHarnessCall(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	rule := add(t, store, db, "NEVER run the restore with source and target on the same spindle.", []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	add(t, store, db, "A restore that spanned two spindles took 41 minutes and the row count matched afterwards.", []float32{0.999, 0.001, 0, 0}, "2026-06-01 00:00:00")

	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vetoed != 1 {
		t.Errorf("Vetoed = %d, want 1", res.Vetoed)
	}
	if cls.Calls() != 0 {
		t.Errorf("classify calls = %d, want 0: the veto settles the pair before any harness call", cls.Calls())
	}
	if res.Confirmed != 0 || res.Created != 0 {
		t.Errorf("a vetoed pair must write nothing: confirmed=%d created=%d", res.Confirmed, res.Created)
	}
	if len(classified) != 0 {
		t.Errorf("classified = %+v, want none for a vetoed pair", classified)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{rule}); len(pairs) != 0 {
		t.Errorf("vetoed pair wrote %d supersedes link(s); the rule must stay undemoted", len(pairs))
	}
	checked, err := store.SupersedeChecked(ctx, "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checked) != 0 {
		t.Errorf("a vetoed pair must not be cached as NEITHER: %v", checked)
	}
	// The pair is offered again on the next pass: the veto is recomputed, not
	// remembered, so a later note that does retire the rule still reaches the
	// classifier.
	second := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res2, _, err := Run(ctx, store, second, "p", 0.9, true, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if res2.Vetoed != 1 || second.Calls() != 0 {
		t.Errorf("second pass: vetoed=%d calls=%d, want 1 and 0 — the veto is free and repeatable, not cached", res2.Vetoed, second.Calls())
	}
}

// TestRunStillClassifiesARuleTheNewerNoteRetires: the veto's second half is the
// one that could swallow a real supersession. A newer note that names the rule
// as retired must still reach the classifier, because whether the retirement
// covers the SAME rule is the judgement the veto refuses to make.
func TestRunStillClassifiesARuleTheNewerNoteRetires(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	rule := add(t, store, db, "NEVER merge on Fridays.", []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	retired := add(t, store, db, "The no-Friday-merge rule is retired: release trains now cut on Tuesdays.", []float32{0.999, 0.001, 0, 0}, "2026-06-01 00:00:00")

	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: merging on Fridays is forbidden"})
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vetoed != 0 {
		t.Errorf("Vetoed = %d, want 0: the newer note retires the rule, so the classifier decides", res.Vetoed)
	}
	if cls.Calls() != 1 {
		t.Errorf("classify calls = %d, want 1", cls.Calls())
	}
	if res.Created != 1 {
		t.Errorf("Created = %d, want 1", res.Created)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{rule, retired}); len(pairs) != 1 {
		t.Errorf("want the retired rule's link written, got %d pair(s)", len(pairs))
	}
}

// TestRunVetoLeavesAnExistingEdgeAlone: the ordinary pass is a creation pass.
// A vetoed pair is not classified and not written, and an edge a previous rubric
// already wrote is left for the repair path (`ghost supersede --reassess`) to
// withdraw — the same line the scope guard draws, and the same reason: the veto
// is a signal about one pair, not a verdict about the graph.
func TestRunVetoLeavesAnExistingEdgeAlone(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	rule := add(t, store, db, "NEVER run the restore with source and target on the same spindle.", []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "A restore that spanned two spindles took 41 minutes and the row count matched afterwards.", []float32{0.999, 0.001, 0, 0}, "2026-06-01 00:00:00")
	// An edge an older, less careful rubric wrote.
	if err := store.CreateLink(ctx, newer, rule, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// Backdate it so skip-if-unchanged holds the pair quiet and the veto is what
	// keeps it from being re-judged.
	if _, err := db.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, rule,
	); err != nil {
		t.Fatal(err)
	}

	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.Calls() != 0 {
		t.Errorf("classify calls = %d, want 0: a vetoed pair never reaches the classifier", cls.Calls())
	}
	if res.Vetoed != 1 {
		t.Errorf("Vetoed = %d, want 1", res.Vetoed)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{rule, newer}); len(pairs) != 1 {
		t.Errorf("the ordinary pass must leave the existing edge for --reassess, got %d pair(s)", len(pairs))
	}
}

// TestRunVetoedPairIsNotCountedAsACandidate: the veto is a per-outcome count, in
// the same place the NEITHER cache's is: the pair was put forward and the pass
// declined to spend a call on it, so "N candidate pairs" still describes the
// work the pass looked at.
func TestRunVetoedPairIsNotCountedAsACandidate(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	add(t, store, db, "NEVER merge on Fridays.", []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	add(t, store, db, "A restore that spanned two spindles took 41 minutes.", []float32{0.999, 0.001, 0, 0}, "2026-06-01 00:00:00")

	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1: a vetoed pair was still a candidate the pass considered", res.Candidates)
	}
	if res.Vetoed != 1 {
		t.Errorf("Vetoed = %d, want 1", res.Vetoed)
	}
}

// TestVetoSupersedeReasonNamesTheFiredPattern: the reason is what a log line and
// the CLI report carry, so it has to say which signal fired — resolve.VetoKeep
// returns the same way.
func TestVetoSupersedeReasonNamesTheFiredPattern(t *testing.T) {
	reason, vetoed := VetoSupersede(Candidate{
		OlderContent: "You must not use the deprecated restore path.",
		NewerContent: "A follow-up note about the index rebuild.",
	})
	if !vetoed {
		t.Fatal("want a veto for an older note stating a rule")
	}
	if !strings.Contains(reason, "must") {
		t.Errorf("reason %q does not name the pattern resolve.VetoKeep would return", reason)
	}
}
