package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// fakeProvider returns a canned response and records the last call it saw.
// resps, when set, is consumed one entry per call and takes precedence over
// resp; errCalls marks 1-based call numbers that return err (nil means every
// call errors when err is set).
type fakeProvider struct {
	resp            string
	resps           []string
	err             error
	errCalls        map[int]bool
	calls           int
	lastSystem      string
	lastUserContent string
}

func (f *fakeProvider) Classify(_ context.Context, systemPrompt, userContent string) (string, error) {
	f.calls++
	f.lastSystem = systemPrompt
	f.lastUserContent = userContent
	if f.err != nil && (f.errCalls == nil || f.errCalls[f.calls]) {
		return "", f.err
	}
	if len(f.resps) > 0 {
		r := f.resps[0]
		f.resps = f.resps[1:]
		return r, nil
	}
	return f.resp, nil
}

func TestIsResolvedParsesResolved(t *testing.T) {
	cases := []struct {
		resp string
		want Verdict
	}{
		{"RESOLVED | closed-by: the runbook it described was replaced", VerdictResolved},
		{"resolved.", VerdictKeep}, // a bare RESOLVED is a KEEP (#640)
		{"KEEP", VerdictKeep},
		{"keep — still a live decision", VerdictKeep},
		{"", VerdictUnknown},                // no explicit verdict → UNKNOWN
		{"I think... KEEP", VerdictUnknown}, // only the first field may decide
		{"unsure, but RESOLVED", VerdictUnknown},
		{"already-resolved", VerdictUnknown},
		{"self-resolved", VerdictUnknown},
		{"previously-resolved", VerdictUnknown},
		// A verdict that appears before any other field still resolves, as
		// long as it names what closed it.
		{"RESOLVED, no doubt | closed-by: superseded by the v0.9 rubric", VerdictResolved},
	}
	for _, c := range cases {
		fp := &fakeProvider{resp: c.resp}
		h := NewResolutionClassifier(fp)
		got, err := h.IsResolved(context.Background(), "some content")
		if err != nil {
			t.Fatalf("IsResolved(%q): %v", c.resp, err)
		}
		if got != c.want {
			t.Errorf("IsResolved(%q) = %v, want %v", c.resp, got, c.want)
		}
	}
}

// TestIsResolvedRequiresClosedByReason is the #640 contract: a RESOLVED verdict
// buries the note from session-start injection, so it must name what made the
// note obsolete. A reply that says RESOLVED without that reason is read as
// KEEP — the safe direction, where a wrongly-KEPT note merely stays visible.
func TestIsResolvedRequiresClosedByReason(t *testing.T) {
	for _, resp := range []string{
		"RESOLVED",
		"RESOLVED, the work is done",
		"RESOLVED |",
		"RESOLVED | closed-by:",
		"RESOLVED | closed-by:   ",
		"RESOLVED | closed by: the feature was dropped",
		"RESOLVED because the thread concluded",
		// A denial phrase is not a reason: the word after a bare mention of the
		// key must never be read as one, or the KEEP-biased contract leaks
		// through the exact shape it was written to close (review finding on
		// PR #643).
		"RESOLVED — no closed-by available",
		"RESOLVED | no closed-by reason",
		"RESOLVED | without closed-by detail",
		// A placeholder names nothing either.
		"RESOLVED | closed-by: none",
		"RESOLVED | closed-by: n/a",
		"RESOLVED | closed-by: TBD",
		"RESOLVED | closed-by: unknown",
		"RESOLVED | closed-by:\nnone",
		// The prompt's own template, echoed back verbatim: a routine failure
		// mode of a format-constrained reply, and it names nothing.
		"RESOLVED | closed-by: <the fact that made note 1 obsolete>",
		"RESOLVED | closed-by: <what made this note obsolete>",
		"RESOLVED | closed-by: <reason>",
		// A value whose FIRST word denies there is one has named nothing
		// either, and a nine-word placeholder list does not catch it. This is
		// the shared grammar internal/supersede's `replaced:` field rides on
		// (#686), so the hole was in both passes and is closed in both.
		"RESOLVED | closed-by: not applicable",
		"RESOLVED | closed-by: no decision was recorded",
		"RESOLVED | closed-by: nothing specific",
		"RESOLVED | closed-by: cannot name it",
		"RESOLVED | closed-by: unclear",
		"RESOLVED | closed-by: irrelevant to this note",
	} {
		fp := &fakeProvider{resp: resp}
		got, err := NewResolutionClassifier(fp).IsResolved(context.Background(), "content")
		if err != nil {
			t.Fatalf("IsResolved(%q): %v", resp, err)
		}
		if got != VerdictKeep {
			t.Errorf("IsResolved(%q) = %v, want KEEP without a non-empty closed-by", resp, got)
		}
	}
}

// TestClassifierKeepsIssueExamplesWithoutClosedBy runs the issue #640 examples
// through the real prompt and parser with a harness that answers a bare
// RESOLVED for every note — the exact failure mode the maintenance benchmark
// measured. Without a reason naming what closed the note, every verdict parses
// as KEEP, so the pass buries nothing. This is the load-bearing half of the
// fix: a veto cannot help a note the harness itself judges wrongly.
func TestClassifierKeepsIssueExamplesWithoutClosedBy(t *testing.T) {
	notes := []string{
		"Fixed (PR #240): the CI job leaked the pull-request token; never echo it in a log line.",
		"Fixed (PR #241): `DINGO_PLUGINS_STORAGE_*_DATA_DIR` silently overrides `--data-dir`.",
		"Fixed (PR #242): the restore path is safe on a single spindle. It is not.",
	}
	fp := &fakeProvider{resp: "1: RESOLVED\n2: RESOLVED\n3: RESOLVED\n"}
	got, err := NewResolutionClassifier(fp).IsResolvedBatch(context.Background(), notes)
	if err != nil {
		t.Fatalf("IsResolvedBatch: %v", err)
	}
	for i, v := range got {
		if v != VerdictKeep {
			t.Errorf("verdict[%d] = %v, want KEEP (no closed-by reason given)", i, v)
		}
	}
	if fp.calls != 1 {
		t.Errorf("provider calls = %d, want 1 batched call", fp.calls)
	}

	// The same notes DO resolve once the harness names what closed them.
	fp = &fakeProvider{resp: "1: RESOLVED | closed-by: a follow-up note documents the replacement rule\n" +
		"2: RESOLVED | closed-by: superseded by the plugin-loader doc\n" +
		"3: RESOLVED | closed-by: the one-spindle rule is documented now\n"}
	got, err = NewResolutionClassifier(fp).IsResolvedBatch(context.Background(), notes)
	if err != nil {
		t.Fatalf("IsResolvedBatch with reasons: %v", err)
	}
	for i, v := range got {
		if v != VerdictResolved {
			t.Errorf("verdict[%d] = %v, want RESOLVED when a reason is given", i, v)
		}
	}
}

func TestIsResolvedBatchMissingLineIsUnknown(t *testing.T) {
	fp := &fakeProvider{resp: "1: RESOLVED | closed-by: the tracking issue closed\n"}
	cls := NewResolutionClassifier(fp)
	got, err := cls.IsResolvedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("IsResolvedBatch: %v", err)
	}
	if len(got) != 2 || got[0] != VerdictResolved || got[1] != VerdictUnknown {
		t.Fatalf("got %v, want [resolved unknown] (missing line is not KEEP)", got)
	}
	// A partial parse must not trigger the fallback.
	if fp.calls != 1 {
		t.Errorf("partial parse must not trigger the fallback: provider calls = %d, want 1", fp.calls)
	}
}

func TestIsResolvedBatchTransportErrorIsFatal(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		cls := NewResolutionClassifier(&fakeProvider{err: errors.New("api down")})
		_, err := cls.IsResolvedBatch(context.Background(), []string{"a", "b"})
		if err == nil {
			t.Fatal("want transport error propagated, got nil")
		}
		if !strings.Contains(err.Error(), "notes 1-2") {
			t.Errorf("transport error must identify the failing chunk, got: %v", err)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		fp := &fakeProvider{
			resps:    []string{"MAYBE"},
			err:      errors.New("api down"),
			errCalls: map[int]bool{2: true},
		}
		cls := NewResolutionClassifier(fp)
		_, err := cls.IsResolvedBatch(context.Background(), []string{"a", "b"})
		if err == nil {
			t.Fatal("want fallback transport error propagated, got nil")
		}
		if !strings.Contains(err.Error(), "note 1") {
			t.Errorf("fallback error must identify the failing note, got: %v", err)
		}
	})
}

func TestIsResolvedBatchChunksBySize(t *testing.T) {
	fp := &fakeProvider{resps: []string{
		"1: KEEP\n2: KEEP",
		"1: KEEP\n2: KEEP",
		"KEEP",
	}}
	cls := NewResolutionClassifier(fp)
	cls.batchSize = 2
	got, err := cls.IsResolvedBatch(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("IsResolvedBatch: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d verdicts, want 5", len(got))
	}
	for i, v := range got {
		if v != VerdictKeep {
			t.Errorf("verdict[%d] = %v, want all KEEP", i, v)
		}
	}
	// 5 notes at batchSize 2 → 2 batched calls + 1 single-note tail.
	if fp.calls != 3 {
		t.Errorf("provider calls = %d, want 3 (2 batches + 1 single tail)", fp.calls)
	}
	if cls.Calls() != 3 {
		t.Errorf("Calls() = %d, want 3", cls.Calls())
	}
}

func TestParseBatchVerdicts(t *testing.T) {
	// Lines map by number, not position.
	got, ok := parseBatchVerdicts("2: RESOLVED | closed-by: closed by PR #240\n1: KEEP\n", 2)
	if !ok || got[0] != VerdictKeep || got[1] != VerdictResolved {
		t.Errorf("out-of-order lines: got %v ok=%v, want [keep resolved] ok=true", got, ok)
	}

	// Missing entries are UNKNOWN; out-of-range numbers are ignored.
	got, ok = parseBatchVerdicts("1: RESOLVED | closed-by: closed by PR #240\n9: RESOLVED | closed-by: closed by PR #241\n", 2)
	if !ok || got[0] != VerdictResolved || got[1] != VerdictUnknown {
		t.Errorf("missing/out-of-range: got %v ok=%v, want [resolved unknown] ok=true", got, ok)
	}

	// A duplicated note number invalidates the whole reply, so an injected or
	// echoed line cannot win by coming first.
	got, ok = parseBatchVerdicts("1: RESOLVED | closed-by: closed by PR #240\n1: KEEP", 1)
	if ok || got[0] != VerdictUnknown {
		t.Errorf("duplicate number must invalidate the reply: got %v ok=%v, want [unknown] ok=false", got, ok)
	}

	// A reply with no recognizable verdict is not a parse.
	got, ok = parseBatchVerdicts("I'm not sure, maybe both?", 1)
	if ok || got[0] != VerdictUnknown {
		t.Errorf("garbage must not parse: got %v ok=%v", got, ok)
	}

	// Prose lines and markdown decoration are tolerated, including a bold
	// number/separator pair and a bulleted line.
	got, ok = parseBatchVerdicts("Here are the verdicts:\n**1:** RESOLVED | closed-by: closed by PR #240", 1)
	if !ok || got[0] != VerdictResolved {
		t.Errorf("decorated line: got %v ok=%v, want [resolved] ok=true", got, ok)
	}
	got, ok = parseBatchVerdicts("- 2: KEEP", 2)
	if !ok || got[0] != VerdictUnknown || got[1] != VerdictKeep {
		t.Errorf("bulleted KEEP: got %v ok=%v, want [unknown keep] ok=true", got, ok)
	}

	// A closed-by reason after the verdict still parses.
	got, ok = parseBatchVerdicts("1: RESOLVED | closed-by: the work concluded", 1)
	if !ok || got[0] != VerdictResolved {
		t.Errorf("verdict with a closed-by reason: got %v ok=%v, want [resolved] ok=true", got, ok)
	}

	// A verdict with only a trailing explanation is a KEEP: no reason, no
	// burial (issue #640).
	got, ok = parseBatchVerdicts("1: RESOLVED because the work concluded", 1)
	if !ok || got[0] != VerdictKeep {
		t.Errorf("reasonless RESOLVED: got %v ok=%v, want [keep] ok=true", got, ok)
	}

	// Garbled lines are UNKNOWN but do not invalidate a reply that recognized
	// at least one line.
	got, ok = parseBatchVerdicts("1: nonsense\n2: RESOLVED | closed-by: closed by PR #240", 2)
	if !ok || got[0] != VerdictUnknown || got[1] != VerdictResolved {
		t.Errorf("partially garbled reply: got %v ok=%v, want [unknown resolved] ok=true", got, ok)
	}
}

// TestFormatBatchContentEvidenceLineStaysOutsideTheBlock: a note with a real
// evidence line gets it on its own line after the block, byte-identical to the
// line the store renders, and the content before it is folded.
func TestFormatBatchContentEvidenceLineStaysOutsideTheBlock(t *testing.T) {
	ev := memory.FlagEvidence{Flagged: 2}.Line()
	if !strings.HasPrefix(ev, "audit: verdicts flagged=2") {
		t.Fatalf("evidence line = %q", ev)
	}
	got := formatBatchContent([]Note{
		{Content: "alpha\nbeta", Evidence: ev},
		{Content: "gamma"},
	})
	want := "1.\nNOTE: «alpha⏎beta»\n" + ev + "\n\n2.\nNOTE: «gamma»"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The evidence line is a line of its own and not inside any block.
	lines := strings.Split(got, "\n")
	if lines[2] != ev {
		t.Errorf("line 3 = %q, want the evidence line", lines[2])
	}
}

func TestVerdictAndLineParsing(t *testing.T) {
	for in, want := range map[string]Verdict{"": VerdictUnknown, "unresolved": VerdictKeep, "not resolved": VerdictKeep, "maybe": VerdictUnknown} {
		if got := parseVerdict(in); got != want {
			t.Errorf("parseVerdict(%q) = %v, want %v", in, got, want)
		}
	}
	for _, line := range []string{"12", "x: y", "1*", "1 x"} {
		if _, _, ok := splitNumberedLine(line); ok {
			t.Errorf("splitNumberedLine(%q) parsed", line)
		}
	}
}

func TestIsResolvedBatchEmpty(t *testing.T) {
	cls := NewResolutionClassifier(&fakeProvider{resp: "KEEP"})
	got, err := cls.IsResolvedBatch(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("empty batch = (%v, %v), want (nil, nil)", got, err)
	}
	if cls.Calls() != 0 {
		t.Errorf("Calls() = %d, want 0 for an empty batch", cls.Calls())
	}
}
