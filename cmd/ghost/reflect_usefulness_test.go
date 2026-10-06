package main

// #648 slice 1: what `ghost reflect` actually SENDS to a harness.
//
// The internal/reflection tests prove the prompt renders the evidence and that a
// `used` verdict stays invisible. They read the prompt through the same input
// struct a caller fills, so a caller that simply forgot to pass the evidence
// would leave every one of them green while the feature did nothing in the one
// place it runs.
//
// So this drives runReflect against a real store, with a fake `opencode` that
// captures the prompt off its own stdin. Nothing between the store and the
// harness's stdin is stubbed: the evidence is read by the reader this build
// wrote, through the command's own wiring.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// reflectHarness installs a fake `opencode` that appends its stdin prompt to a
// capture file and answers with a reply the caller supplies.
//
// It captures rather than echoes because the prompt is the ONLY observable: what
// the harness decided is not what this slice changed. The reply keeps every id,
// which is the one response shape that cannot turn a capture into a guess about
// which memories were in the input.
func reflectHarness(t *testing.T, capture string, keepIDs []string) {
	t.Helper()
	binDir := t.TempDir()
	// The reply lives in its own file rather than in the script: it is JSON with
	// braces and quotes in it, and inlining it would mean hand-quoting it for a
	// shell, where the escapes a Go %q writes are not the ones sh reads.
	// The consolidator's reply is itself JSON and it travels as the harness event's
	// `text` STRING, so the event is marshalled rather than pasted by hand: pasted,
	// the reply's own quotes would sit unescaped inside a JSON string and the
	// harness would answer a line no parser can read.
	// Every listed id gets a `keep`. An empty ops array is REJECTED by the tier
	// ("carries no operations"), which would make runReflect exit 1 before the
	// capture was ever asserted on — and an answer that omits an id is audited as
	// a near-drop, so the keeps are named rather than left to the pass-through.
	ops := make([]string, 0, len(keepIDs))
	for _, id := range keepIDs {
		ops = append(ops, "keep "+id)
	}
	replyBody, err := json.Marshal(map[string]any{"learned_context": "ctx", "ops": ops})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	replyFile := filepath.Join(binDir, "reply.jsonl")
	event, err := json.Marshal(map[string]any{
		"type": "text",
		"part": map[string]any{"type": "text", "text": string(replyBody)},
	})
	if err != nil {
		t.Fatalf("marshal harness event: %v", err)
	}
	if err := os.WriteFile(replyFile, append(event, '\n'), 0o600); err != nil {
		t.Fatalf("write reply: %v", err)
	}
	harness := filepath.Join(binDir, "opencode")
	// /bin/cat by absolute path, not a bare `cat`: isolatedLifecycleEnv pins PATH
	// at an empty temp dir so no harness is reachable, and a PATH lookup would
	// leave the capture empty rather than failing loudly.
	writeExecutable(t, harness, fmt.Sprintf("#!/bin/sh\n/bin/cat >> %q\n/bin/cat %q\n", capture, replyFile))
	// Both the PATH and the configured override: isolatedLifecycleEnv pins the
	// override at a path that does not exist, and the pass resolves the tier
	// through it before it scans anything.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)
}

// plantVerdicts records one verdict per memory against the given outcomes,
// unattributed to a call (rowid 0) which the writer accepts.
func plantVerdicts(t *testing.T, store *memory.Store, project string, ids []string, outcome, session string) {
	t.Helper()
	rows := make([]memory.RetrievalAuditRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, memory.RetrievalAuditRow{
			ProjectID: project, SessionID: session, Source: "search",
			MemoryID: id, Outcome: outcome,
		})
	}
	refused, err := store.RecordRetrievalAudits(t.Context(), rows)
	if err != nil {
		t.Fatalf("RecordRetrievalAudits(%s): %v", outcome, err)
	}
	if len(refused) != 0 {
		t.Fatalf("the writer refused %d %s verdicts, so the test would prove nothing: %+v",
			len(refused), outcome, refused)
	}
}

// TestRunReflectSendsTheAuditsNegativeEvidenceAndNothingElse is the end-to-end
// statement of the slice: a store holding a `used` verdict on one memory and a
// `contradicted` verdict on another produces a prompt that names the
// contradiction and carries no trace of the `used` verdict.
//
// Both verdicts are planted over BOTH memories, so "the prompt changed" cannot be
// satisfied by any annotation at all and "the prompt carries the count" cannot be
// satisfied by a leaked `used`. The assertion is on what the harness received.
func TestRunReflectSendsTheAuditsNegativeEvidenceAndNothingElse(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	capture := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	const project = "usefulness"
	const contradicted = "the deploy failure was a stale hash, per the postmortem"
	const merelyRead = "the graph bonus is gone as of PR #210"

	store := openReflectStore(t, dataHome, project)
	ids := make([]string, 0, 2)
	for _, content := range []string{contradicted, merelyRead} {
		id, _, _, err := store.UpsertWithOptions(t.Context(), project, "fact", content, "mcp", 0.6, nil,
			memory.UpsertOptions{})
		if err != nil {
			t.Fatalf("seed %q: %v", content, err)
		}
		ids = append(ids, id)
	}
	// Both buckets over both memories: the positive one twice, so a leak cannot
	// be a single stray figure.
	plantVerdicts(t, store, project, ids, memory.VerdictOutcomeUsed, "ses_loud")
	plantVerdicts(t, store, project, ids, memory.VerdictOutcomeUsed, "ses_loud")
	plantVerdicts(t, store, project, ids, memory.VerdictOutcomeIgnored, "ses_loud")
	plantVerdicts(t, store, project, []string{ids[0]}, memory.VerdictOutcomeContradicted, "ses_quiet")
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	reflectHarness(t, capture, ids)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "reflect", project, "--tier", "opencode", "--apply"}
	t.Cleanup(func() { os.Args = origArgs })
	runReflect()

	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read the captured prompt: %v", err)
	}
	got := string(prompt)

	line := memoryRecordLine(t, got, ids[0])
	if !strings.Contains(line, "contradicted=1") {
		t.Errorf("the contradicted memory's line carries no evidence; runReflect did not pass "+
			"what the reader returned:\n%s", line)
	}
	other := memoryRecordLine(t, got, ids[1])
	if strings.Contains(other, "audit") {
		t.Errorf("a memory with only `used` and `ignored` verdicts was annotated; the #284 "+
			"popularity loop is a boost the moment a frequency reaches this prompt:\n%s", other)
	}
	for _, leaked := range []string{"used=", "ignored=", "ses_loud"} {
		if strings.Contains(got, leaked) {
			t.Errorf("the prompt carries %q, which only the positive verdicts were recorded under:\n%s",
				leaked, got)
		}
	}
}

// TestRunReflectOnAnUnauditedProjectSendsTheSamePrompt checks the other half at
// the command layer: the evidence read must not alter the prompt for a project
// with nothing on file. Without it, a reader that errored, or a map that arrived
// populated with zeroes, would look identical to the working case.
func TestRunReflectOnAnUnauditedProjectSendsTheSamePrompt(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	capture := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// Not named "unaudited": the project's own name is printed in the prompt, and
	// the assertion below looks for the word "audit" in it.
	const project = "freshproj"
	const content = "unit tests run with the race detector on linux only"
	store := openReflectStore(t, dataHome, project)
	id, _, _, err := store.UpsertWithOptions(t.Context(), project, "fact", content, "mcp", 0.6, nil,
		memory.UpsertOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A verdict of the only kind this slice filters OUT, so the store is not
	// simply empty and the reader really is asked a question it answers.
	plantVerdicts(t, store, project, []string{id}, memory.VerdictOutcomeUsed, "ses_loud")
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	reflectHarness(t, capture, []string{id})

	origArgs := os.Args
	os.Args = []string{origArgs[0], "reflect", project, "--tier", "opencode", "--apply"}
	t.Cleanup(func() { os.Args = origArgs })
	runReflect()

	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read the captured prompt: %v", err)
	}
	if strings.Contains(string(prompt), "audit") {
		t.Errorf("the prompt mentions the audit for a project with no negative verdicts:\n%s", prompt)
	}
}

// memoryRecordLine returns the single rendered corpus record for one memory id.
func memoryRecordLine(t *testing.T, prompt, id string) string {
	t.Helper()
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "- id:"+id+" ") {
			return l
		}
	}
	t.Fatalf("no corpus record for %s in the prompt:\n%s", id, prompt)
	return ""
}
