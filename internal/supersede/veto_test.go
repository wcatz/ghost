package supersede

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// #686's deterministic veto. A standing rule is not a stale fact: the older
// wrong edge #686's judge found demoted exactly such a note. The vocabulary is
func TestVetoSupersedeKeepsARuleTheNewerNoteDoesNotRetire(t *testing.T) {
	cases := []struct {
		name       string
		older      string
		newer      string
		wantVetoed bool
		wantReason string
	}{
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

// work the pass looked at.

func TestVetoSupersedeLetsEveryRetirementMarkerThrough(t *testing.T) {
	rule := "NEVER merge on Fridays; the release is cut on Tuesdays instead."
	cases := []struct {
		marker string
		newer  string
	}{
		{"no longer", "The no-Friday-merge rule is no longer in force."},
		{"retire", "We retired the Friday merge ban last sprint."},
		{"retires", "The policy retires the Friday ban in the next release."},
		{"retired", "The Friday ban was retired; deploys run on Tuesdays."},
		{"retiring", "The team is retiring the Friday merge rule."},
		{"retirement noun", "Retirement of the Friday merge rule starts next sprint."},
		{"remove", "We remove the Friday restriction from the branch policy."},
		{"removes", "The policy removes the Friday restriction."},
		{"removed", "The Friday restriction was removed in the policy rewrite."},
		{"removing", "The next release is removing the Friday restriction."},
		{"removal noun", "The removal of the Friday merge restriction is complete."},
		{"deprecated", "The Friday merge rule is deprecated; use the canary gate."},
		{"obsolete", "The Friday merge rule is obsolete after the canary gate landed."},
		{"replace", "The canary gate replaces the Friday merge rule."},
		{"replaced", "The Friday merge rule was replaced by the canary gate."},
		{"superseded", "The Friday merge rule is superseded by the canary gate."},
		{"dropped", "The Friday merge rule was dropped from the branch policy."},
		{"relaxed", "The Friday merge rule was relaxed; any weekday works now."},
		{"loosened", "The Friday merge rule was loosened after the canary gate landed."},
		{"lifted", "The Friday merge ban was lifted once the gate went green."},
		{"waived", "The Friday merge ban was waived for the 1.x line only."},
		{"must now", "A merge must now wait for the canary gate, on any weekday."},
		{"not required", "The Friday merge ban is not required any more."},
		{"exception to", "An exception to the Friday ban applies to release branches."},
	}
	// rather than as a failure.
	words := []string{
		"no longer", "retire", "retires", "retired", "retiring", "retirement",
		"remove", "removes", "removed", "removing", "removal",
		"deprecated", "obsolete", "replace", "replaced", "superseded", "dropped",
		"relaxed", "loosened", "lifted", "waived", "must now", "not required",
		"not be required",
		"exception to",
	}
	for _, c := range cases {
		if reason, vetoed := VetoSupersede(Candidate{OlderContent: rule, NewerContent: c.newer}); vetoed {
			t.Errorf("marker %q: the veto stood on a pair the newer note retires (reason %q)", c.marker, reason)
		}
	}
	for _, w := range words {
		if !namesRetirement(w) {
			t.Errorf("marker word %q is recognised by no entry in retireMarkers, so the veto stands on a pair the newer note retires", w)
		}
	}
}

// the one form every marker already had.
//
// five is held.
func TestNamesRetirementCoversEveryInflection(t *testing.T) {
	// The real four-way verb inflections, base first.
	verbs := []struct {
		word  string
		forms []string
	}{
		{"retire", []string{"retire", "retires", "retired", "retiring"}},
		{"remove", []string{"remove", "removes", "removed", "removing"}},
		{"replace", []string{"replace", "replaces", "replaced", "replacing"}},
		{"supersede", []string{"supersede", "supersedes", "superseded", "superseding"}},
		{"drop", []string{"drop", "drops", "dropped", "dropping"}},
		{"deprecate", []string{"deprecate", "deprecates", "deprecated", "deprecating"}},
		{"relax", []string{"relax", "relaxes", "relaxed", "relaxing"}},
		{"loosen", []string{"loosen", "loosens", "loosened", "loosening"}},
		{"lift", []string{"lift", "lifts", "lifted", "lifting"}},
		{"waive", []string{"waive", "waives", "waived", "waiving"}},
	}
	for _, v := range verbs {
		for _, form := range v.forms {
			if !namesRetirement(form) {
				t.Errorf("namesRetirement(%q) = false, want true: it is a way to retire %q, and a veto that misses it stands on a genuine supersession (costs recall)", form, v.word)
			}
		}
	}

	// matching.
	for _, noun := range []string{
		"retiral", "retirement", "removal", "deprecation", "replacement",
		"supersession", "supersessions",
	} {
		if !namesRetirement(noun) {
			t.Errorf("namesRetirement(%q) = false, want true: it is the noun form of a retirement marker, and no verb form in this test would notice it rotting (costs recall)", noun)
		}
	}

	// standing rule through to the classifier for free.
	nearMiss := []string{
		"dropdown",   // drop + "down"
		"droplet",    // drop + "let"
		"liftoff",    // lift + "off"
		"looseners",  // loosen + "ers"
		"waiver",     // waiv + "er"
		"relaxation", // relax + "ation"
	}
	for _, w := range nearMiss {
		if namesRetirement(w) {
			t.Errorf("namesRetirement(%q) = true, want false: it is not a way to retire a rule, so recognising it lets a note that retires nothing through the veto (costs precision)", w)
		}
	}
}

// returns the same way.
