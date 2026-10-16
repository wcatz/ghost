package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var testOracle = []OraclePair{{OlderMark: "v4.18.1", NewerMark: "v5.6.0", Replaced: "v4.18.1"}}

// TestSupersedeVerdictIsTheOracle: a pair the oracle names is SUPERSEDES with
// the retired claim, every other pair is NEITHER, and the pair is read from the
// item's own OLDER/NEWER lines rather than from the rubric's prose before them.
func TestSupersedeVerdictIsTheOracle(t *testing.T) {
	item := "rubric prose mentioning OLDER and NEWER\nOLDER 2026-01-01: «pins v4.18.1»\nNEWER 2026-01-02: «pins v5.6.0»\n"
	if got := supersedeVerdict(testOracle, item); got != "SUPERSEDES | replaced: v4.18.1" {
		t.Errorf("oracle pair = %q", got)
	}
	swapped := "OLDER 2026-01-01: «pins v5.6.0»\nNEWER 2026-01-02: «pins v4.18.1»\n"
	if got := supersedeVerdict(testOracle, swapped); got != "NEITHER" {
		t.Errorf("reversed pair = %q, want NEITHER", got)
	}
	if got := supersedeVerdict(testOracle, "OLDER x: «a»\nNEWER y: «b»\n"); got != "NEITHER" {
		t.Errorf("unrelated pair = %q", got)
	}
}

// TestNumberedAnswersEveryItem: a batched request gets one "N: VERDICT" line per
// numbered item, in order, and a single-item request gets the bare verdict.
func TestNumberedAnswersEveryItem(t *testing.T) {
	batch := "rubric\n1.\nOLDER a: «pins v4.18.1»\nNEWER b: «pins v5.6.0»\n2.\nOLDER a: «x»\nNEWER b: «y»\n"
	got := numbered(batch, func(item string) string { return supersedeVerdict(testOracle, item) })
	want := "1: SUPERSEDES | replaced: v4.18.1\n2: NEITHER"
	if got != want {
		t.Errorf("batched answer\n got %q\nwant %q", got, want)
	}
	if got := numbered("no numbers here", func(string) string { return "KEEP" }); got != "KEEP" {
		t.Errorf("single answer = %q", got)
	}
}

// TestRunSpeaksTheOpencodeContract: --version answers 2.0.15, a resolve request
// is answered KEEP inside one JSONL text event, and a non-opencode name refuses.
func TestRunSpeaksTheOpencodeContract(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"/x/opencode", "--version"}, strings.NewReader(""), &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "2.0.15" {
		t.Fatalf("--version: code %d, out %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"/x/opencode", "run"}, strings.NewReader("closed-by: rubric\n1.\nnote a\n2.\nnote b\n"), &out, &errb); code != 0 {
		t.Fatalf("resolve: code %d, stderr %q", code, errb.String())
	}
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Text string `json:"text"`
		} `json:"part"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &ev); err != nil || ev.Type != "text" || ev.Part.Text != "1: KEEP\n2: KEEP" {
		t.Fatalf("resolve answer %q (err %v)", out.String(), err)
	}
	out.Reset()
	if code := run([]string{"/x/claude", "-p"}, strings.NewReader("anything"), &out, &errb); code == 0 || out.Len() != 0 {
		t.Fatalf("claude name: code %d, stdout %q; want a refusal with no answer", code, out.String())
	}
}
