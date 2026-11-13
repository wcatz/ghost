package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// importCredential is a token-shaped value in the GitHub PAT format, assembled
// because push protection matches that format anywhere in a diff.
func importCredential() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

// TestPrintRecordLineNeverEchoesARefusedCredential: the store refuses a
// token-shaped value and `ghost import` must not then print it, in the
// per-record line or the summary (a dry run is how a user inspects an artifact
// they do not trust yet). A record the store accepted keeps its content prefix.
func TestPrintRecordLineNeverEchoesARefusedCredential(t *testing.T) {
	credential := importCredential()
	refusal := &memory.SecretContentError{Field: "content", Format: "GitHub personal access token"}
	results := []portable.RecordResult{
		{Type: portable.TypeMemory, ID: "m1", Line: 3, Action: portable.ActionReject, Detail: credential, Error: refusal},
		{Type: portable.TypeTask, ID: "t1", Line: 4, Action: portable.ActionReject, Detail: "rotate " + credential, Error: refusal},
		{Type: portable.TypeMemory, ID: "m2", Line: 5, Action: portable.ActionCreate, Detail: "an ordinary memory about the relay port"},
	}
	var perRecord strings.Builder
	for _, r := range results {
		if err := printRecordLine(&perRecord, r); err != nil {
			t.Fatalf("printRecordLine: %v", err)
		}
	}
	report := portable.ImportReport{Applied: true, Created: map[string]int{"memory": 1}, Rejected: 2, Errors: []error{refusal, refusal}}
	var summary strings.Builder
	if err := printImportReport(&summary, "/tmp/x.jsonl", report, portable.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("printImportReport: %v", err)
	}
	output := perRecord.String() + summary.String()
	if strings.Contains(output, credential) || strings.Contains(output, "ghp_") {
		t.Errorf("the printed report echoed the refused credential:\n%s", output)
	}
	for _, want := range []string{"m1", "t1", "GitHub personal access token", "an ordinary memory about the relay port"} {
		if !strings.Contains(output, want) {
			t.Errorf("the report is missing %q:\n%s", want, output)
		}
	}
}
