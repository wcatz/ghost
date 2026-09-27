package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// importCredential is a token-shaped value in the GitHub PAT format, assembled
// rather than written out because GitHub push protection matches that format
// anywhere in a diff and rejects the push (GH013) before review starts.
func importCredential() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

// TestPrintRecordLineNeverEchoesARefusedCredential is the CLI half of the leak
// the store's credential guard would otherwise create: the store refuses the
// value, and then `ghost import` prints it — the per-record line, the rejected
// list, or both. A dry run printed it too, which is the worse half, because a
// dry run is how a user is meant to inspect an artifact they do not trust yet.
//
// This drives the real print functions over a real report, so it covers the whole
// path a reader sees rather than one field in isolation.
func TestPrintRecordLineNeverEchoesARefusedCredential(t *testing.T) {
	credential := importCredential()
	refusal := &memory.SecretContentError{Field: "content", Format: "GitHub personal access token"}

	results := []portable.RecordResult{
		{
			Type: portable.TypeMemory, ID: "m1", Line: 3, Action: portable.ActionReject,
			// A RecordResult that reached the printer with the record's own words
			// in Detail — which is what the unsafe code path produced, and what
			// the second line in printRecordLine exists for.
			Detail: credential, Error: refusal,
		},
		{
			Type: portable.TypeTask, ID: "t1", Line: 4, Action: portable.ActionReject,
			Detail: "rotate " + credential, Error: refusal,
		},
		{
			Type: portable.TypeMemory, ID: "m2", Line: 5, Action: portable.ActionCreate,
			// No error, so nothing substituted the id: the label is the
			// record's own words and this asserts the CLI does not second-guess
			// a record the store accepted.
			Detail: "an ordinary memory about the relay port",
		},
	}

	var perRecord strings.Builder
	for _, r := range results {
		if err := printRecordLine(&perRecord, r); err != nil {
			t.Fatalf("printRecordLine: %v", err)
		}
	}

	report := portable.ImportReport{
		Applied:  true,
		Created:  map[string]int{"memory": 1},
		Rejected: 2,
		Errors: []error{
			refusal,
			refusal,
		},
	}
	var summary strings.Builder
	if err := printImportReport(&summary, "/tmp/x.jsonl", report, portable.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("printImportReport: %v", err)
	}

	output := perRecord.String() + summary.String()
	if strings.Contains(output, credential) || strings.Contains(output, "ghp_") {
		t.Errorf("the printed report echoed the refused credential:\n%s", output)
	}
	// The record still has to be identifiable, and the format still has to be
	// named, or the report is useless exactly when it matters.
	for _, want := range []string{"m1", "t1", "GitHub personal access token"} {
		if !strings.Contains(output, want) {
			t.Errorf("the report is missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(perRecord.String(), "an ordinary memory about the relay port") {
		t.Errorf("an accepted record lost its content prefix:\n%s", perRecord.String())
	}
}
