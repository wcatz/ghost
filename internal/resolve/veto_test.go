package resolve

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestVetoKeepIssueExamples is the regression set from issue #640: a judge
// found ~1 resolve in 3 wrongly buried durable knowledge, and imperatives and
// open markers did not protect those notes. Every case below is a real memory
// the maintenance benchmark caught, and each must be vetoed KEEP without any
// harness call.
func TestVetoKeepIssueExamples(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"never rule", "NEVER run `dingo database restore` with source and target on the same spindle"},
		{"do not rule", "Do NOT manually run `dingo-fix serve`; the mithril-sync runbook owns the process"},
		{"don't rule", "don't restart mithril-sync by hand during a reorg"},
		{"must rule", "`DINGO_PLUGINS_STORAGE_*_DATA_DIR` must be unset, the env vars silently override `--data-dir`"},
		{"always rule", "always archive the HDD safety-backup set before an unsupervised re-bootstrap"},
		{"required rule", "a rebase onto origin/main is required before the final push"},
		{"not yet marker", "NOT YET FIXED: the delete-blob path still skips the safety-backup copy"},
		{"outstanding marker", "OUTSTANDING: nobody has checked whether the restore path is safe on one spindle"},
		{"still pending marker", "the reward-import fix is still pending review"},
		{"still open marker", "the backup-deletion safety lesson is still open after the incident"},
		{"still stale marker", "STILL STALE: the compose hash in the runbook points at last month's image"},
		{"unresolved marker", "the env-var precedence question is unresolved; check the plugin loader before assuming"},
		{"todo marker", "TODO: document that a restore on one spindle corrupts the target ledger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, vetoed := VetoKeep(tc.content)
			if !vetoed {
				t.Errorf("VetoKeep(%q) = false, want a KEEP veto", tc.content)
			}
			if reason == "" {
				t.Errorf("VetoKeep(%q) vetoed with an empty reason; the reason names the pattern that fired", tc.content)
			}
		})
	}
}

// TestVetoKeepIsWordBounded: the patterns are word-bounded, so a keyword buried
// inside a longer word is not a signal. Without the boundary these notes would
// all be pinned in the ranked surface forever.
func TestVetoKeepIsWordBounded(t *testing.T) {
	for _, content := range []string{
		"nevertheless the timeline held through the cutover",
		"mustard seeds are not part of the reward fixture",
		"the todos list is empty",
		"requirements were met by the reviewer",
	} {
		if reason, vetoed := VetoKeep(content); vetoed {
			t.Errorf("VetoKeep(%q) = %q, want no veto (word-bounded patterns)", content, reason)
		}
	}
}

// TestVetoKeepLetsGenuineEvidenceThrough is the other half of the contract: the
// vetoes must not swallow the plain concluded-work notes resolve exists to
// demote, or resolve would stop working entirely.
func TestVetoKeepLetsGenuineEvidenceThrough(t *testing.T) {
	for _, content := range []string{
		"Kill experiment found 7.3% cross-session links, so we removed the ranking bonus.",
		"Cost estimate from May: $148/mo projected; actuals have since replaced it.",
		"Postmortem (concluded): deploy failure was a stale hash; mitigated. No open actions.",
		"Changelog: connection leak fixed in v0.9.3 (PR #398). Concluded work.",
		"PR locator: compose file split landed in PR #405. Reference only.",
	} {
		if reason, vetoed := VetoKeep(content); vetoed {
			t.Errorf("VetoKeep(%q) = %q, want no veto (concluded-work evidence)", content, reason)
		}
	}
}

// TestVetoKeepPatternList pins every entry in the named pattern lists to a
// minimal fixture that carries nothing else, in both directions: a pattern that
// stops matching (a typo, a dropped `(?i)`, a lost word boundary) or disappears
// fails here instead of silently weakening the veto, and an unlisted pattern
// fails too, so the list stays the audited set.
func TestVetoKeepPatternList(t *testing.T) {
	byName := map[string]*regexp.Regexp{}
	for _, p := range append(append([]vetoPattern{}, keepVetoImperatives...), keepVetoOpenMarkers...) {
		byName[p.pattern] = p.re
	}
	// pattern -> the shortest note that carries it and nothing else.
	want := map[string]string{
		"never":         "never run restore on one spindle",
		"do not":        "do not restart mithril-sync by hand",
		"don't":         "don't restart mithril-sync by hand",
		"must":          "the env var must be unset",
		"always":        "always archive the backup set first",
		"required":      "a rebase is required before the push",
		"not yet":       "the delete-blob path is not yet fixed",
		"outstanding":   "outstanding: nobody checked the restore path",
		"still pending": "the reward-import fix is still pending review",
		"still open":    "the safety lesson is still open",
		"still stale":   "still stale: the runbook image tag",
		"unresolved":    "the env-var precedence question is unresolved",
		"todo":          "todo: document the one-spindle restore",
	}

	if len(byName) != len(want) {
		t.Errorf("veto list has %d pattern(s), want %d — an entry was added, removed or renamed without a fixture",
			len(byName), len(want))
	}
	for name, fixture := range want {
		t.Run(name, func(t *testing.T) {
			re, ok := byName[name]
			if !ok {
				t.Fatalf("pattern %q is missing from the veto list", name)
			}
			if !re.MatchString(fixture) {
				t.Errorf("pattern %q (%s) does not match its own fixture %q", name, re, fixture)
			}
			// A pattern the list claims must also fire through the exported
			// gate, which is what both the resolve and reassess passes call.
			if _, vetoed := VetoKeep(fixture); !vetoed {
				t.Errorf("VetoKeep(%q) = false, want a veto from pattern %q", fixture, name)
			}
		})
	}
}

// TestVetoKeepIsCaseInsensitive: memories shout their rules (NEVER, Do NOT,
// STILL STALE), and the whole point of the veto is catching the shouting.
func TestVetoKeepIsCaseInsensitive(t *testing.T) {
	for _, content := range []string{
		"never run restore on one spindle",
		"NEVER RUN RESTORE ON ONE SPINDLE",
		"Never run restore on one spindle",
		"STILL STALE: the runbook image tag",
		"still stale: the runbook image tag",
	} {
		if _, vetoed := VetoKeep(content); !vetoed {
			t.Errorf("VetoKeep(%q) = false, want a veto (patterns are case-insensitive)", content)
		}
	}
}

// TestRunVetoSkipsClassifierCall is the behavioural guard for the pass: a
// vetoed candidate is KEEP with no harness call at all, and it is not written
// into the KEEP cache (the veto is recomputed free every pass, so a cache entry
// would only add a stale row to reason about).
func TestRunVetoSkipsClassifierCall(t *testing.T) {
	rule := memory.Memory{ID: "rule", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle. Fixed in the 0.31 release notes."}
	evidence := memory.Memory{ID: "evidence", Category: "changelog",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	store := &fakeStore{candidates: []memory.Memory{rule, evidence}}
	// The classifier would happily resolve the rule — the veto must stop it.
	cls := &fakeClassifier{drop: map[string]bool{rule.Content: true, evidence.Content: true}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 (only the un-vetoed note is asked about)", cls.calls)
	}
	if res.Vetoed != 1 {
		t.Errorf("res.Vetoed = %d, want 1", res.Vetoed)
	}
	if res.Confirmed != 1 || len(confirmed) != 1 || confirmed[0].ID != "evidence" {
		t.Fatalf("confirmed = %v (confirmed=%d), want only the concluded-work evidence", confirmed, res.Confirmed)
	}
	if len(store.resolved) != 1 || store.resolved[0] != "evidence" {
		t.Errorf("resolved = %v, want [evidence]", store.resolved)
	}
	for id := range store.kept {
		if id == "rule" {
			t.Errorf("a vetoed memory must not be KEEP-cached; cache = %v", store.kept)
		}
	}
}

// TestRunVetoDoesNotShieldDeterministicDemotions documents where the veto
// stops: it guards the LLM classifier, not the two free deterministic signals.
// The older endpoint of a live 'supersedes' link was already adjudicated by
// supersede's own classifier, so a note that reads like a rule there is still
// demoted. Changing this would be a separate, deliberate change.
func TestRunVetoDoesNotShieldDeterministicDemotions(t *testing.T) {
	older := memory.Memory{ID: "older", Category: "gotcha",
		Content: "never use the old reward calculation on import"}
	// The newer note is vetoed too, so the pass makes no classifier call at all
	// and the only thing that can demote "older" is the piggyback.
	newer := memory.Memory{ID: "newer", Category: "gotcha",
		Content: "superseded the old reward calculation; never rely on the old ledgerstate import"}
	store := &fakeStore{
		candidates: []memory.Memory{older, newer},
		links: []memory.Link{{
			SourceID: "newer", TargetID: "older", Relation: "supersedes", Source: "llm",
		}},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, confirmed, err := Run(context.Background(), store, cls, "proj", true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Superseded != 1 || len(confirmed) != 1 || confirmed[0].ID != "older" {
		t.Errorf("superseded=%d confirmed=%v, want the supersedes piggyback to still demote older",
			res.Superseded, confirmed)
	}
	if res.Vetoed != 1 {
		t.Errorf("res.Vetoed = %d, want 1 (the newer note is vetoed before the call)", res.Vetoed)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 (the newer note is vetoed, the older is piggybacked)", cls.calls)
	}
}

// TestVetoKeepReasonNamesThePattern: the returned reason is the pattern text,
// which is what the pass logs, so a wrong resolution can be traced to the rule
// that vetoed it.
func TestVetoKeepReasonNamesThePattern(t *testing.T) {
	reason, vetoed := VetoKeep("STILL STALE: the runbook image tag")
	if !vetoed {
		t.Fatal("STILL STALE must veto")
	}
	if !strings.Contains(strings.ToLower(reason), "stale") {
		t.Errorf("reason = %q, want the matched pattern text", reason)
	}
}
