package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/portable"
)

// TestTheImportReportLineIsWrittenRawWhenThereIsNoID is the residual the
// reviewer's trace implied but did not reach, and it is a real gap in the switch
// rather than in the preview: with `r.ID == ""` and a non-empty `detail`, neither
// arm of the final switch fires and `detail` reaches `Fprintf` with no quoting at
// all. It is not reachable from a created memory today, because the import always
// knows the id it stored under — but the switch has no reason to depend on that,
// and a record type added later need not.
func TestTheImportReportLineIsWrittenRawWhenThereIsNoID(t *testing.T) {
	var out strings.Builder
	err := printRecordLine(&out, portable.RecordResult{
		Type: portable.TypeMemory, Line: 3, Action: portable.ActionReject,
		Detail: "legitimate claim\rsomething the operator should not read",
	})
	if err != nil {
		t.Fatalf("printRecordLine: %v", err)
	}
	if got := out.String(); strings.Contains(got, "\r") {
		t.Errorf("a report line with no id printed its detail raw:\n%q", got)
	}
}
