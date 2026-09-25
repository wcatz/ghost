// internal/resolve/live_test.go
package resolve

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
)

// liveTestSource prefers GHOST_TEST_SOURCE and otherwise detects the calling
// harness to decide WHICH harness to call. It no longer decides WHETHER to
// call one: that is GHOST_LIVE_TESTS=1 alone, so running the suite from a
// session shell no longer starts billable work merely because a harness is
// around (issue #548).
func liveTestSource() string {
	if s := os.Getenv("GHOST_TEST_SOURCE"); s != "" {
		return s
	}
	return ai.DetectSource()
}

// liveResolveCases is the labeled resolve set both live classifier tests score
// against. One table means the single-note and batched paths are held to the
// same accuracy bar. `want` is VerdictResolved when the note is RESOLVED
// evidence (should be dropped from ranked injection) and VerdictKeep when it
// must remain injectable.
//
// The set deliberately pairs each RESOLVED example with a KEEP case that
// resembles it on the surface: a decision record containing the word "RESOLVED"
// (case 6), a current fact describing a concluded migration (case 7), and an
// open task phrased like a changelog (case 8). Those are the cases a
// KEEP-bias model must NOT resolve — a false RESOLVED there buries a live
// memory — and they are the ones this measurement exists to watch.
var liveResolveCases = []struct {
	content string
	want    Verdict
}{
	// RESOLVED: intermediate findings, changelogs, cost estimates, and
	// experiment results for work that has concluded (rubric examples).
	{"Kill experiment found 7.3% cross-session links, so we removed the ranking bonus.", VerdictResolved},
	{"Cost estimate from May: $148/mo projected; actuals have since replaced it.", VerdictResolved},
	{"Postmortem (concluded): deploy failure was a stale hash; mitigated. No open actions.", VerdictResolved},
	{"Changelog: connection leak fixed in v0.9.3 (PR #398). Concluded work.", VerdictResolved},
	{"Draft decision: pin resolve to big-pickle; superseded by the KEEP-bias measurement before shipping.", VerdictResolved},

	// KEEP: terminal conclusions, standing rules, active decisions, and
	// reusable knowledge — even when phrased with conclusions or history.
	{"Graph-expansion RESOLVED NO-GO (2026-07-20): decision record — ranking stays off.", VerdictKeep},
	{"The repository default branch is main; the master rename landed in v0.9.0.", VerdictKeep},
	{"Split resolve classification into batches of 8 notes per harness call with a KEEP-biased rubric.", VerdictKeep},
	{"Always run go vet ./... before committing.", VerdictKeep},
	{"Open task: measure resolve KEEP bias against a labeled set before validating the resolve model pin.", VerdictKeep},
}

// TestResolutionClassifierLive validates the actual prompt against the labeled
// set. It needs a working LLM CLI (claude, opencode, codex, or goose), and it is
// OFF by default because it makes real, billable calls — set GHOST_LIVE_TESTS=1
// to run it, then it skips if no CLI answers. Run it manually to get a KEEP-bias signal on
// the classifier (the one piece of the resolve path with no deterministic
// test). The KEEP side is the dangerous direction: a false RESOLVED drops a
// live memory from ranked injection, so the prompt biases KEEP when uncertain.
func TestResolutionClassifierLive(t *testing.T) {
	if !ai.LiveTestsEnabled() {
		t.Skip("live LLM test makes billable harness calls; set GHOST_LIVE_TESTS=1 to run")
	}
	cli := ai.NewSourceProviderForSource(liveTestSource(), "", "", "", "")
	if !cli.Available() {
		t.Skip("no LLM CLI (claude/opencode/codex/goose) available; skipping live classifier test")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cls := NewResolutionClassifier(cli)
	cls.SetLogger(logger)

	correct, keepFalsePos, keepLabeled := 0, 0, 0
	for _, c := range liveResolveCases {
		got, err := cls.IsResolved(context.Background(), c.content)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		verdict := "ok"
		if got != c.want {
			verdict = "MISS"
		} else {
			correct++
		}
		if c.want == VerdictKeep {
			keepLabeled++
			if got == VerdictResolved {
				keepFalsePos++
			}
		}
		t.Logf("[%s] want=%v got=%v  note=%q", verdict, c.want, got, c.content)
	}
	acc := float64(correct) / float64(len(liveResolveCases))
	kfac := float64(keepFalsePos) / float64(keepLabeled)
	t.Logf("resolve classifier accuracy on labeled set: %d/%d = %.2f; KEEP-side false-RESOLVED %d/%d (%.2f)",
		correct, len(liveResolveCases), acc, keepFalsePos, keepLabeled, kfac)
	if acc < 0.75 {
		t.Errorf("resolution accuracy %.2f below 0.75 — prompt may need work", acc)
	}
	// The dangerous direction is bounded on purpose: one KEEP-sided miss is
	// run-to-run variance (a hard cap signals a dangerously false-RESOLVED
	// prompt and any CHOICE_THRESHOLD-style regression), but two or more live
	// KEEP notes judged RESOLVED means the small model is not resolving with
	// the intended bias — a false RESOLVED above 25% would bury live memories
	// and invalidate pinning resolve to this harness.
	if keepFalsePos > 1 {
		t.Errorf("false-RESOLVED on %d/%d KEEP-labeled notes — KEEP bias not holding (bad for pinning resolve)", keepFalsePos, keepLabeled)
	}
}

// TestResolutionClassifierLiveBatch runs the same labeled set through the
// batched path (chunks of 3), validating the numbered-line prompt and parser
// against a real harness, and holding the batched path to the same KEEP-bias
// bar. Off by default: set GHOST_LIVE_TESTS=1, plus GHOST_TEST_SOURCE=opencode
// to compel a particular harness.
func TestResolutionClassifierLiveBatch(t *testing.T) {
	if !ai.LiveTestsEnabled() {
		t.Skip("live LLM test makes billable harness calls; set GHOST_LIVE_TESTS=1 to run")
	}
	cli := ai.NewSourceProviderForSource(liveTestSource(), "", "", "", "")
	if !cli.Available() {
		t.Skip("no LLM CLI (claude/opencode/codex/goose) available; skipping live batch test")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cls := NewResolutionClassifier(cli)
	cls.SetLogger(logger)
	// batchSize 3 (not the shipped 8) keeps one bad chunk from sinking the
	// whole labeled set; the batched prompt format is unit-covered (fake
	// providers) and the 8-note prompt is exercised by real passes.
	cls.batchSize = 3

	contents := make([]string, len(liveResolveCases))
	for i, c := range liveResolveCases {
		contents[i] = c.content
	}
	got, err := cls.IsResolvedBatch(context.Background(), contents)
	if err != nil {
		t.Fatalf("IsResolvedBatch: %v", err)
	}
	if len(got) != len(liveResolveCases) {
		t.Fatalf("got %d verdicts for %d notes", len(got), len(liveResolveCases))
	}
	correct, keepFalsePos, keepLabeled := 0, 0, 0
	for i, c := range liveResolveCases {
		verdict := "ok"
		if got[i] != c.want {
			verdict = "MISS"
		} else {
			correct++
		}
		if c.want == VerdictKeep {
			keepLabeled++
			if got[i] == VerdictResolved {
				keepFalsePos++
			}
		}
		t.Logf("[%s] want=%v got=%v  note=%q", verdict, c.want, got[i], c.content)
	}
	acc := float64(correct) / float64(len(liveResolveCases))
	kfac := float64(keepFalsePos) / float64(keepLabeled)
	t.Logf("batched resolve accuracy: %d/%d = %.2f in %d call(s); KEEP-side false-RESOLVED %d/%d (%.2f)",
		correct, len(liveResolveCases), acc, cls.Calls(), keepFalsePos, keepLabeled, kfac)
	// The batched path must not have silently fallen back: 10 notes at
	// batchSize 3 is 3 batched calls + 1 single-note tail. More calls means
	// the numbered prompt/parser did not work and accuracy was measured on
	// the fallback path instead.
	if cls.Calls() != 4 {
		t.Errorf("batched path fell back: %d calls for 10 notes at batchSize 3, want 4", cls.Calls())
	}
	if acc < 0.75 {
		t.Errorf("batched resolution accuracy %.2f below 0.75", acc)
	}
	if keepFalsePos > 1 {
		t.Errorf("false-RESOLVED on %d/%d KEEP-labeled notes — KEEP bias not holding (bad for pinning resolve)", keepFalsePos, keepLabeled)
	}
}
